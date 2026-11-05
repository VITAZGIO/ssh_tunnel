// Package phonelink — плагин «Связь с телефоном» (как KDE Connect): между
// своими устройствами по сети устройств — уведомления телефона на ПК,
// «Найти устройство» (громкий звонок), пульт музыки, телефон как тачпад и
// клавиатура для ПК, заряд батареи.
//
// Устройства говорят друг с другом по HTTP на порту Port сети устройств
// (как «Общий буфер»): POST /ev {kind, …}. Что делать с пришедшим, решает
// платформа: на ПК — нажатия и мышь (input_*.go) и уведомление у часов, на
// телефоне — приложение (SetPhone: звонок, медиа-кнопки).
package phonelink

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"time"

	"sshtunnel/internal/addons"
	"sshtunnel/internal/devnet"
	"sshtunnel/internal/mesh"
)

// ID — плагин в каталоге.
const ID = "phonelink"

// Port — порт плагина в сети устройств.
const Port = 47839

func init() {
	addons.Register(ID, func() addons.Module { return &module{} })
	addons.RegisterRoutes(ID, addons.Route{
		Path: "/api/phonelink", Methods: "GET, POST",
		Desc: "Связь с телефоном: GET — устройства (заряд), уведомления с других устройств, настройки; POST {\"op\":\"ring|media|mouse|text|clear|set\",\"peer\",…} — позвонить, медиа-кнопка (playpause|next|prev|volup|voldown|mute), мышь {dx,dy,btn,scroll}, напечатать текст, image {img: base64 JPEG/PNG} — в буфер обмена компьютера и Ctrl+V, очистить уведомления, настройки {sendNotifs,allowControl}.",
		H:    handleAPI,
	})
}

// Ev — сообщение между устройствами.
type Ev struct {
	Kind string `json:"kind"` // notif, ring, media, mouse, text, image, battery

	App   string `json:"app,omitempty"` // notif
	Title string `json:"title,omitempty"`
	Text  string `json:"text,omitempty"` // notif, text
	Key   string `json:"key,omitempty"`  // media
	// Img — картинка (JPEG/PNG в base64): кладётся в буфер обмена
	// компьютера и сразу вставляется (Ctrl+V) — фото с телефона в сообщение.
	Img string `json:"img,omitempty"`

	DX     int    `json:"dx,omitempty"` // mouse
	DY     int    `json:"dy,omitempty"`
	Btn    string `json:"btn,omitempty"` // left, right, middle — щелчок
	Scroll int    `json:"scroll,omitempty"`

	Level    int  `json:"level,omitempty"` // battery, %
	Charging bool `json:"charging,omitempty"`

	From string `json:"from,omitempty"` // заполняет получатель
	Time int64  `json:"time,omitempty"`
}

// Phone — что умеет сам телефон (задаёт приложение Android).
type Phone interface {
	Ring()
	StopRing()
	Media(key string)
}

// Settings — настройки этого устройства.
type Settings struct {
	SendNotifs   bool `json:"sendNotifs"`   // телефон: пересылать свои уведомления
	AllowControl bool `json:"allowControl"` // пускать мышь/клавиатуру/медиа с других устройств
}

type battery struct {
	Level    int   `json:"level"`
	Charging bool  `json:"charging"`
	Time     int64 `json:"time"`
}

var (
	mu       sync.Mutex
	phone    Phone
	notifs   []Ev
	bats     = map[string]battery{} // ip → заряд
	settings = Settings{SendNotifs: true, AllowControl: true}
	loaded   bool
	running  bool
)

// Notify — уведомление у часов на ПК (задаёт окно программы:
// nativeui.Notify). Через переменную, чтобы ядро телефона не тянуло окно.
var Notify func(title, text string)

func notify(title, text string) {
	if f := Notify; f != nil {
		f(title, text)
	}
}

// SetPhone — телефон подключает свои звонок и медиа-кнопки.
func SetPhone(p Phone) {
	mu.Lock()
	phone = p
	mu.Unlock()
}

func setPath() string { return filepath.Join(addons.DataDir(), "phonelink.json") }

func loadSettings() {
	if loaded {
		return
	}
	loaded = true
	if b, err := os.ReadFile(setPath()); err == nil {
		_ = json.Unmarshal(b, &settings)
	}
}

