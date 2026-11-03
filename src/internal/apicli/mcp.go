package apicli

// MCP-сервер (Model Context Protocol) поверх stdio: нейросеть на этом
// компьютере (Claude Desktop, Claude Code) запускает `ssh_tunnel.exe mcp` и
// получает инструменты — действия API. Наружу ничего не слушает: связь
// только через stdin/stdout запустившей программы. Работает, только пока
// в настройках включена галочка «Управление нейросетью (MCP)»
// (config.MCPEnabled) — проверяется на каждом вызове.
//
// Инструменты: несколько понятных (состояние, кнопки, правила,
// приложения) и по одному на каждый адрес GET /api — так новое в API
// появляется у нейросети само. Опасные (поле confirm в самоописании)
// помечены в описании: сначала спросить человека.

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"sshtunnel/internal/updater"
)

const confirmNote = "ОПАСНО — перед вызовом спроси человека и дождись явного «да»: "

// errMCPOff — галочка выключена.
var errMCPOff = errors.New("управление нейросетью выключено: включи галочку «Управление нейросетью (MCP)» в настройках ssh_tunnel на этом устройстве")

type rpcReq struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResp struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcErr         `json:"error,omitempty"`
}

// Tool — инструмент MCP.
type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	Annotations map[string]any `json:"annotations,omitempty"`

	call func(c *Client, args map[string]any) (string, error)
}

var protocolVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

// ServeMCP читает запросы JSON-RPC построчно из in и пишет ответы в out,
// пока in не закроется.
func ServeMCP(c *Client, in io.Reader, out io.Writer) error {
	m := &mcp{c: c, out: out}
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var req rpcReq
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			m.send(rpcResp{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcErr{-32700, "не разобрал JSON"}})
			continue
		}
		m.handle(req)
	}
	return sc.Err()
}

type mcp struct {
	c   *Client
	out io.Writer
	mu  sync.Mutex
}

func (m *mcp) send(r rpcResp) {
	b, _ := json.Marshal(r)
	m.mu.Lock()
	m.out.Write(append(b, '\n'))
	m.mu.Unlock()
}

func (m *mcp) handle(req rpcReq) {
	if len(req.ID) == 0 {
		return // уведомление (notifications/initialized и т.п.) — ответа не ждут
	}
	res, err := m.dispatch(req)
	r := rpcResp{JSONRPC: "2.0", ID: req.ID}
	if err != nil {
		r.Error = err
	} else {
		r.Result = res
	}
	m.send(r)
}

func (m *mcp) dispatch(req rpcReq) (any, *rpcErr) {
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		v := protocolVersions[0]
		for _, s := range protocolVersions {
			if s == p.ProtocolVersion {
				v = s
			}
		}
		return map[string]any{
			"protocolVersion": v,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "ssh_tunnel", "version": updater.Version},
			"instructions": "ssh_tunnel — обход блокировок через свой SSH-сервер и сеть устройств. " +
				"Две независимые кнопки: туннель («Обход») и сеть устройств. Инструменты с пометкой ОПАСНО " +
				"вызывай только после явного согласия человека.",
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		// Список отдаём и при выключенной галочке: нейросеть видит, что
		// умеет, а вызов ответит, что галочку надо включить (без
		// перезапуска нейросети после включения).
		tools, err := Tools(m.c)
		if err != nil {
			return nil, &rpcErr{-32603, err.Error()}
		}
		return map[string]any{"tools": tools}, nil
	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, &rpcErr{-32602, "нужны name и arguments"}
		}
		text, err := m.call(p.Name, p.Arguments)
		if err != nil {
			return toolResult(err.Error(), true), nil
		}
		return toolResult(text, false), nil
	}
	return nil, &rpcErr{-32601, "нет метода " + req.Method}
}

func toolResult(text string, isErr bool) map[string]any {
	return map[string]any{"content": []map[string]any{{"type": "text", "text": text}}, "isError": isErr}
}

// enabled — включена ли галочка MCP на этом устройстве.
func (m *mcp) enabled() error {
	var cfg struct {
		MCPEnabled bool `json:"mcpEnabled"`
	}
	if err := m.c.JSON("GET", "/api/config", nil, &cfg); err != nil {
		return err
	}
	if !cfg.MCPEnabled {
		return errMCPOff
	}
	return nil
}

func (m *mcp) call(name string, args map[string]any) (string, error) {
	if err := m.enabled(); err != nil {
		return "", err
	}
	tools, err := Tools(m.c)
	if err != nil {
		return "", err
	}
	for _, t := range tools {
		if t.Name == name {
			if args == nil {
				args = map[string]any{}
			}
			return t.call(m.c, args)
		}
	}
	return "", fmt.Errorf("нет инструмента %s", name)
}

