// Package apicli — управление работающей программой без окна: команды
// (`ssh_tunnel.exe state`, `tunnel off`, …) и MCP-сервер для нейросетей
// (`ssh_tunnel.exe mcp`). Своей логики тут нет — только обёртка над API ядра
// (docs/API.md): адрес и ключ берутся из api.json, список команд — из
// самоописания GET /api, так что новое в API сразу появляется и здесь.
package apicli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"sshtunnel/internal/config"
)

// Client — связь с API работающей программы.
type Client struct {
	URL   string
	Token string
	HTTP  *http.Client
}

// errNotRunning — api.json нет: программа не запущена (или запущена другим
// пользователем).
var errNotRunning = errors.New("программа не запущена: нет " + discoveryPath() +
	" (запусти ssh_tunnel — окно или службу)")

func discoveryPath() string { return filepath.Join(config.Dir(), "api.json") }

// Discover читает api.json работающей программы.
func Discover() (*Client, error) {
	b, err := os.ReadFile(discoveryPath())
	if err != nil {
		return nil, errNotRunning
	}
	var d struct {
		URL   string `json:"url"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(b, &d); err != nil || d.URL == "" {
		return nil, fmt.Errorf("не разобрал %s: %v", discoveryPath(), err)
	}
	return &Client{URL: strings.TrimRight(d.URL, "/"), Token: d.Token,
		HTTP: &http.Client{Timeout: 3 * time.Minute}}, nil
}

// Do — запрос к API. body: nil — без тела, []byte — как есть, иначе JSON.
// Ответ — тело и его тип.
func (c *Client) Do(method, path string, query url.Values, body any) ([]byte, string, error) {
	u := c.URL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var rd io.Reader
	ct := ""
	switch b := body.(type) {
	case nil:
	case []byte:
		rd, ct = bytes.NewReader(b), "application/octet-stream"
	default:
		j, err := json.Marshal(b)
		if err != nil {
			return nil, "", err
		}
		rd, ct = bytes.NewReader(j), "application/json"
	}
	req, err := http.NewRequest(method, u, rd)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("программа не отвечает (%s): %v", c.URL, err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, "", err
	}
	if resp.StatusCode >= 400 {
		msg := strings.TrimSpace(string(out))
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(out, &e) == nil && e.Error != "" {
			msg = e.Error
		}
		return out, resp.Header.Get("Content-Type"), fmt.Errorf("%s %s: %d %s", method, path, resp.StatusCode, msg)
	}
	return out, resp.Header.Get("Content-Type"), nil
}

// JSON — запрос с разбором ответа в v. Поле "error" в ответе — ошибка.
func (c *Client) JSON(method, path string, body, v any) error {
	b, _, err := c.Do(method, path, nil, body)
	if err != nil {
		return err
	}
	if e := apiError(b); e != "" {
		return errors.New(e)
	}
	if v == nil {
		return nil
	}
	return json.Unmarshal(b, v)
}

// apiError — текст поля "error" ответа ("" — ошибки нет).
func apiError(b []byte) string {
	var e struct {
		Error any `json:"error"`
	}
	if json.Unmarshal(b, &e) != nil || e.Error == nil {
		return ""
	}
	if s, ok := e.Error.(string); ok {
		return s
	}
	return fmt.Sprint(e.Error)
}

// Route — строка самоописания GET /api.
type Route struct {
	Path    string `json:"path"`
	Methods string `json:"methods"`
	Desc    string `json:"desc"`
	Confirm string `json:"confirm,omitempty"`
}

// Has — умеет ли адрес метод.
func (r Route) Has(method string) bool {
	for _, m := range strings.Split(r.Methods, ",") {
		if strings.TrimSpace(m) == method {
			return true
		}
	}
	return false
}

// Routes — все адреса API работающей программы, по пути.
func (c *Client) Routes() ([]Route, error) {
	var idx struct {
		Routes []Route `json:"routes"`
	}
	if err := c.JSON("GET", "/api", nil, &idx); err != nil {
		return nil, err
	}
	sort.Slice(idx.Routes, func(i, j int) bool { return idx.Routes[i].Path < idx.Routes[j].Path })
	return idx.Routes, nil
}

// streaming — адреса-потоки: ни команда, ни MCP их не ждут до конца.
func streaming(p string) bool {
	return p == "/events" || strings.HasSuffix(p, "/out")
}
