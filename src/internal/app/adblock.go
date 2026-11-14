package app

import (
	"errors"

	"sshtunnel/internal/adblock"
	"sshtunnel/internal/config"
)

// AdBlocker — блокировка рекламы и слежки по DNS. Её умеет только слой VPN:
// в режиме прокси имена разрешают сами программы, перехватывать нечего.
type AdBlocker interface {
	// SetAdBlock применяет настройки (вкл/выкл, исключения) и перечитывает
	// сохранённый список. Работает и на поднятом адаптере.
	SetAdBlock(cfg config.Config)
	// UpdateAdBlockLists скачивает списки и сохраняет их; возвращает число имён.
	UpdateAdBlockLists(sources string) (int, error)
}

// adBlocker — блокировка слоя VPN, к исключениям которой добавлены
// исключения общего списка сервера (ТЗ 15, internal/adblock).
func (a *App) adBlocker() AdBlocker {
	ab := a.rawAdBlocker()
	if ab == nil {
		return nil
	}
	return serverList{ab}
}

func (a *App) rawAdBlocker() AdBlocker {
	a.mu.Lock()
	defer a.mu.Unlock()
	ab, _ := a.net.(AdBlocker)
	return ab
}

// serverList — блокировка, которая знает о списке с сервера сети: его
// исключения действуют вместе со своими, а свои источники не качаются,
// пока список ведёт сервер.
type serverList struct{ AdBlocker }

func (s serverList) SetAdBlock(cfg config.Config) { s.AdBlocker.SetAdBlock(adblock.WithServer(cfg)) }

func (s serverList) UpdateAdBlockLists(sources string) (int, error) {
	if on, srv := adblock.UsingServer(); on {
		return 0, errors.New("список ведёт сервер сети «" + srv + "» — источники меняются в его панели; чтобы качать свои, сними «Брать список с сервера»")
	}
	return s.AdBlocker.UpdateAdBlockLists(sources)
}

// attachAdBlock — слой VPN подключён: применить настройки и дать списку с
// сервера способ перечитать блокировку, когда он обновится.
func (a *App) attachAdBlock(ab AdBlocker, cfg config.Config) {
	serverList{ab}.SetAdBlock(cfg)
	adblock.SetReload(func() {
		if x := a.adBlocker(); x != nil {
			x.SetAdBlock(a.Config())
		}
	})
}

// AdBlockAvailable — есть ли блокировка рекламы в этой версии программы.
func (a *App) AdBlockAvailable() bool { return a.adBlocker() != nil }

// UpdateAdBlockLists скачивает списки по кнопке в настройках и сразу
// применяет их.
func (a *App) UpdateAdBlockLists(sources string) (int, error) {
	ab := a.adBlocker()
	if ab == nil {
		return 0, errors.New("блокировка рекламы есть только в режиме VPN")
	}
	n, err := ab.UpdateAdBlockLists(sources)
	if err != nil {
		return 0, err
	}
	ab.SetAdBlock(a.Config())
	return n, nil
}

// AdBlocked — сколько запросов имён заблокировано с подключения; 0, если
// блокировки в этой версии нет.
func (a *App) AdBlocked() int {
	if c, ok := a.rawAdBlocker().(interface{ AdBlocked() int }); ok {
		return c.AdBlocked()
	}
	return 0
}

// AdStatsJSON — статистика плагина «Блокировка рекламы» (JSON); "" — нет
// блокировки в этой версии.
func (a *App) AdStatsJSON() string {
	if c, ok := a.rawAdBlocker().(interface{ AdStatsJSON() string }); ok {
		return c.AdStatsJSON()
	}
	return ""
}

// ConfigLayer — слой VPN, которому нужны общие настройки программы на ходу
// (сейчас — куда вести устройства точки доступа). Зовётся при подключении
// слоя и после каждого сохранения настроек.
type ConfigLayer interface {
	ApplyConfig(cfg config.Config)
}