func schema(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func prop(typ, desc string, enum ...string) map[string]any {
	p := map[string]any{"type": typ, "description": desc}
	if len(enum) > 0 {
		p["enum"] = enum
	}
	return p
}

// pretty — ответ API для нейросети: JSON с отступами.
func pretty(b []byte) string {
	var v any
	if json.Unmarshal(b, &v) != nil {
		return string(b)
	}
	j, _ := json.MarshalIndent(v, "", " ")
	return string(j)
}

func doJSON(c *Client, method, path string, body any) (string, error) {
	b, ct, err := c.Do(method, path, nil, body)
	if err != nil {
		return "", err
	}
	if !strings.Contains(ct, "json") && !strings.HasPrefix(ct, "text/") {
		return fmt.Sprintf("(двоичные данные, %d байт, %s)", len(b), ct), nil
	}
	if e := apiError(b); e != "" {
		return "", errors.New(e)
	}
	return pretty(b), nil
}

func button(which string) func(c *Client, a map[string]any) (string, error) {
	return func(c *Client, a map[string]any) (string, error) {
		on, ok := a["on"].(bool)
		if !ok {
			return "", errors.New("нужно on: true или false")
		}
		b, _, err := c.Do("POST", "/api/state", nil, map[string]bool{which: on})
		if err != nil {
			return "", err
		}
		if e := apiError(b); e != "" {
			return "", errors.New(e)
		}
		st, err := WaitSettled(c, which, 60*time.Second)
		if err != nil {
			return "", err
		}
		return pretty(st), nil
	}
}

// friendlyTools — понятные инструменты для частых дел.
func friendlyTools() []Tool {
	str := func(a map[string]any, k string) string { s, _ := a[k].(string); return s }
	return []Tool{
		{Name: "get_state", Description: "Что сейчас: туннель (tunnel — круглая кнопка «Обход»), сеть устройств (mesh), режим (off | mesh | bypass | bypass+mesh), связь с сервером.",
			InputSchema: schema(map[string]any{}), Annotations: map[string]any{"readOnlyHint": true},
			call: func(c *Client, _ map[string]any) (string, error) { return doJSON(c, "GET", "/api/state", nil) }},
		{Name: "set_tunnel", Description: "Включить или выключить туннель («Обход» блокировок через сервер). Сеть устройств не трогается. Ждёт результата подключения (до минуты).",
			InputSchema: schema(map[string]any{"on": prop("boolean", "true — включить, false — выключить")}, "on"),
			call:        button("tunnel")},
		{Name: "set_mesh", Description: "Включить или выключить сеть устройств. " + confirmNote + "выключение обрывает связь с другими устройствами (включение — без спроса).",
			InputSchema: schema(map[string]any{"on": prop("boolean", "true — включить, false — выключить")}, "on"),
			Annotations: map[string]any{"destructiveHint": true},
			call:        button("mesh")},
		{Name: "get_rules", Description: "Правила активного сервера: filterMode (all — все программы через туннель, only — только filterApps, except — все кроме filterApps), directHosts — всегда напрямую, tunnelHosts — всегда через сервер, fakeIP — подставные адреса в DNS режима VPN, realIPHosts — имена, которым подставной адрес не выдаётся никогда.",
			InputSchema: schema(map[string]any{}), Annotations: map[string]any{"readOnlyHint": true},
			call: func(c *Client, _ map[string]any) (string, error) { return doJSON(c, "GET", "/api/rules", nil) }},
		{Name: "add_rule", Description: "Добавить правило: list=direct — адрес всегда напрямую, tunnel — всегда через сервер, apps — программа в список фильтра, realip — имя всегда с настоящим адресом в DNS (для программ, которые проверяют адрес). Применяется сразу.",
			InputSchema: schema(map[string]any{
				"list":  prop("string", "какой список", "direct", "tunnel", "apps", "realip"),
				"value": prop("string", "домен (youtube.com), адрес, сеть (10.0.0.0/8) или программа (steam.exe)"),
			}, "list", "value"),
			call: func(c *Client, a map[string]any) (string, error) {
				return doJSON(c, "POST", "/api/rules", map[string]string{"op": "add", "list": str(a, "list"), "value": str(a, "value")})
			}},
		{Name: "remove_rule", Description: "Убрать правило из списка direct, tunnel, apps или realip.",
			InputSchema: schema(map[string]any{
				"list":  prop("string", "какой список", "direct", "tunnel", "apps", "realip"),
				"value": prop("string", "что убрать"),
			}, "list", "value"),
			call: func(c *Client, a map[string]any) (string, error) {
				return doJSON(c, "POST", "/api/rules", map[string]string{"op": "remove", "list": str(a, "list"), "value": str(a, "value")})
			}},
		{Name: "list_apps", Description: "Плагины: каталог (catalog) и добавленные на этом устройстве (added: star — в быстром доступе, on — фоновое включено).",
			InputSchema: schema(map[string]any{}), Annotations: map[string]any{"readOnlyHint": true},
			call: func(c *Client, _ map[string]any) (string, error) { return doJSON(c, "GET", "/api/addons", nil) }},
		{Name: "app_action", Description: "Действие с плагином: add — добавить из каталога, star/unstar — быстрый доступ, on/off — выключатель фонового. remove — убрать: " + confirmNote + "своё приложение удаляется вместе с данными.",
			InputSchema: schema(map[string]any{
				"op": prop("string", "действие", "add", "remove", "star", "unstar", "on", "off"),
				"id": prop("string", "id приложения из list_apps"),
			}, "op", "id"),
			Annotations: map[string]any{"destructiveHint": true},
			call: func(c *Client, a map[string]any) (string, error) {
				op, val := str(a, "op"), true
				switch op {
				case "unstar":
					op, val = "star", false
				case "off":
					op, val = "on", false
				}
				return doJSON(c, "POST", "/api/addons", map[string]any{"op": op, "id": str(a, "id"), "value": val})
			}},
	}
}

var nonName = regexp.MustCompile(`[^a-z0-9]+`)

// Tools — все инструменты: понятные и по одному на адрес API.
func Tools(c *Client) ([]Tool, error) {
	routes, err := c.Routes()
	if err != nil {
		return nil, err
	}
	tools := friendlyTools()
	used := map[string]bool{}
	for _, t := range tools {
		used[t.Name] = true
	}
	for _, rt := range routes {
		if streaming(rt.Path) || rt.Path == "/api" {
			continue
		}
		rt := rt
		name := "api_" + strings.Trim(nonName.ReplaceAllString(strings.ToLower(strings.TrimPrefix(rt.Path, "/api")), "_"), "_")
		if strings.HasSuffix(rt.Path, "/") {
			name += "_action" // /api/appprofiles/ рядом с /api/appprofiles
		}
		if len(name) > 64 {
			name = name[:64]
		}
		if used[name] {
			continue
		}
		used[name] = true
		var methods []string
		for _, m := range strings.Split(rt.Methods, ",") {
			methods = append(methods, strings.TrimSpace(m))
		}
		desc := rt.Path + " (" + rt.Methods + "). " + rt.Desc
		ann := map[string]any{}
		if rt.Confirm != "" {
			desc = confirmNote + rt.Confirm + ". " + desc
			ann["destructiveHint"] = true
		}
		if len(methods) == 1 && methods[0] == "GET" {
			ann["readOnlyHint"] = true
		}
		t := Tool{Name: name, Description: desc, Annotations: ann,
			InputSchema: schema(map[string]any{
				"method": prop("string", "HTTP-метод (по умолчанию первый из списка)", methods...),
				"body":   map[string]any{"type": "object", "description": "тело JSON для POST"},
				"query":  map[string]any{"type": "object", "description": "параметры адреса (?ключ=значение)"},
				"suffix": prop("string", "продолжение пути для адресов, кончающихся на / (например <id>/apply)"),
			}),
		}
		t.call = func(c *Client, a map[string]any) (string, error) {
			method, _ := a["method"].(string)
			if method == "" {
				method = methods[0]
				if _, ok := a["body"]; ok && rt.Has("POST") {
					method = "POST"
				}
			}
			if !rt.Has(method) {
				return "", fmt.Errorf("%s не умеет %s", rt.Path, method)
			}
			path := rt.Path
			if s, _ := a["suffix"].(string); s != "" && strings.HasSuffix(path, "/") {
				path += strings.TrimLeft(s, "/")
			}
			if q, ok := a["query"].(map[string]any); ok && len(q) > 0 {
				v := url.Values{}
				for k, x := range q {
					v.Set(k, fmt.Sprint(x))
				}
				path += "?" + v.Encode()
			}
			var body any
			if method == "POST" {
				body = a["body"]
				if body == nil {
					body = map[string]any{}
				}
			}
			return doJSON(c, method, path, body)
		}
		tools = append(tools, t)
	}
	return tools, nil
}
