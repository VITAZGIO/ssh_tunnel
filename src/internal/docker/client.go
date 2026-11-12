package docker

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"syscall"
	"time"
)

// Client — Docker Engine API через его сокет (без программы docker).
type Client struct {
	// Socket — путь к сокету (для сообщений).
	Socket string
	hc     *http.Client
	base   string
}

// DefaultSocket — где Docker держит сокет; DOCKER_HOST=unix:///… — другой.
func DefaultSocket() string {
	if h := os.Getenv("DOCKER_HOST"); strings.HasPrefix(h, "unix://") {
		return strings.TrimPrefix(h, "unix://")
	}
	return "/var/run/docker.sock"
}

// NewClient — клиент сокета path.
func NewClient(path string) *Client {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		},
		MaxIdleConns: 2,
	}
	return &Client{Socket: path, hc: &http.Client{Transport: tr}, base: "http://docker"}
}

// newHTTPClient — клиент обычного HTTP-адреса (для тестов с поддельным API).
func newHTTPClient(base string) *Client {
	return &Client{Socket: base, hc: &http.Client{}, base: strings.TrimSuffix(base, "/")}
}

// ErrNoDocker и ErrNoAccess — понятные причины, почему списка нет.
var (
	ErrNoDocker = errors.New("Docker не найден")
	ErrNoAccess = errors.New("нет прав на сокет Docker")
)

func (c *Client) wrap(err error) error {
	switch {
	case errors.Is(err, os.ErrNotExist), errors.Is(err, syscall.ENOENT), errors.Is(err, syscall.ECONNREFUSED):
		return fmt.Errorf("%w (нет сокета %s или Docker не запущен)", ErrNoDocker, c.Socket)
	case errors.Is(err, os.ErrPermission), errors.Is(err, syscall.EACCES):
		return fmt.Errorf("%w %s", ErrNoAccess, c.Socket)
	}
	if strings.Contains(err.Error(), "no such file") || strings.Contains(err.Error(), "refused") {
		return fmt.Errorf("%w (нет сокета %s или Docker не запущен)", ErrNoDocker, c.Socket)
	}
	if strings.Contains(err.Error(), "permission denied") {
		return fmt.Errorf("%w %s", ErrNoAccess, c.Socket)
	}
	return err
}

func (c *Client) get(ctx context.Context, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", c.base+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, c.wrap(err)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		return nil, fmt.Errorf("Docker API %s: %s %s", path, resp.Status, strings.TrimSpace(string(b)))
	}
	return resp, nil
}

// apiContainer — ответ GET /containers/json (нужные поля).
type apiContainer struct {
	ID         string            `json:"Id"`
	Names      []string          `json:"Names"`
	Image      string            `json:"Image"`
	State      string            `json:"State"`
	Labels     map[string]string `json:"Labels"`
	HostConfig struct {
		NetworkMode string `json:"NetworkMode"`
	} `json:"HostConfig"`
	NetworkSettings struct {
		Networks map[string]struct {
			IPAddress         string `json:"IPAddress"`
			GlobalIPv6Address string `json:"GlobalIPv6Address"`
		} `json:"Networks"`
	} `json:"NetworkSettings"`
}

// List — все контейнеры (и остановленные: их выбор тоже показывается).
func (c *Client) List(ctx context.Context) ([]Container, error) {
	resp, err := c.get(ctx, "/containers/json?all=1")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var raw []apiContainer
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("Docker API: %w", err)
	}
	return convert(raw), nil
}

func convert(raw []apiContainer) []Container {
	out := make([]Container, 0, len(raw))
	for _, r := range raw {
		c := Container{ID: r.ID, Image: r.Image, State: r.State, Labels: r.Labels}
		if len(r.Names) > 0 {
			c.Name = strings.TrimPrefix(r.Names[0], "/")
		}
		c.Project, c.Service = r.Labels[labelProject], r.Labels[labelService]
		mode := r.HostConfig.NetworkMode
		switch {
		case mode == "host" || mode == "none":
			c.NetMode = mode
		case strings.HasPrefix(mode, "container:"):
			c.NetMode, c.Parent = "container", strings.TrimPrefix(mode, "container:")
		default:
			c.NetMode = "bridge"
		}
		for name, n := range r.NetworkSettings.Networks {
			nw := Network{Name: name}
			if a, err := netip.ParseAddr(n.IPAddress); err == nil {
				nw.IP = a
			}
			if a, err := netip.ParseAddr(n.GlobalIPv6Address); err == nil {
				nw.IP6 = a
			}
			c.Networks = append(c.Networks, nw)
		}
		sortNetworks(c.Networks)
		out = append(out, c)
	}
	// Короткий ID у «сети другого контейнера» Docker пишет полным ID —
	// сопоставляется в Resolve по ID или имени.
	return out
}

func sortNetworks(ns []Network) {
	for i := 1; i < len(ns); i++ {
		for j := i; j > 0 && ns[j].Name < ns[j-1].Name; j-- {
			ns[j], ns[j-1] = ns[j-1], ns[j]
		}
	}
}

// Watch зовёт changed на каждое событие контейнера или сети, которое может
// сменить адреса (start, die, destroy, rename, connect, disconnect). Ждёт,
// пока поток не оборвётся или ctx не закончится.
func (c *Client) Watch(ctx context.Context, changed func()) error {
	f := url.QueryEscape(`{"type":["container","network"]}`)
	since := fmt.Sprint(time.Now().Unix())
	resp, err := c.get(ctx, "/events?since="+since+"&filters="+f)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	dec := json.NewDecoder(bufio.NewReader(resp.Body))
	for {
		var ev struct {
			Type   string `json:"Type"`
			Action string `json:"Action"`
		}
		if err := dec.Decode(&ev); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		if interesting(ev.Type, ev.Action) {
			changed()
		}
	}
}

func interesting(typ, action string) bool {
	action, _, _ = strings.Cut(action, ":") // «exec_start: sh» и подобное
	switch typ {
	case "container":
		switch action {
		case "start", "restart", "die", "stop", "kill", "destroy", "create", "rename", "pause", "unpause", "update":
			return true
		}
	case "network":
		return action == "connect" || action == "disconnect"
	}
	return false
}
