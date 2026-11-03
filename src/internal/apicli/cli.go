package apicli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// IsCommand — похожи ли аргументы на команду (а не на флаги вроде
// -nowindow или -host): первое слово без минуса.
func IsCommand(args []string) bool {
	return len(args) > 0 && args[0] != "" && !strings.HasPrefix(args[0], "-")
}

// Коды выхода.
const (
	exitOK       = 0
	exitError    = 1
	exitUsage    = 2
	exitNoServer = 3
)

// Main выполняет команду и возвращает код выхода. name — как зовут
// программу в подсказке (ssh_tunnel.exe, ssh_tunnel_linux).
func Main(name string, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	asJSON, yes := false, false
	var rest []string
	for _, a := range args {
		switch a {
		case "--json", "-json":
			asJSON = true
		case "--yes", "-yes", "-y":
			yes = true
		default:
			rest = append(rest, a)
		}
	}
	if len(rest) == 0 || rest[0] == "help" || rest[0] == "--help" || rest[0] == "-h" || rest[0] == "-help" {
		return help(name, stdout)
	}
	c, err := Discover()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitNoServer
	}
	if rest[0] == "mcp" {
		if err := ServeMCP(c, stdin, stdout); err != nil {
			fmt.Fprintln(stderr, err)
			return exitError
		}
		return exitOK
	}
	r := &runner{c: c, out: stdout, json: asJSON, yes: yes}
	code, err := r.run(rest)
	if err != nil {
		fmt.Fprintln(stderr, "Ошибка:", err)
		if code == exitOK {
			code = exitError
		}
	}
	return code
}

type runner struct {
	c    *Client
	out  io.Writer
	json bool
	yes  bool
}

func (r *runner) run(a []string) (int, error) {
	switch a[0] {
	case "state", "status":
		return r.state()
	case "tunnel", "mesh":
		if len(a) < 2 {
			return exitUsage, fmt.Errorf("%s on | off", a[0])
		}
		on, err := onOff(a[1])
		if err != nil {
			return exitUsage, err
		}
		return r.setButton(a[0], on)
	case "rules":
		return r.rules(a[1:])
	case "apps":
		return r.apps(a[1:])
	case "check":
		if len(a) >= 3 && a[1] == "site" {
			return r.call("POST", "/api/diag/site", map[string]any{"site": a[2]})
		}
		return r.call("POST", "/api/selfcheck", nil)
	case "api":
		if len(a) < 3 {
			return exitUsage, errors.New("api GET|POST /api/путь [JSON]")
		}
		var body any
		if len(a) > 3 {
			body = json.RawMessage(strings.Join(a[3:], " "))
		}
		return r.call(strings.ToUpper(a[1]), a[2], body)
	}
	return r.generic(a)
}

func onOff(s string) (bool, error) {
	switch strings.ToLower(s) {
	case "on", "1", "true", "вкл":
		return true, nil
	case "off", "0", "false", "выкл":
		return false, nil
	}
	return false, fmt.Errorf("нужно on или off, а не %q", s)
}

// coreState — то, что отдаёт GET /api/state.
type coreState struct {
	Tunnel struct {
		Want  bool   `json:"want"`
		Phase string `json:"phase"`
		Error string `json:"error"`
	} `json:"tunnel"`
	Mesh struct {
		Want  bool   `json:"want"`
		Phase string `json:"phase"`
		Error string `json:"error"`
	} `json:"mesh"`
	MeshAvailable bool   `json:"meshAvailable"`
	Mode          string `json:"mode"`
	Link          string `json:"link"`
	Detail        string `json:"detail"`
}

var phaseText = map[string]string{"off": "выключен", "connecting": "подключаюсь…", "on": "включён", "error": "ошибка"}
var modeText = map[string]string{"off": "Выключено", "mesh": "Только сеть устройств", "bypass": "Обход", "bypass+mesh": "Обход + сеть устройств"}

