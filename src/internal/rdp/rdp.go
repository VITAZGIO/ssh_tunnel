// Package rdp — плагин «Удалённый стол» (RDP): рабочий стол Windows-машины
// в окне программы, с ПК и с телефона, без проброса портов.
//
// Сам RDP разбирает guacd (Apache Guacamole) — «ретранслятор RDP», который
// стоит в локальной сети рядом с машинами (у владельца — на Ubuntu, :4822).
// Программа доходит до guacd через сеть устройств или локалку
// (devnet.Dial), делает рукопожатие Guacamole и дальше просто передаёт
// инструкции между guacd и страницей (addons/rdp.html, guacamole-common-js).
// Канал к странице — длинный опрос по HTTP (open/read/write/close): без
// WebSocket, одинаково через webui на ПК и uihost на телефоне.
package rdp

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"sshtunnel/internal/addons"
	"sshtunnel/internal/devnet"
)

// ID — плагин в каталоге.
const ID = "rdp"

func init() {
	addons.Register(ID, func() addons.Module { return module{} })
	addons.RegisterRoutes(ID,
		addons.Route{Path: "/api/rdp", Methods: "GET, POST",
			Desc: "Удалённый стол: GET — ретранслятор и машины; POST {\"op\":\"relay|add|remove|check\",…} — задать ретранслятор (host:port guacd), добавить/убрать машину {name,host,port,user,pass,domain}, проверить ретранслятор.",
			H:    handleConf},
		addons.Route{Path: "/api/rdp/open", Methods: "POST", Desc: "Открыть сеанс: {id машины, w, h, dpi, quality, user?, pass?} → {sid}.", H: handleOpen},
		addons.Route{Path: "/api/rdp/read", Methods: "GET", Desc: "Инструкции Guacamole сеанса ?sid (длинный опрос, до 20 с).", H: handleRead},
		addons.Route{Path: "/api/rdp/write", Methods: "POST", Desc: "Инструкции Guacamole в сеанс ?sid.", H: handleWrite},
		addons.Route{Path: "/api/rdp/close", Methods: "POST", Desc: "Закрыть сеанс ?sid.", H: handleClose},
	)
}

type module struct{}

func (module) Start() error { return nil }
func (module) Stop()        { closeAll() }

// Host — машина, к которой подключаться.
type Host struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Host   string `json:"host"` // адрес машины так, как его видит guacd (локалка)
	Port   int    `json:"port,omitempty"`
	User   string `json:"user,omitempty"`
	Pass   string `json:"pass,omitempty"` // только если попросили запомнить; файл 0600
	Domain string `json:"domain,omitempty"`
}

type conf struct {
	Relay string `json:"relay"` // guacd: «ubuntu.mesh:4822» или «192.168.1.201:4822»
	Hosts []Host `json:"hosts"`
}

var confMu sync.Mutex

func confPath() string { return filepath.Join(addons.DataDir(), "rdp.json") }

func loadConf() conf {
	var c conf
	if b, err := os.ReadFile(confPath()); err == nil {
		_ = json.Unmarshal(b, &c)
	}
	if c.Hosts == nil {
		c.Hosts = []Host{}
	}
	return c
}

func saveConf(c conf) error {
	b, _ := json.MarshalIndent(c, "", "  ")
	tmp := confPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, confPath())
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