// Current — настройки (телефон спрашивает перед пересылкой уведомления).
func Current() Settings {
	mu.Lock()
	defer mu.Unlock()
	loadSettings()
	return settings
}

// Active — плагин добавлен и включён на этом устройстве.
func Active() bool {
	mu.Lock()
	defer mu.Unlock()
	return running
}

type module struct {
	srv *http.Server
	ln  *chanListener
}

type chanListener struct {
	ch   chan net.Conn
	done chan struct{}
	once sync.Once
}

func (l *chanListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}
func (l *chanListener) Close() error   { l.once.Do(func() { close(l.done) }); return nil }
func (l *chanListener) Addr() net.Addr { return &net.TCPAddr{Port: Port} }

type fromKey struct{}

type callerConn struct {
	net.Conn
	from mesh.Caller
}

func (m *module) Start() error {
	m.ln = &chanListener{ch: make(chan net.Conn), done: make(chan struct{})}
	mux := http.NewServeMux()
	mux.HandleFunc("/ev", receive)
	// /hello — «я здесь, плагин есть, я телефон/компьютер»: по нему страница
	// показывает только свои устройства с этим плагином, без выбора.
	mux.HandleFunc("/hello", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]string{"role": role()})
	})
	m.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second,
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			if cc, ok := c.(callerConn); ok {
				return context.WithValue(ctx, fromKey{}, cc.from)
			}
			return ctx
		}}
	go m.srv.Serve(m.ln)
	ln := m.ln
	mesh.Serve(Port, func(c net.Conn, from mesh.Caller) {
		select {
		case ln.ch <- callerConn{c, from}:
		case <-ln.done:
			c.Close()
		}
	})
	mu.Lock()
	running = true
	loadSettings()
	mu.Unlock()
	return nil
}

func (m *module) Stop() {
	mesh.Serve(Port, nil)
	if m.srv != nil {
		m.srv.Close()
	}
	if m.ln != nil {
		m.ln.Close()
	}
	mu.Lock()
	running = false
	mu.Unlock()
}

// role — телефон или компьютер (что показывать на карточке).
func role() string {
	if runtime.GOOS == "android" {
		return "phone"
	}
	return "computer"
}

