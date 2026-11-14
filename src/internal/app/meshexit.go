package app

// Цепочка выхода из панели сервера (ТЗ 16) со стороны приложения: панели
// серверов видят, для каких серверов у устройства есть профили и какие у
// них цепочки (mesh.Client.SetProfiles), и могут прислать свою цепочку —
// она применяется так, как если бы человек сам выставил «Выход через»,
// если она новее того, что есть (config.ApplyExitOrder).

import (
	"strings"
	"time"

	"sshtunnel/internal/config"
	"sshtunnel/internal/mesh"
)

// meshProfiles — профили для панелей серверов: ключ «адрес:порт», имя,
// подключено ли через него и его цепочка ключами звеньев.
func meshProfiles(cfg config.Config) []mesh.ProfileRef {
	var out []mesh.ProfileRef
	for _, p := range cfg.Profiles {
		key := config.ServerKey(p)
		if key == "" {
			continue
		}
		r := mesh.ProfileRef{Key: key, Name: p.Name, Active: p.ID == cfg.ActiveProfile, T: p.ExitT, By: p.ExitBy}
		for _, id := range p.ExitVia {
			if k, need := strings.CutPrefix(id, config.ExitNeedPrefix); need {
				r.Via = append(r.Via, k)
				continue
			}
			hop, _ := cfg.ProfileByID(id)
			r.Via = append(r.Via, config.ServerKey(hop)) // "" — профиль удалён
		}
		out = append(out, r)
	}
	return out
}

// pushMeshProfiles рассказывает серверам сети устройств свежие профили.
func (a *App) pushMeshProfiles(cfg config.Config) {
	if c := a.meshHost.Client(); c != nil {
		c.SetProfiles(meshProfiles(cfg))
	}
}

// onMeshExit — панель сервера прислала цепочку выхода.
func (a *App) onMeshExit(o mesh.ExitOrder) {
	a.exitMu.Lock()
	defer a.exitMu.Unlock()
	cfg := a.Config()
	changed, note, err := cfg.ApplyExitOrder(config.ExitOrder{Entry: o.Entry, Via: o.Via, T: o.T, By: o.By})
	if err != nil {
		a.Bus.Warnf("Сеть устройств: %v", err)
		return
	}
	if !changed {
		return
	}
	path := o.Entry
	for _, v := range o.Via {
		path += " → " + v
	}
	if len(o.Via) == 0 {
		a.Bus.Infof("Сеть устройств: панель сервера %s убрала цепочку выхода у %s", o.By, o.Entry)
	} else {
		a.Bus.Infof("Сеть устройств: панель сервера %s задала цепочку выхода %s", o.By, path)
	}
	if note != "" {
		a.Bus.Warnf("Цепочка выхода: %s", note)
	}
	if _, err := a.SetConfig(cfg); err != nil {
		a.Bus.Warnf("Цепочка выхода из панели не сохранилась: %v", err)
	}
}

// SetConfigEdited — настройки целиком от экрана (POST /api/config). Правка
// цепочки выхода получает время (config.StampExitEdits), под тем же замком,
// что и цепочка из панели.
func (a *App) SetConfigEdited(cfg config.Config) (string, error) {
	a.exitMu.Lock()
	defer a.exitMu.Unlock()
	stale := config.StampExitEdits(a.Config(), &cfg, time.Now().Unix())
	note, err := a.SetConfig(cfg)
	if stale != "" {
		if note != "" {
			note += "; "
		}
		note += stale
	}
	return note, err
}
