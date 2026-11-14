package meshsvc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// Device — устройство сети глазами панели (см. adminDevice в cmd/meshd).
type Device struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Alias       string  `json:"alias,omitempty"`
	Display     string  `json:"display"`
	Host        string  `json:"host"`
	IP          string  `json:"ip"`
	User        string  `json:"user,omitempty"`
	Online      bool    `json:"online"`
	Platform    string  `json:"platform,omitempty"`
	OS          string  `json:"os,omitempty"`
	App         string  `json:"app,omitempty"`
	Mode        string  `json:"mode,omitempty"`
	Via         string  `json:"via,omitempty"`
	Hostname    string  `json:"hostname,omitempty"`
	FirstSeen   int64   `json:"firstSeen,omitempty"`
	LastSeen    int64   `json:"lastSeen,omitempty"`
	ConnectedAt int64   `json:"connectedAt,omitempty"`
	Sessions    int64   `json:"sessions"`
	RTTMs       float64 `json:"rttMs,omitempty"`
	RTTAvgMs    float64 `json:"rttAvgMs,omitempty"`
	JitterMs    float64 `json:"jitterMs,omitempty"`
	LossPct     float64 `json:"lossPct"`
	Quality     string  `json:"quality"`
	BytesUp     int64   `json:"bytesUp"`
	BytesDown   int64   `json:"bytesDown"`
	ActiveCalls int64   `json:"activeCalls"`
	NAT         *NAT    `json:"nat,omitempty"`
	// Direct — с какими устройствами у этого сейчас прямое соединение
	// (минуя сервер), по отчёту самого устройства.
	Direct []DirectLink `json:"direct,omitempty"`
}

// DirectLink — прямое соединение с другим устройством сети.
type DirectLink struct {
	IP    string  `json:"ip"`
	RTTMs float64 `json:"rttMs,omitempty"`
}

// NAT — что устройство узнало о своём NAT (см. mesh.NATInfo).
type NAT struct {
	Tested        int64  `json:"tested"`
	UDP           bool   `json:"udp"`
	Mapping       string `json:"mapping,omitempty"`
	Filtering     string `json:"filtering,omitempty"`
	PublicIP      string `json:"publicIp,omitempty"`
	PortPreserved bool   `json:"portPreserved,omitempty"`
	PortDelta     int    `json:"portDelta,omitempty"`
	IPv6          bool   `json:"ipv6,omitempty"`
}

// Network — одна сеть (все устройства с одинаковым ключом).
type Network struct {
	ID string `json:"id"`
	// Name — имя сети из панели; Created — когда сеть появилась (unix);
	// EmptySince — с какого момента в ней нет устройств (0 — есть).
	// Старый meshd этих полей не присылает — они нулевые.
	Name       string   `json:"name"`
	Created    int64    `json:"created"`
	EmptySince int64    `json:"emptySince"`
	Devices    []Device `json:"devices"`
}

// Server — подключённый побочный сервер.
type Server struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Host        string   `json:"host"`
	IP          string   `json:"ip,omitempty"` // адрес в сети устройств
	MeshHost    string   `json:"meshHost,omitempty"`
	Addrs       []string `json:"addrs,omitempty"`
	App         string   `json:"app,omitempty"`
	ConnectedAt int64    `json:"connectedAt"`
	RTTMs       float64  `json:"rttMs,omitempty"`
	RTTAvgMs    float64  `json:"rttAvgMs,omitempty"`
	JitterMs    float64  `json:"jitterMs,omitempty"`
	LossPct     float64  `json:"lossPct"`
	Quality     string   `json:"quality"`
}

// Self — этот сервер как узел сети устройств.
type Self struct {
	Name    string `json:"name"`
	Host    string `json:"host"`
	IP      string `json:"ip"`
	Enabled bool   `json:"enabled"`
	Ports   string `json:"ports,omitempty"`
	// ID — id этого сервера, Pol — отпечаток его правил доступа (meshd 4+).
	ID  string `json:"id,omitempty"`
	Pol string `json:"pol,omitempty"`
}