func (r *runner) printState(raw []byte) error {
	if r.json {
		return r.printJSON(raw)
	}
	var st coreState
	if err := json.Unmarshal(raw, &st); err != nil {
		return err
	}
	line := func(name, phase, e string) {
		t := phaseText[phase]
		if t == "" {
			t = phase
		}
		if e != "" {
			t += ": " + e
		}
		fmt.Fprintf(r.out, "%-16s %s\n", name, t)
	}
	line("Туннель:", st.Tunnel.Phase, st.Tunnel.Error)
	if st.MeshAvailable {
		line("Сеть устройств:", st.Mesh.Phase, st.Mesh.Error)
	}
	m := modeText[st.Mode]
	if m == "" {
		m = st.Mode
	}
	fmt.Fprintf(r.out, "%-16s %s\n", "Режим:", m)
	if st.Link != "" {
		l := st.Link
		if st.Detail != "" {
			l += " (" + st.Detail + ")"
		}
		fmt.Fprintf(r.out, "%-16s %s\n", "Связь:", l)
	}
	return nil
}

func (r *runner) state() (int, error) {
	b, _, err := r.c.Do("GET", "/api/state", nil, nil)
	if err != nil {
		return exitError, err
	}
	return exitOK, r.printState(b)
}

// setButton — «хочу» для одной кнопки; ждёт, пока кончится «подключаюсь»
// (до минуты), и печатает итог. Вторая кнопка не трогается.
func (r *runner) setButton(which string, on bool) (int, error) {
	b, _, err := r.c.Do("POST", "/api/state", nil, map[string]bool{which: on})
	if err != nil {
		return exitError, err
	}
	if e := apiError(b); e != "" {
		return exitError, errors.New(e)
	}
	st, err := WaitSettled(r.c, which, 60*time.Second)
	if err != nil {
		return exitError, err
	}
	if err := r.printState(st); err != nil {
		return exitError, err
	}
	var cs coreState
	_ = json.Unmarshal(st, &cs)
	if (which == "tunnel" && cs.Tunnel.Phase == "error") || (which == "mesh" && cs.Mesh.Phase == "error") {
		return exitError, nil
	}
	return exitOK, nil
}

// WaitSettled ждёт, пока кнопка which выйдет из «подключаюсь», и отдаёт
// состояние (GET /api/state).
func WaitSettled(c *Client, which string, max time.Duration) ([]byte, error) {
	deadline := time.Now().Add(max)
	for {
		b, _, err := c.Do("GET", "/api/state", nil, nil)
		if err != nil {
			return nil, err
		}
		var st coreState
		if err := json.Unmarshal(b, &st); err != nil {
			return nil, err
		}
		phase := st.Tunnel.Phase
		if which == "mesh" {
			phase = st.Mesh.Phase
		}
		if phase != "connecting" || time.Now().After(deadline) {
			return b, nil
		}
		time.Sleep(300 * time.Millisecond)
	}
}

func (r *runner) rules(a []string) (int, error) {
	if len(a) == 0 {
		return r.call("GET", "/api/rules", nil)
	}
	lists := map[string]string{"direct": "direct", "tunnel": "tunnel", "app": "apps", "apps": "apps", "realip": "realip"}
	switch a[0] {
	case "add", "remove":
		if len(a) < 3 || lists[a[1]] == "" {
			return exitUsage, fmt.Errorf("rules %s direct|tunnel|app|realip <значение>", a[0])
		}
		return r.call("POST", "/api/rules", map[string]string{"op": a[0], "list": lists[a[1]], "value": strings.Join(a[2:], " ")})
	case "mode":
		if len(a) < 2 {
			return exitUsage, errors.New("rules mode all|only|except")
		}
		return r.call("POST", "/api/rules", map[string]string{"filterMode": a[1]})
	case "fakeip":
		if len(a) < 2 || (a[1] != "on" && a[1] != "off") {
			return exitUsage, errors.New("rules fakeip on|off")
		}
		return r.call("POST", "/api/rules", map[string]bool{"fakeIP": a[1] == "on"})
	}
	return exitUsage, errors.New("rules | rules add|remove direct|tunnel|app|realip <значение> | rules mode all|only|except | rules fakeip on|off")
}