// hello — у устройства есть этот плагин? Его роль, "" — нет.
func hello(ip string) string {
	c := &http.Client{Timeout: 3 * time.Second, Transport: peerClient.Transport}
	resp, err := c.Get("http://" + net.JoinHostPort(ip, "47839") + "/hello")
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var v struct {
		Role string `json:"role"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 512)).Decode(&v) != nil {
		return ""
	}
	return v.Role
}

func peerName(ip, fallback string) string {
	for _, p := range devnet.Status().Peers {
		if p.IP.String() == ip {
			if p.Name != "" {
				return p.Name
			}
			return p.Host
		}
	}
	return fallback
}

// maxBody — с запасом под фото (картинку страница ужимает до ~2560 px).
const maxBody = 16 << 20

func receive(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST", http.StatusMethodNotAllowed)
		return
	}
	var e Ev
	if err := json.NewDecoder(io.LimitReader(r.Body, maxBody)).Decode(&e); err != nil {
		http.Error(w, "json", http.StatusBadRequest)
		return
	}
	from, _ := r.Context().Value(fromKey{}).(mesh.Caller)
	e.From = peerName(from.IP, from.Host)
	e.Time = time.Now().Unix()
	if err := handleEv(e, from.IP); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func handleEv(e Ev, ip string) error {
	mu.Lock()
	loadSettings()
	allow, p := settings.AllowControl, phone
	switch e.Kind {
	case "notif":
		notifs = append([]Ev{e}, notifs...)
		if len(notifs) > 100 {
			notifs = notifs[:100]
		}
	case "battery":
		bats[ip] = battery{e.Level, e.Charging, e.Time}
	}
	mu.Unlock()
	switch e.Kind {
	case "notif":
		showNotif(e)
	case "ring":
		switch {
		case e.Key == "stop" && p != nil:
			p.StopRing()
		case e.Key == "stop":
			// у компьютера сигнал короткий — останавливать нечего
		case p != nil:
			p.Ring()
		default:
			ring(e.From)
		}
	case "media", "mouse", "text", "image":
		if !allow {
			return errors.New("управление этим устройством выключено")
		}
		if e.Kind == "media" && p != nil {
			p.Media(e.Key)
			return nil
		}
		if e.Kind == "image" {
			b, err := base64.StdEncoding.DecodeString(e.Img)
			if err != nil {
				return errors.New("картинка испорчена")
			}
			return pasteImage(b)
		}
		return inject(e)
	}
	return nil
}

var peerClient = &http.Client{
	Timeout: 8 * time.Second,
	Transport: &http.Transport{
		DialContext: func(_ context.Context, _, addr string) (net.Conn, error) {
			return devnet.DialPeer(addr)
		},
		MaxIdleConnsPerHost: 2,
		IdleConnTimeout:     60 * time.Second,
	},
}

func sendTo(ip string, e Ev) error {
	b, _ := json.Marshal(e)
	resp, err := peerClient.Post("http://"+net.JoinHostPort(ip, "47839")+"/ev", "application/json", bytes.NewReader(b))
	if err != nil {
		return errors.New("устройство не отвечает — на нём нет плагина «Связь с телефоном» или оно не в сети")
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
		return errors.New(string(bytes.TrimSpace(msg)))
	}
	return nil
}

// Broadcast — всем устройствам сети, кто онлайн (уведомление, заряд).
func Broadcast(e Ev) int {
	if !Active() {
		return 0
	}
	n := 0
	var wg sync.WaitGroup
	var nmu sync.Mutex
	for _, p := range devnet.Status().Peers {
		if p.Self || !p.Online || !p.IP.IsValid() {
			continue
		}
		wg.Add(1)
		go func(ip string) {
			defer wg.Done()
			if sendTo(ip, e) == nil {
				nmu.Lock()
				n++
				nmu.Unlock()
			}
		}(p.IP.String())
	}
	wg.Wait()
	return n
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

func handleAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		st := devnet.Status()
		type peer struct {
			IP      string   `json:"ip"`
			Name    string   `json:"name"`
			Online  bool     `json:"online"`
			Role    string   `json:"role"`
			Battery *battery `json:"battery,omitempty"`
		}
		// Только свои устройства, где тоже есть этот плагин (спрашиваем
		// /hello разом): выбирать, к кому подключаться, не нужно.
		var ps []peer
		var pmu sync.Mutex
		var wg sync.WaitGroup
		for _, p := range st.Peers {
			if p.Self || !p.IP.IsValid() || !p.Online {
				continue
			}
			pp := peer{IP: p.IP.String(), Name: p.Name, Online: p.Online}
			if pp.Name == "" {
				pp.Name = p.Host
			}
			wg.Add(1)
			go func(pp peer) {
				defer wg.Done()
				if pp.Role = hello(pp.IP); pp.Role == "" {
					return
				}
				pmu.Lock()
				ps = append(ps, pp)
				pmu.Unlock()
			}(pp)
		}
		wg.Wait()
		mu.Lock()
		loadSettings()
		for i := range ps {
			if b, ok := bats[ps[i].IP]; ok {
				b := b
				ps[i].Battery = &b
			}
		}
		sort.Slice(ps, func(i, j int) bool {
			return ps[i].Online && !ps[j].Online || ps[i].Online == ps[j].Online && ps[i].Name < ps[j].Name
		})
		out := map[string]any{"devnet": st.State, "peers": ps, "role": role(), "notifs": append([]Ev{}, notifs...), "settings": settings}
		mu.Unlock()
		writeJSON(w, out)
		return
	}
	var req struct {
		Op   string `json:"op"`
		Peer string `json:"peer"`
		Ev
		Settings *Settings `json:"settings"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, maxBody)).Decode(&req); err != nil {
		writeJSON(w, map[string]string{"error": "нужен JSON {\"op\"}"})
		return
	}
	var err error
	switch req.Op {
	case "clear":
		mu.Lock()
		notifs = nil
		mu.Unlock()
	case "set":
		if req.Settings != nil {
			mu.Lock()
			settings = *req.Settings
			b, _ := json.MarshalIndent(settings, "", "  ")
			err = os.WriteFile(setPath(), b, 0o600)
			mu.Unlock()
		}
	case "ring", "media", "mouse", "text", "image":
		if req.Peer == "" {
			err = errors.New("выбери устройство")
			break
		}
		e := req.Ev
		e.Kind = req.Op
		err = sendTo(req.Peer, e)
	default:
		err = errors.New("op: ring, media, mouse, text, image, clear или set")
	}
	if err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}