// SeenServer — другой сервер той же сети, о котором рассказали устройства
// (они на связи и с ним). Same — правила доступа там такие же (по
// отпечатку); Devices — сколько устройств этого сервера сейчас на связи и
// с тем.
type SeenServer struct {
	ID       string `json:"id"`
	Name     string `json:"name,omitempty"`
	Pol      string `json:"pol,omitempty"`
	Same     bool   `json:"same"`
	Devices  int    `json:"devices"`
	LastSeen int64  `json:"lastSeen,omitempty"`
}

// KnownServer — сервер сети, который панель советует устройствам.
type KnownServer struct {
	Name string `json:"name,omitempty"`
	Host string `json:"host"`
	Port int    `json:"port,omitempty"`
}

// State — ответ meshd на /v1/state.
type State struct {
	Version   string    `json:"version"`
	StartedAt int64     `json:"startedAt"`
	Networks  []Network `json:"networks"`
	Servers   []Server  `json:"servers"`
	Self      Self      `json:"self"`
	// Peers и Known — несколько серверов без главного (meshd 4+; у старого
	// пусто).
	Peers []SeenServer  `json:"peers"`
	Known []KnownServer `json:"known"`
}

// Admin — клиент входа meshd для панели (HTTP по unix-сокету).
type Admin struct {
	http *http.Client
}

// NewAdmin — клиент для сокета sock (обычно AdminSocket).
func NewAdmin(sock string) *Admin {
	return &Admin{http: &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			},
		},
	}}
}

// ErrNotRunning — meshd не отвечает: не установлен, остановлен или запущен
// без входа для панели (старая версия).
var ErrNotRunning = errors.New("meshd не отвечает")

// ErrUnsupported — meshd старой версии не знает такого запроса.
var ErrUnsupported = errors.New("эта версия meshd не умеет этого — обновите meshd на сервере (Сеть устройств → переустановить)")

func (a *Admin) do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://meshd"+path, rd)
	if err != nil {
		return err
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrNotRunning, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusNotFound && !json.Valid(data) {
		// Старый meshd: путь неизвестен, отвечает текстом «404 page not found».
		return ErrUnsupported
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			return errors.New(e.Error)
		}
		return fmt.Errorf("meshd ответил %s", resp.Status)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}

// State — все сети, устройства и подключённые серверы.
func (a *Admin) State(ctx context.Context) (State, error) {
	var st State
	err := a.do(ctx, http.MethodGet, "/v1/state", nil, &st)
	return st, err
}

type deviceReq struct {
	Net    string `json:"net"`
	Device string `json:"device"`
	Alias  string `json:"alias,omitempty"`
}

// Rename задаёт устройству имя; пустое — вернуть его собственное.
func (a *Admin) Rename(ctx context.Context, netID, device, alias string) error {
	return a.do(ctx, http.MethodPost, "/v1/rename", deviceReq{Net: netID, Device: device, Alias: alias}, nil)
}

// Forget убирает устройство из сети.
func (a *Admin) Forget(ctx context.Context, netID, device string) error {
	return a.do(ctx, http.MethodPost, "/v1/forget", deviceReq{Net: netID, Device: device}, nil)
}

// Event — запись журнала событий meshd (см. /v1/events). Net, Device и Peer —
// id; Text — подпись для человека.
type Event struct {
	Seq    int64  `json:"seq"`
	Time   int64  `json:"time"`
	Type   string `json:"type"`
	Net    string `json:"net,omitempty"`
	Device string `json:"device,omitempty"`
	Peer   string `json:"peer,omitempty"`
	Port   int    `json:"port,omitempty"`
	Text   string `json:"text,omitempty"`
}

// Events — ответ /v1/events: Seq — номер последнего события в журнале.
type Events struct {
	Seq    int64   `json:"seq"`
	Events []Event `json:"events"`
}