func (r *runner) apps(a []string) (int, error) {
	if len(a) == 0 || a[0] == "list" {
		return r.appList()
	}
	if a[0] == "install" {
		if len(a) < 2 {
			return exitUsage, errors.New("apps install <архив.zip> [--yes]")
		}
		return r.install(a[1])
	}
	if len(a) < 2 {
		return exitUsage, errors.New("apps add|remove|star|unstar|on|off <id>")
	}
	ops := map[string][2]any{"add": {"add", false}, "remove": {"remove", false}, "star": {"star", true},
		"unstar": {"star", false}, "on": {"on", true}, "off": {"on", false}}
	o, ok := ops[a[0]]
	if !ok {
		return exitUsage, fmt.Errorf("нет действия %q: add, remove, star, unstar, on, off, install", a[0])
	}
	var st map[string]any
	if err := r.c.JSON("POST", "/api/addons", map[string]any{"op": o[0], "id": a[1], "value": o[1]}, &st); err != nil {
		return exitError, err
	}
	return r.appList()
}

func (r *runner) appList() (int, error) {
	b, _, err := r.c.Do("GET", "/api/addons", url.Values{"lang": {"ru"}}, nil)
	if err != nil {
		return exitError, err
	}
	if r.json {
		return exitOK, r.printJSON(b)
	}
	var st struct {
		Catalog []struct {
			ID          string            `json:"id"`
			Name        map[string]string `json:"name"`
			Background  bool              `json:"background"`
			Custom      bool              `json:"custom"`
			Unavailable string            `json:"unavailable"`
		} `json:"catalog"`
		Added []struct {
			ID     string `json:"id"`
			Star   bool   `json:"star"`
			On     bool   `json:"on"`
			Active bool   `json:"active"`
		} `json:"added"`
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return exitError, err
	}
	added := map[string]int{}
	for i, e := range st.Added {
		added[e.ID] = i
	}
	fmt.Fprintln(r.out, "Добавлено:")
	if len(st.Added) == 0 {
		fmt.Fprintln(r.out, "  (пусто)")
	}
	for _, m := range st.Catalog {
		i, ok := added[m.ID]
		if !ok {
			continue
		}
		e := st.Added[i]
		var marks []string
		if e.Star {
			marks = append(marks, "★")
		}
		if m.Background {
			marks = append(marks, map[bool]string{true: "вкл", false: "выкл"}[e.On])
		}
		if m.Custom {
			marks = append(marks, "не из каталога")
		}
		if m.Unavailable != "" {
			marks = append(marks, m.Unavailable)
		}
		fmt.Fprintf(r.out, "  %-12s %s  %s\n", m.ID, m.Name["ru"], strings.Join(marks, ", "))
	}
	fmt.Fprintln(r.out, "Можно добавить:")
	for _, m := range st.Catalog {
		if _, ok := added[m.ID]; !ok {
			fmt.Fprintf(r.out, "  %-12s %s\n", m.ID, m.Name["ru"])
		}
	}
	return exitOK, nil
}

// install — своё приложение из файла: показать разрешения, спросить, поставить.
func (r *runner) install(file string) (int, error) {
	zip, err := os.ReadFile(file)
	if err != nil {
		return exitError, err
	}
	var dry struct {
		ID          string            `json:"id"`
		Name        map[string]string `json:"name"`
		Update      bool              `json:"update"`
		Permissions []struct {
			Text string `json:"text"`
		} `json:"permissions"`
	}
	if err := r.c.JSON("POST", "/api/addons/file?dry=1", zip, &dry); err != nil {
		return exitError, err
	}
	fmt.Fprintf(r.out, "Своё приложение «%s» (%s), не из каталога.\n", dry.Name["ru"], dry.ID)
	if len(dry.Permissions) == 0 {
		fmt.Fprintln(r.out, "Ничего не просит — только свою страницу.")
	} else {
		fmt.Fprintln(r.out, "Просит:")
		for _, p := range dry.Permissions {
			fmt.Fprintln(r.out, "  -", p.Text)
		}
	}
	if !r.yes {
		fmt.Fprintln(r.out, "Чтобы установить с этими разрешениями, повтори команду с --yes.")
		return exitUsage, nil
	}
	if err := r.c.JSON("POST", "/api/addons/file", zip, nil); err != nil {
		return exitError, err
	}
	fmt.Fprintln(r.out, "Установлено.")
	return exitOK, nil
}

