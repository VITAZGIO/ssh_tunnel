// Package nodeapi — общее у хоста плагинов сервера (ssh_tunnel_node,
// internal/node) и панели: адреса, типы управляющего входа и его клиент.
// Отдельно от internal/node, чтобы панель не тянула в себя сами плагины.
//
// Управляющий вход — HTTP по unix-сокету ControlSocket (права 0600, владелец
// — пользователь ssh_tunnel_node; панель работает от root):
//
//	GET  /state                                  State
//	POST /config            ConfigPatch          → State
//	POST /folder/add        {net,name,quotaMB}   → Folder
//	POST /folder/remove     {net,id,deleteData}  → {"ok":true}
//	POST /folder/quota      {net,id,quotaMB}     → {"ok":true}
//	POST /clipboard/clear   {net}                → {"ok":true}
//	POST /backup/delete     {net,scope,device,set} → {"ok":true}
//	POST /net/delete        {net}                → {"ok":true}  данные сети целиком
//
// Ошибки — HTTP 400 {"error":"<текст>"}.
package nodeapi

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

const (
	// Unit — служба systemd хоста плагинов.
	Unit = "ssh_tunnel_node.service"
	// User — пользователь системы, под которым он работает.
	User = "ssh_tunnel_node"
	// Binary — куда ставится.
	Binary = "/usr/local/bin/ssh_tunnel_node"
	// DataDir — его данные (StateDirectory юнита).
	DataDir = "/var/lib/ssh_tunnel_node"
	// ControlSocket — управляющий вход для панели.
	ControlSocket = "/run/ssh_tunnel_node/control.sock"

	// Порты плагинов на 127.0.0.1 сервера — те же, что у служб на
	// устройствах (devfiles.Port, mirror.Port, clipshare.Port): устройства
	// ходят на 198.19.255.254:ПОРТ, meshd ведёт на 127.0.0.1:ПОРТ.
	PortFiles     = 47835
	PortMirror    = 47836
	PortClipboard = 47837
	// PortBackup — «Резервные копии» (ТЗ 13): только на сервере.
	PortBackup = 47838

	// DefaultQuotaMB — сколько места всем плагинам вместе по умолчанию.
	DefaultQuotaMB = 10 * 1024
	// DefaultFolderQuotaMB — квота новой зеркальной папки по умолчанию.
	DefaultFolderQuotaMB = 2 * 1024
)

// Ports — порты всех плагинов.
var Ports = []int{PortFiles, PortMirror, PortClipboard, PortBackup}

// Плагины.
const (
	PluginMirror    = "mirror"
	PluginFiles     = "files"
	PluginClipboard = "clipboard"
	PluginBackup    = "backup"
)

// Caller — устройство сети, которое пришло к плагину (ответ meshd на who).
type Caller struct {
	Net    string `json:"net"` // id сети (хеш ключа)
	Device string `json:"device"`
	IP     string `json:"ip"`
	Host   string `json:"host"`
	Name   string `json:"name"`
	User   string `json:"user"`
}

// Config — настройки хоста (хранятся у него, меняет панель).
type Config struct {
	Mirror     bool  `json:"mirror"`
	Files      bool  `json:"files"`
	FilesWrite bool  `json:"filesWrite"`
	Clipboard  bool  `json:"clipboard"`
	Backup     bool  `json:"backup"`
	QuotaMB    int64 `json:"quotaMB"` // всем плагинам вместе
}

// ConfigPatch — изменение настроек: nil — не трогать.
type ConfigPatch struct {
	Mirror     *bool  `json:"mirror,omitempty"`
	Files      *bool  `json:"files,omitempty"`
	FilesWrite *bool  `json:"filesWrite,omitempty"`
	Clipboard  *bool  `json:"clipboard,omitempty"`
	Backup     *bool  `json:"backup,omitempty"`
	QuotaMB    *int64 `json:"quotaMB,omitempty"`
}

// Folder — зеркальная папка сети на сервере.
type Folder struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	QuotaMB   int64  `json:"quotaMB"`
	UsedBytes int64  `json:"usedBytes"`
	Files     int    `json:"files"`
	Conflicts int    `json:"conflicts,omitempty"`
}

// Clipboard — буфер сети на сервере.
type Clipboard struct {
	Items   int   `json:"items"`
	Bytes   int64 `json:"bytes"`
	Devices int   `json:"devices"`
}

// Network — данные одной сети на сервере.
type Network struct {
	ID         string    `json:"id"`
	Folders    []Folder  `json:"folders"`
	FilesBytes int64     `json:"filesBytes"`
	Clipboard  Clipboard `json:"clipboard"`
	UsedBytes  int64     `json:"usedBytes"`
	// Backups — резервные копии устройств сети, BackupBytes — сколько они
	// занимают на диске (одинаковое содержимое — один раз).
	Backups     []BackupSet `json:"backups,omitempty"`
	BackupBytes int64       `json:"backupBytes,omitempty"`
	// LastActive — последнее обращение устройства по плагинам (unix).
	LastActive map[string]int64 `json:"lastActive,omitempty"`
}

