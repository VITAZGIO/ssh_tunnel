package mobile

// Другие серверы телефона (ТЗ 20): дополнительные серверы сети устройств и
// цепочка выхода — то же, что на ПК (ТЗ 12 и ТЗ 8), и тем же общим кодом.
//
// Список серверов хранит приложение (Settings.kt). Оно отдаёт его сюда
// целиком (SetProfiles) — в том же JSON, что профиль на ПК (config.Profile:
// id, name, host, sshPort, user, keyPath, meshEnabled, meshKey, meshOnlyThis,
// exitVia), — а выбирает, кого брать, общий код: config.Config.MeshServers и
// config.Config.ExitChain. Так правило «тот же ключ сети — тот же сервер
// сети» и «Выход через» на телефоне ровно такие же, как на ПК.
//
// Дополнительный сервер — лёгкое соединение (tunnel.MeshLinks): SSH
// keepalive раз в 60 с и «тихий» пинг meshd раз в 60 с, без пингов клиента и
// проверки NAT. По оценке ТЗ 12 это ≈ 2 обмена в минуту к ≈ 9,4 у основного,
// то есть два сервера ≈ 1,2× по батарее. Живьём не мерилось.
//
// Цепочка: туннель ведёт «через сервер» только через последний сервер
// цепочки (tunnel.Config.Exit, Tunnel.Dial), запасного пути нет. Имена
// с подставными адресами открываются по имени на последнем сервере — значит
// и DNS идёт через него; UDP — через его ретранслятор. Сервер цепочки лёг —
// интернета через VPN нет вовсе: интерфейс VPN остаётся, пакеты ловятся, а
// соединения получают отказ. Утечки мимо цепочки нет, потому что обходного
// пути в коде нет.

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"sshtunnel/internal/config"
	core2 "sshtunnel/internal/core"
	"sshtunnel/internal/mesh"
	"sshtunnel/internal/tunnel"
)

// SetProfiles — все серверы приложения (JSON-массив профилей, см. выше) и id
// активного. udpRelay — галочка «ретранслятор UDP» (на телефоне общая для
// всех серверов). Зовётся до StartCore (после Configure); на поднятом
// туннеле дополнительные серверы меняются сразу, а цепочка — со следующим
// подключением (её меняет переподключение, как на ПК).
func (t *Tunnel) SetProfiles(profilesJSON, activeID string, udpRelay bool) error {
	var list []config.Profile
	if s := strings.TrimSpace(profilesJSON); s != "" {
		if err := json.Unmarshal([]byte(s), &list); err != nil {
			return fmt.Errorf("список серверов не разобран: %v", err)
		}
	}
	cfg := config.Config{Profiles: list}
	active, ok := cfg.ProfileByID(activeID)
	if !ok {
		t.mu.Lock()
		t.extras, t.exit = nil, nil
		t.mu.Unlock()
		t.applyExtras()
		if activeID != "" && len(list) > 0 {
			return errors.New("активный сервер не найден в списке")
		}
		return nil
	}
	if len(active.ExitVia) > config.MaxExitHops {
		return fmt.Errorf("в цепочке может быть не больше %d серверов после этого", config.MaxExitHops)
	}
	var extras []config.Profile
	if active.MeshEnabled && active.MeshKey != "" {
		extras = cfg.MeshServers(active)
	}
	var hops []tunnel.Hop
	for _, h := range cfg.ExitChain(active) {
		if h.ID == active.ID {
			continue // сам себе не выход
		}
		hops = append(hops, tunnel.Hop{Name: h.Name, Host: h.Host, SSHPort: h.SSHPort, User: h.User,
			KeyPath: h.KeyPath, Cipher: h.Cipher, UDPRelay: udpRelay})
	}
	t.mu.Lock()
	t.extras, t.exit = extras, hops
	t.mu.Unlock()
	t.applyExtras()
	return nil
}

// meshServersLocked — дополнительные серверы для MeshLinks. Под t.mu.
func (t *Tunnel) meshServersLocked() []tunnel.MeshServer {
	out := make([]tunnel.MeshServer, 0, len(t.extras))
	for _, q := range t.extras {
		out = append(out, tunnel.MeshServer{
			Key: q.ID, Name: q.Name, Host: q.Host, SSHPort: q.SSHPort, User: q.User, KeyPath: q.KeyPath,
			KnownHostsPath: t.cfg.KnownHostsPath, IPVersion: q.IPVersion, Cipher: q.Cipher,
			ProtectSocket: t.protect,
		})
	}
	return out
}

// startMesh поднимает клиента сети устройств и его дополнительные серверы.
func (t *Tunnel) startMesh(mc *mesh.Config) {
	t.meshHost.Start(*mc)
	t.applyExtras()
}

// stopMesh гасит дополнительные серверы и клиента (как App.stopMesh на ПК).
func (t *Tunnel) stopMesh() {
	t.meshLinks.Stop()
	t.meshHost.Stop()
}

// applyExtras — дополнительные серверы: держать, пока клиент сети нужен и
// работает, иначе — ни одного соединения.
func (t *Tunnel) applyExtras() {
	// Running — под замком MeshHost, а его журнал берёт t.mu: спрашивать до
	// t.mu, иначе два потока могут ждать друг друга.
	running := t.meshHost.Running()
	t.mu.Lock()
	var list []tunnel.MeshServer
	if running && t.meshWantedLocked() != nil {
		list = t.meshServersLocked()
	}
	t.mu.Unlock()
	t.meshLinks.Set(list)
}

// MeshServersJSON — какие дополнительные серверы сети устройств сейчас
// держатся (id профилей), для проверки и экрана: ["id", …].
func (t *Tunnel) MeshServersJSON() string {
	keys := t.meshLinks.Keys()
	if keys == nil {
		keys = []string{}
	}
	b, _ := json.Marshal(keys)
	return string(b)
}

// ExitJSON — цепочка выхода для экрана (core.ExitPath: state up / connecting
// / down, path, hop, error, errorKind) или пусто, если цепочки нет или туннель
// выключен.
func (t *Tunnel) ExitJSON() string {
	t.mu.Lock()
	tun := t.core
	t.mu.Unlock()
	if tun == nil {
		return ""
	}
	st := tun.ExitStatus()
	if len(st.Path) == 0 {
		return ""
	}
	out := core2.ExitPath{Path: st.Path, State: core2.ExitConnecting}
	switch {
	case st.Up:
		out.State = core2.ExitUp
	case st.Err != nil:
		out.State = core2.ExitDown
		out.Error = st.Err.Error()
		var ed *tunnel.ExitDownError
		if errors.As(st.Err, &ed) {
			out.Hop = ed.Name
			out.ErrorKind = errorKind(ed.Err)
		}
	}
	b, _ := json.Marshal(out)
	return string(b)
}
