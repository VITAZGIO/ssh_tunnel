package app

// Цепочка выхода (ТЗ 8) со стороны приложения: из профилей собрать серверы
// цепочки для туннеля и показать ядру, как она живёт. Само исполнение — в
// tunnel/exit.go; «куда» по-прежнему решает core.Decide: цепочка меняет не
// «куда», а то, через какой сервер выходит «через сервер».

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"sshtunnel/internal/config"
	"sshtunnel/internal/core"
	"sshtunnel/internal/tunnel"
)

// exitHops — серверы цепочки профиля p для туннеля.
func exitHops(cfg config.Config, p config.Profile) []tunnel.Hop {
	var out []tunnel.Hop
	for _, h := range cfg.ExitChain(p) {
		need, found := strings.CutPrefix(h.ID, config.ExitNeedPrefix)
		if !found {
			need = ""
		}
		out = append(out, tunnel.Hop{
			Name: h.Name, Host: h.Host, SSHPort: h.SSHPort, User: h.User,
			KeyPath: h.KeyPath, Cipher: h.Cipher, UDPRelay: h.UDPRelayEnabled, Need: need,
		})
	}
	return out
}

// exitChainKey — всё в цепочке, ради чего надо переподключаться.
func exitChainKey(cfg config.Config, p config.Profile) string {
	var b strings.Builder
	for _, h := range exitHops(cfg, p) {
		fmt.Fprintf(&b, "%s|%s|%d|%s|%s|%s|%v|%s;", h.Name, h.Host, h.SSHPort, h.User, h.KeyPath, h.Cipher, h.UDPRelay, h.Need)
	}
	return b.String()
}

// ExitPath — цепочка для экрана (core.ExitReporter); nil — её нет.
func (d *driver) Exit() *core.ExitPath {
	a := d.app()
	a.mu.Lock()
	tun := a.tun
	a.mu.Unlock()
	if tun == nil {
		return nil
	}
	st := tun.ExitStatus()
	if len(st.Path) == 0 {
		return nil
	}
	out := &core.ExitPath{Path: st.Path, State: core.ExitConnecting}
	switch {
	case st.Up:
		out.State = core.ExitUp
	case st.Err != nil:
		out.State = core.ExitDown
		out.Error = st.Err.Error()
		var ed *tunnel.ExitDownError
		if errors.As(st.Err, &ed) {
			out.Hop = ed.Name
			out.ErrorKind = errorKind(ed.Err)
		}
	}
	return out
}

// SetExitVia задаёт «Выход через» у сервера id: via — id других серверов по
// порядку (пусто — без цепочки). Подключённое переподключается, если это
// активный сервер. Это правка человека на устройстве: она новее цепочки из
// панели, если та была (ТЗ 16), и панели серверов её увидят. Звено из
// панели без профиля ("need:адрес:порт") можно оставить как есть.
func (a *App) SetExitVia(id string, via []string) (string, error) {
	a.exitMu.Lock()
	defer a.exitMu.Unlock()
	cfg := a.Config()
	p, ok := cfg.ProfileByID(id)
	if !ok {
		return "", errors.New("сервер не найден")
	}
	if len(via) > config.MaxExitHops {
		return "", fmt.Errorf("в цепочке может быть не больше %d серверов после этого", config.MaxExitHops)
	}
	seen := map[string]bool{id: true}
	for _, v := range via {
		if v == id {
			return "", errors.New("сервер не может выходить в интернет через самого себя")
		}
		if seen[v] {
			return "", errors.New("один сервер дважды в цепочке")
		}
		seen[v] = true
		if strings.HasPrefix(v, config.ExitNeedPrefix) {
			continue
		}
		hop, ok := cfg.ProfileByID(v)
		if !ok {
			return "", fmt.Errorf("сервер %q не найден", v)
		}
		if hop.Host == "" {
			return "", fmt.Errorf("у сервера «%s» не указан адрес", hop.Name)
		}
	}
	p.ExitVia = append([]string(nil), via...)
	config.TouchExit(&p, time.Now().Unix())
	cfg.SetProfile(p)
	return a.SetConfig(cfg)
}