// BackupSet — резервная копия одной папки одного устройства.
type BackupSet struct {
	Scope      string `json:"scope"` // чьё: пользователь или устройство (для удаления)
	Device     string `json:"device"`
	DeviceName string `json:"deviceName,omitempty"`
	Set        string `json:"set"`
	Name       string `json:"name"`
	Keep       int    `json:"keep"`
	Versions   int    `json:"versions"`
	Bytes      int64  `json:"bytes"` // последняя версия
	Last       int64  `json:"last,omitempty"`
}

// BackupReq — убрать резервную копию.
type BackupReq struct {
	Net    string `json:"net"`
	Scope  string `json:"scope"`
	Device string `json:"device"`
	Set    string `json:"set"`
}

// Event — заметное событие или ошибка плагина.
type Event struct {
	Time   int64  `json:"time"`
	Plugin string `json:"plugin,omitempty"`
	Net    string `json:"net,omitempty"`
	Error  bool   `json:"error,omitempty"`
	Text   string `json:"text"`
}

// State — что хост рассказывает панели.
type State struct {
	Version string `json:"version"`
	Config  Config `json:"config"`
	// Meshd — может ли хост узнать, кто пришёл: "ok", "old" (meshd без
	// op who: никого не пускаем), "down" (meshd не отвечает), "" — ещё
	// никто не приходил.
	Meshd     string    `json:"meshd,omitempty"`
	UsedBytes int64     `json:"usedBytes"`
	Networks  []Network `json:"networks"`
	Events    []Event   `json:"events"`
	// Listening — какие плагины сейчас слушают свой порт.
	Listening map[string]bool `json:"listening"`
}

// FolderReq — запросы про зеркальные папки.
type FolderReq struct {
	Net        string `json:"net"`
	ID         string `json:"id,omitempty"`
	Name       string `json:"name,omitempty"`
	QuotaMB    int64  `json:"quotaMB,omitempty"`
	DeleteData bool   `json:"deleteData,omitempty"`
}

// API — управляющий вход (Client; в тестах панели — подделка).
type API interface {
	State(ctx context.Context) (State, error)
	SetConfig(ctx context.Context, p ConfigPatch) (State, error)
	AddFolder(ctx context.Context, r FolderReq) (Folder, error)
	RemoveFolder(ctx context.Context, r FolderReq) error
	SetFolderQuota(ctx context.Context, r FolderReq) error
	ClearClipboard(ctx context.Context, net string) error
	DeleteBackup(ctx context.Context, r BackupReq) error
	DeleteNet(ctx context.Context, net string) error
}

// Client — API поверх unix-сокета.
type Client struct {
	hc *http.Client
}

// NewClient — клиент управляющего входа по сокету path.
func NewClient(path string) *Client {
	return &Client{hc: &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		}},
	}}
}

// ErrNotRunning — хост не запущен (сокета нет или он не отвечает).
var ErrNotRunning = errors.New("хост плагинов не запущен")

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://node"+path, body)
	if err != nil {
		return err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrNotRunning, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(data, &e)
		if e.Error == "" {
			e.Error = resp.Status
		}
		return errors.New(e.Error)
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

func (c *Client) State(ctx context.Context) (State, error) {
	var st State
	err := c.do(ctx, http.MethodGet, "/state", nil, &st)
	return st, err
}

func (c *Client) SetConfig(ctx context.Context, p ConfigPatch) (State, error) {
	var st State
	err := c.do(ctx, http.MethodPost, "/config", p, &st)
	return st, err
}

func (c *Client) AddFolder(ctx context.Context, r FolderReq) (Folder, error) {
	var f Folder
	err := c.do(ctx, http.MethodPost, "/folder/add", r, &f)
	return f, err
}

func (c *Client) RemoveFolder(ctx context.Context, r FolderReq) error {
	return c.do(ctx, http.MethodPost, "/folder/remove", r, nil)
}

func (c *Client) SetFolderQuota(ctx context.Context, r FolderReq) error {
	return c.do(ctx, http.MethodPost, "/folder/quota", r, nil)
}

func (c *Client) ClearClipboard(ctx context.Context, n string) error {
	return c.do(ctx, http.MethodPost, "/clipboard/clear", map[string]string{"net": n}, nil)
}

func (c *Client) DeleteBackup(ctx context.Context, r BackupReq) error {
	return c.do(ctx, http.MethodPost, "/backup/delete", r, nil)
}

func (c *Client) DeleteNet(ctx context.Context, n string) error {
	return c.do(ctx, http.MethodPost, "/net/delete", map[string]string{"net": n}, nil)
}

// ValidNet — id сети от meshd: шестнадцатеричная строка (хеш ключа). Из
// него собирается путь на диске, поэтому ничего другого не пускаем.
func ValidNet(id string) bool {
	if len(id) == 0 || len(id) > 64 {
		return false
	}
	for _, c := range id {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