// call — запрос и вывод ответа: человеку — текстом, с --json — как есть.
func (r *runner) call(method, path string, body any) (int, error) {
	b, ct, err := r.c.Do(method, path, nil, body)
	if err != nil {
		return exitError, err
	}
	if !strings.Contains(ct, "json") {
		r.out.Write(b)
		return exitOK, nil
	}
	if r.json {
		if err := r.printJSON(b); err != nil {
			return exitError, err
		}
	} else {
		var v any
		if err := json.Unmarshal(b, &v); err != nil {
			r.out.Write(b)
			return exitOK, nil
		}
		printText(r.out, v, "")
	}
	if e := apiError(b); e != "" {
		return exitError, nil
	}
	return exitOK, nil
}

func (r *runner) printJSON(b []byte) error {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		r.out.Write(b)
		return nil
	}
	j, _ := json.MarshalIndent(v, "", "  ")
	fmt.Fprintln(r.out, string(j))
	return nil
}

// generic — команда по самоописанию GET /api: слова — путь адреса
// (`diag site` → /api/diag/site), дальше — поля запроса: `ключ=значение`
// или один JSON. Есть поля и адрес умеет POST — POST, иначе GET (поля — в
// адрес).
func (r *runner) generic(a []string) (int, error) {
	routes, err := r.c.Routes()
	if err != nil {
		return exitError, err
	}
	var rt *Route
	n := 0
	for i := len(a); i > 0 && rt == nil; i-- {
		p := "/api/" + strings.Join(a[:i], "/")
		for j := range routes {
			if strings.TrimSuffix(routes[j].Path, "/") == p {
				rt, n = &routes[j], i
				break
			}
		}
	}
	if rt == nil {
		return exitUsage, fmt.Errorf("нет такой команды %q — список: help", strings.Join(a, " "))
	}
	if streaming(rt.Path) {
		return exitUsage, fmt.Errorf("%s — поток событий, команде его не дождаться", rt.Path)
	}
	args := a[n:]
	var body any
	fields := map[string]any{}
	if len(args) == 1 && strings.HasPrefix(strings.TrimSpace(args[0]), "{") {
		body = json.RawMessage(args[0])
	} else {
		for _, kv := range args {
			k, v, ok := strings.Cut(kv, "=")
			if !ok {
				return exitUsage, fmt.Errorf("поле %q: нужно ключ=значение или один JSON", kv)
			}
			fields[k] = parseValue(v)
		}
	}
	method := "GET"
	if (body != nil || len(fields) > 0 || !rt.Has("GET")) && rt.Has("POST") {
		method = "POST"
		if body == nil {
			body = fields
		}
	}
	path := rt.Path
	if method == "GET" && len(fields) > 0 {
		q := url.Values{}
		for k, v := range fields {
			q.Set(k, fmt.Sprint(v))
		}
		path += "?" + q.Encode()
	}
	return r.call(method, path, body)
}

// parseValue — значение поля: true/false, число, JSON или строка.
func parseValue(v string) any {
	switch v {
	case "true":
		return true
	case "false":
		return false
	}
	if n, err := strconv.ParseFloat(v, 64); err == nil {
		return n
	}
	if strings.HasPrefix(v, "{") || strings.HasPrefix(v, "[") {
		var j any
		if json.Unmarshal([]byte(v), &j) == nil {
			return j
		}
	}
	return v
}