func randID(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// public — машины для страницы: пароль не отдаём, только «есть ли».
func public(c conf) map[string]any {
	type pub struct {
		Host
		Saved bool `json:"saved"`
	}
	hs := make([]pub, 0, len(c.Hosts))
	for _, h := range c.Hosts {
		p := pub{Host: h, Saved: h.Pass != ""}
		p.Pass = ""
		hs = append(hs, p)
	}
	return map[string]any{"relay": c.Relay, "hosts": hs}
}

func handleConf(w http.ResponseWriter, r *http.Request) {
	confMu.Lock()
	defer confMu.Unlock()
	c := loadConf()
	if r.Method != http.MethodPost {
		writeJSON(w, public(c))
		return
	}
	var req struct {
		Op    string `json:"op"`
		Relay string `json:"relay"`
		Host
		Forget bool `json:"forget"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		writeJSON(w, map[string]string{"error": "нужен JSON {\"op\"}"})
		return
	}
	var err error
	switch req.Op {
	case "relay":
		c.Relay = strings.TrimSpace(req.Relay)
		if c.Relay != "" && !strings.Contains(c.Relay, ":") {
			c.Relay += ":4822"
		}
		err = saveConf(c)
	case "check":
		relay := strings.TrimSpace(req.Relay)
		if relay == "" {
			relay = c.Relay
		}
		if err = checkRelay(relay); err == nil {
			writeJSON(w, map[string]any{"ok": true})
			return
		}
	case "add":
		h := req.Host
		h.Name, h.Host = strings.TrimSpace(h.Name), strings.TrimSpace(h.Host)
		if h.Host == "" {
			err = errors.New("нужен адрес машины")
			break
		}
		if h.Name == "" {
			h.Name = h.Host
		}
		if h.ID == "" {
			h.ID = randID(4)
		}
		out := c.Hosts[:0]
		for _, o := range c.Hosts {
			if o.ID == h.ID {
				// Правка без нового пароля — старый остаётся, если не «забыть».
				if h.Pass == "" && !req.Forget {
					h.Pass = o.Pass
				}
				continue
			}
			out = append(out, o)
		}
		c.Hosts = append(out, h)
		err = saveConf(c)
	case "remove":
		out := c.Hosts[:0]
		for _, o := range c.Hosts {
			if o.ID != req.ID {
				out = append(out, o)
			}
		}
		c.Hosts = out
		err = saveConf(c)
	default:
		err = errors.New("op: relay, check, add или remove")
	}
	if err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, public(c))
}

// ---------------------------------------------------------------------------
// Протокол Guacamole: инструкция — «длина.значение,длина.значение;», длина —
// в символах Юникода.

func encode(op string, args ...string) []byte {
	var b bytes.Buffer
	for i, a := range append([]string{op}, args...) {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.Itoa(utf8.RuneCountInString(a)))
		b.WriteByte('.')
		b.WriteString(a)
	}
	b.WriteByte(';')
	return b.Bytes()
}

// readInstr читает одну инструкцию (для рукопожатия).
func readInstr(r *bufio.Reader) ([]string, error) {
	var out []string
	for {
		ls, err := r.ReadString('.')
		if err != nil {
			return nil, err
		}
		n, err := strconv.Atoi(strings.TrimSuffix(ls, "."))
		if err != nil || n < 0 || n > 1<<20 {
			return nil, errors.New("guacd ответил не по протоколу")
		}
		var sb strings.Builder
		for i := 0; i < n; i++ {
			ch, _, err := r.ReadRune()
			if err != nil {
				return nil, err
			}
			sb.WriteRune(ch)
		}
		out = append(out, sb.String())
		sep, err := r.ReadByte()
		if err != nil {
			return nil, err
		}
		if sep == ';' {
			return out, nil
		}
	}
}

// quality — как у сайта: «high» — всё красиво, «low» — для плохой связи.
var quality = map[string]map[string]string{
	"high": {"color-depth": "32", "enable-wallpaper": "true", "enable-theming": "true", "enable-font-smoothing": "true",
		"enable-full-window-drag": "true", "enable-desktop-composition": "true", "enable-menu-animations": "true"},
	"medium": {"color-depth": "16", "enable-wallpaper": "false", "enable-theming": "true", "enable-font-smoothing": "true",
		"enable-full-window-drag": "false", "enable-desktop-composition": "false", "enable-menu-animations": "false"},
	"low": {"color-depth": "8", "enable-wallpaper": "false", "enable-theming": "false", "enable-font-smoothing": "false",
		"enable-full-window-drag": "false", "enable-desktop-composition": "false", "enable-menu-animations": "false"},
}

func dialRelay(relay string) (net.Conn, error) {
	if relay == "" {
		return nil, errors.New("не задан ретранслятор RDP (guacd) — впиши его адрес на вкладке «Ретранслятор»")
	}
	c, err := devnet.Dial("tcp", relay)
	if err != nil {
		return nil, fmt.Errorf("ретранслятор %s не отвечает: %v", relay, err)
	}
	return c, nil
}

// checkRelay — живой ли guacd: просим список параметров RDP и закрываем.
func checkRelay(relay string) error {
	c, err := dialRelay(relay)
	if err != nil {
		return err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(8 * time.Second))
	if _, err := c.Write(encode("select", "rdp")); err != nil {
		return err
	}
	in, err := readInstr(bufio.NewReader(c))
	if err != nil || len(in) == 0 || in[0] != "args" {
		return fmt.Errorf("%s — это не guacd (ответ: %v)", relay, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Сеансы.

type sess struct {
	conn  net.Conn
	rd    *bufio.Reader
	mu    sync.Mutex // один read за раз
	rest  []byte     // хвост без «;» с прошлого чтения
	wmu   sync.Mutex
	touch time.Time
}

var (
	smu      sync.Mutex
	sessions = map[string]*sess{}
)

func closeAll() {
	smu.Lock()
	defer smu.Unlock()
	for id, s := range sessions {
		s.conn.Close()
		delete(sessions, id)
	}
}

func getSess(r *http.Request) (*sess, string) {
	id := r.URL.Query().Get("sid")
	smu.Lock()
	defer smu.Unlock()
	s := sessions[id]
	if s != nil {
		s.touch = time.Now()
	}
	return s, id
}

// reaper закрывает брошенные сеансы (вкладку закрыли без «Отключить»).
func init() {
	go func() {
		for range time.Tick(15 * time.Second) {
			smu.Lock()
			for id, s := range sessions {
				if time.Since(s.touch) > time.Minute {
					s.conn.Close()
					delete(sessions, id)
				}
			}
			smu.Unlock()
		}
	}()
}

func handleOpen(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID       string `json:"id"`
		W        int    `json:"w"`
		H        int    `json:"h"`
		DPI      int    `json:"dpi"`
		Quality  string `json:"quality"`
		User     string `json:"user"`
		Pass     string `json:"pass"`
		Remember bool   `json:"remember"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		writeJSON(w, map[string]string{"error": "нужен JSON"})
		return
	}
	confMu.Lock()
	c := loadConf()
	var h *Host
	for i := range c.Hosts {
		if c.Hosts[i].ID == req.ID {
			h = &c.Hosts[i]
		}
	}
	if h != nil && req.Remember && (req.User != "" || req.Pass != "") {
		if req.User != "" {
			h.User = req.User
		}
		if req.Pass != "" {
			h.Pass = req.Pass
		}
		_ = saveConf(c)
	}
	confMu.Unlock()
	if h == nil {
		writeJSON(w, map[string]string{"error": "нет такой машины"})
		return
	}
	user, pass := h.User, h.Pass
	if req.User != "" {
		user = req.User
	}
	if req.Pass != "" {
		pass = req.Pass
	}
	if req.W < 320 || req.W > 8192 {
		req.W = 1280
	}
	if req.H < 240 || req.H > 8192 {
		req.H = 720
	}
	if req.DPI < 72 || req.DPI > 400 {
		req.DPI = 96
	}
	q := quality[req.Quality]
	if q == nil {
		q = quality["medium"]
	}

	conn, err := dialRelay(c.Relay)
	if err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
	rd := bufio.NewReaderSize(conn, 64<<10)
	fail := func(e error) {
		conn.Close()
		writeJSON(w, map[string]string{"error": e.Error()})
	}
	if _, err := conn.Write(encode("select", "rdp")); err != nil {
		fail(err)
		return
	}
	args, err := readInstr(rd)
	if err != nil || len(args) == 0 || args[0] != "args" {
		fail(fmt.Errorf("%s — не guacd или он занят: %v", c.Relay, err))
		return
	}
	port := h.Port
	if port == 0 {
		port = 3389
	}
	params := map[string]string{
		"hostname": h.Host, "port": strconv.Itoa(port), "username": user, "password": pass, "domain": h.Domain,
		"ignore-cert": "true", "security": "any",
		"width": strconv.Itoa(req.W), "height": strconv.Itoa(req.H), "dpi": strconv.Itoa(req.DPI),
		// Повернул телефон — Windows сама меняет разрешение (RDP 8.1+).
		"resize-method": "display-update",
	}
	for k, v := range q {
		params[k] = v
	}
	var hs bytes.Buffer
	hs.Write(encode("size", strconv.Itoa(req.W), strconv.Itoa(req.H), strconv.Itoa(req.DPI)))
	hs.Write(encode("audio"))
	hs.Write(encode("video"))
	hs.Write(encode("image", "image/png", "image/jpeg", "image/webp"))
	vals := make([]string, 0, len(args)-1)
	for i, name := range args[1:] {
		if i == 0 && strings.HasPrefix(name, "VERSION_") {
			vals = append(vals, name) // версия протокола — отвечаем той же
			continue
		}
		vals = append(vals, params[name])
	}
	hs.Write(encode("connect", vals...))
	if _, err := conn.Write(hs.Bytes()); err != nil {
		fail(err)
		return
	}
	_ = conn.SetDeadline(time.Time{})
	id := randID(12)
	smu.Lock()
	sessions[id] = &sess{conn: conn, rd: rd, touch: time.Now()}
	smu.Unlock()
	writeJSON(w, map[string]string{"sid": id})
}

// handleRead отдаёт всё, что прислал guacd, целыми инструкциями (до
// последней «;»), ждёт данных до 20 с.
func handleRead(w http.ResponseWriter, r *http.Request) {
	s, id := getSess(r)
	if s == nil {
		http.Error(w, "сеанс закрыт", http.StatusGone)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	buf := make([]byte, 256<<10)
	out := append([]byte(nil), s.rest...)
	s.rest = nil
	deadline := time.Now().Add(20 * time.Second)
	for {
		if bytes.IndexByte(out, ';') >= 0 {
			// Данные есть — дочитываем то, что уже пришло, но не ждём.
			_ = s.conn.SetReadDeadline(time.Now().Add(15 * time.Millisecond))
		} else {
			_ = s.conn.SetReadDeadline(deadline)
		}
		n, err := s.rd.Read(buf)
		out = append(out, buf[:n]...)
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				if bytes.IndexByte(out, ';') >= 0 || time.Now().After(deadline) {
					break
				}
				continue
			}
			// guacd закрыл — отдаём остаток и закрываем сеанс.
			smu.Lock()
			delete(sessions, id)
			smu.Unlock()
			s.conn.Close()
			if len(out) == 0 {
				http.Error(w, "сеанс закрыт", http.StatusGone)
				return
			}
			break
		}
		if len(out) > 512<<10 {
			break
		}
	}
	if i := bytes.LastIndexByte(out, ';'); i >= 0 {
		s.rest = append(s.rest, out[i+1:]...)
		out = out[:i+1]
	} else {
		s.rest, out = out, nil
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(out)
}

func handleWrite(w http.ResponseWriter, r *http.Request) {
	s, _ := getSess(r)
	if s == nil {
		http.Error(w, "сеанс закрыт", http.StatusGone)
		return
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err == nil {
		s.wmu.Lock()
		_, err = s.conn.Write(b)
		s.wmu.Unlock()
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusGone)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func handleClose(w http.ResponseWriter, r *http.Request) {
	s, id := getSess(r)
	if s != nil {
		s.wmu.Lock()
		_, _ = s.conn.Write(encode("disconnect"))
		s.wmu.Unlock()
		s.conn.Close()
		smu.Lock()
		delete(sessions, id)
		smu.Unlock()
	}
	w.WriteHeader(http.StatusNoContent)
}