// CheckResult — ответ /v1/check: можно ли устройству From открыть соединение
// к To на порт. Rule — индекс правила или -1; Reason — «rule», «policy off»,
// «no rule», «unknown device», «server cannot initiate».
type CheckResult struct {
	Allowed bool   `json:"allowed"`
	Rule    int    `json:"rule"`
	Reason  string `json:"reason"`
	Visible bool   `json:"visible"`
}

// RenameNetwork задаёт сети имя (пустое — убрать).
func (a *Admin) RenameNetwork(ctx context.Context, netID, name string) error {
	return a.do(ctx, http.MethodPost, "/v1/net/rename", map[string]string{"net": netID, "name": name}, nil)
}

// DeleteNetwork удаляет сеть и отключает её устройства.
func (a *Admin) DeleteNetwork(ctx context.Context, netID string) error {
	return a.do(ctx, http.MethodPost, "/v1/net/delete", map[string]string{"net": netID}, nil)
}

// Events — события журнала с номером больше after (limit 0 — по умолчанию).
func (a *Admin) Events(ctx context.Context, after int64, limit int) (Events, error) {
	var ev Events
	path := fmt.Sprintf("/v1/events?after=%d&limit=%d", after, limit)
	err := a.do(ctx, http.MethodGet, path, nil, &ev)
	if ev.Events == nil {
		ev.Events = []Event{}
	}
	return ev, err
}

// Check спрашивает, пустят ли устройство from к устройству to на порт.
func (a *Admin) Check(ctx context.Context, netID, from, to string, port int) (CheckResult, error) {
	var res CheckResult
	err := a.do(ctx, http.MethodPost, "/v1/check", map[string]any{"net": netID, "from": from, "to": to, "port": port}, &res)
	return res, err
}

// SetSelf — как этот сервер выглядит в сети устройств: имя, пускать ли к нему
// устройства и на какие порты ("" — на все).
func (a *Admin) SetSelf(ctx context.Context, name string, enabled bool, ports string) error {
	return a.do(ctx, http.MethodPost, "/v1/self", map[string]any{"name": name, "enabled": enabled, "ports": ports}, nil)
}

// SetSelfIP — адрес этого сервера в сети устройств (meshd 4+): если два
// сервера сети получили один и тот же или нужно сохранить прежний.
func (a *Admin) SetSelfIP(ctx context.Context, ip string) error {
	st, err := a.State(ctx)
	if err != nil {
		return err
	}
	return a.do(ctx, http.MethodPost, "/v1/self", map[string]any{"name": st.Self.Name,
		"enabled": st.Self.Enabled, "ports": st.Self.Ports, "ip": ip}, nil)
}

// SetKnown — какие серверы сети советовать устройствам (meshd 4+).
func (a *Admin) SetKnown(ctx context.Context, list []KnownServer) error {
	if list == nil {
		list = []KnownServer{}
	}
	return a.do(ctx, http.MethodPost, "/v1/known", map[string]any{"servers": list}, nil)
}

// ForgetServer забывает адрес отключённого побочного сервера.
func (a *Admin) ForgetServer(ctx context.Context, id string) error {
	return a.do(ctx, http.MethodPost, "/v1/forget-server", deviceReq{Device: id}, nil)
}

// PolicyRule — правило доступа: устройства группы From могут открывать
// соединения к устройствам группы To на порты Ports (пусто — на все).
// Группа "*" — все, "@servers" — серверы сети.
type PolicyRule struct {
	From  string `json:"from"`
	To    string `json:"to"`
	Ports string `json:"ports,omitempty"`
}

// Policy — права доступа устройств для meshd: Users — SSH-пользователь
// сервера (tun_*) → его группы.
type Policy struct {
	Enabled bool                `json:"enabled"`
	Users   map[string][]string `json:"users,omitempty"`
	Rules   []PolicyRule        `json:"rules,omitempty"`
}

// SetPolicy передаёт meshd права доступа устройств (он их сохраняет сам).
func (a *Admin) SetPolicy(ctx context.Context, p Policy) error {
	return a.do(ctx, http.MethodPost, "/v1/policy", p, nil)
}