// printText — ответ человеку: вложенные поля с отступом.
func printText(w io.Writer, v any, indent string) {
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			switch y := x[k].(type) {
			case map[string]any, []any:
				if isEmpty(y) {
					fmt.Fprintf(w, "%s%s: —\n", indent, k)
					continue
				}
				fmt.Fprintf(w, "%s%s:\n", indent, k)
				printText(w, y, indent+"  ")
			default:
				fmt.Fprintf(w, "%s%s: %s\n", indent, k, scalar(y))
			}
		}
	case []any:
		for _, e := range x {
			switch y := e.(type) {
			case map[string]any:
				fmt.Fprintf(w, "%s-\n", indent)
				printText(w, y, indent+"  ")
			default:
				fmt.Fprintf(w, "%s- %s\n", indent, scalar(y))
			}
		}
	default:
		fmt.Fprintf(w, "%s%s\n", indent, scalar(x))
	}
}

func isEmpty(v any) bool {
	switch x := v.(type) {
	case map[string]any:
		return len(x) == 0
	case []any:
		return len(x) == 0
	}
	return false
}

func scalar(v any) string {
	switch x := v.(type) {
	case nil:
		return "—"
	case bool:
		if x {
			return "да"
		}
		return "нет"
	case string:
		return x
	case []any, map[string]any:
		j, _ := json.Marshal(x)
		return string(j)
	}
	return fmt.Sprint(v)
}

func help(name string, w io.Writer) int {
	fmt.Fprintf(w, `Управление работающей программой ssh_tunnel из командной строки.

  %[1]s state                      что сейчас: туннель, сеть устройств, режим
  %[1]s tunnel on|off              круглая кнопка «Обход» (сеть устройств не трогается)
  %[1]s mesh on|off                кнопка «Сеть устройств»
  %[1]s rules                      правила активного сервера
  %[1]s rules add direct youtube.com    всегда напрямую (tunnel — всегда через сервер,
                                   app — программа в списке фильтра)
  %[1]s rules remove direct youtube.com
  %[1]s rules mode all|only|except какие программы через туннель
  %[1]s rules add realip .deepseek.com  всегда настоящий адрес в DNS режима VPN
  %[1]s rules fakeip on|off        подставные адреса (fake-IP) в DNS режима VPN
  %[1]s apps                       дополнительные приложения
  %[1]s apps add|remove|star|unstar|on|off <id>
  %[1]s apps install app.zip [--yes]   своё приложение из файла
  %[1]s check                      самопроверка цепочки до сервера
  %[1]s check site youtube.com     открывается ли сайт (нужно приложение «Проверить сайт»)
  %[1]s mcp                        MCP-сервер для нейросети (stdio; нужна галочка в настройках)
  %[1]s api GET|POST /api/… [JSON] любой адрес API как есть

Любой адрес API — тоже команда: слова пути и поля ключ=значение, например
  %[1]s diag site site=youtube.com      → POST /api/diag/site {"site":"youtube.com"}
  %[1]s traffic period=week             → GET /api/traffic?period=week
Список всех адресов: %[1]s api GET /api (или GET /api в docs/API.md).

--json — ответ как есть (для скриптов). Коды выхода: 0 — получилось, 1 — ошибка,
2 — не так вызвано, 3 — программа не запущена.
`, name)
	// Программа запущена — все её адреса как команды: новое в API видно
	// здесь без правки этого файла.
	c, err := Discover()
	if err != nil {
		return exitOK
	}
	routes, err := c.Routes()
	if err != nil {
		return exitOK
	}
	fmt.Fprintln(w, "\nВсе команды этой версии (по GET /api):")
	for _, rt := range routes {
		if streaming(rt.Path) || rt.Path == "/api" {
			continue
		}
		cmd := strings.ReplaceAll(strings.Trim(strings.TrimPrefix(rt.Path, "/api"), "/"), "/", " ")
		fmt.Fprintf(w, "  %-22s %s\n", cmd, rt.Desc)
	}
	return exitOK
}
