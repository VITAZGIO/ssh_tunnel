package app

import (
	"errors"
	"time"

	"sshtunnel/internal/core"
	"sshtunnel/internal/tunnel"
)

// Сколько и как часто StartOnLaunch пробует подключиться. Переменные, а не
// константы, — чтобы тест не ждал минутами.
var (
	launchRetryFor   = 3 * time.Minute
	launchRetryEvery = 5 * time.Second
)

// StartOnLaunch поднимает туннель сразу после запуска программы (галочка
// «Подключаться сразу при запуске»).
//
// При автозапуске программа стартует вместе со входом в систему, и сети в
// этот момент часто ещё нет: Wi-Fi подключается через несколько секунд после
// рабочего стола. Одна попытка тогда заканчивалась «автозапуск не удался», и
// туннель так и оставался выключенным до ручного «Подключить». Поэтому
// пробуем ещё несколько минут.
//
// Сдаёмся сразу, если дело в ключе или имени пользователя (ожиданием это не
// чинится), и тихо выходим, если за это время человек сам нажал «Подключить»
// или «Отключить»: решение уже за ним.
func (a *App) StartOnLaunch() { a.StartOnLaunchWithin(launchRetryFor) }

// StartOnLaunchWithin — то же, но с другим сроком ожидания сети; 0 — ждать
// сколько угодно. Так работает служба Windows: компьютер, включённый
// удалённо, должен появиться в сети устройств, когда бы ни поднялась его
// сеть, — сдаться через три минуты значило бы потерять его до перезагрузки.
func (a *App) StartOnLaunchWithin(limit time.Duration) {
	deadline := time.Now().Add(limit)
	human := a.machine.HumanSeq()
	for attempt := 1; ; attempt++ {
		err := a.StartLaunchMode()
		if err == nil {
			if attempt > 1 {
				a.Bus.Infof("Автозапуск: подключились с %d-й попытки", attempt)
			}
			return
		}
		if a.machine.HumanSeq() != human {
			return // человек уже нажал кнопку сам — решение за ним
		}
		if tunnel.IsAuthError(err) || errors.Is(err, errNoMesh) || (limit > 0 && time.Now().After(deadline)) {
			a.Bus.Errorf("Автозапуск не удался: %v", err)
			return
		}
		if attempt == 1 {
			a.Bus.Warnf("Автозапуск: пока не подключиться (%v) — возможно, сеть ещё не поднялась, пробую снова", err)
		}
		time.Sleep(launchRetryEvery)
		if a.machine.HumanSeq() != human {
			return
		}
	}
}

// StartLaunchMode включает кнопки так, как выбрано в настройках запуска:
// «Туннель при запуске» и «Сеть устройств при запуске» — два отдельных
// выбора, «как при закрытии» восстанавливает обе кнопки. Только сеть нужна
// домашним серверам: их видно с других устройств, а свой интернет идёт
// напрямую. Без сети устройств у сервера «только сеть» не подключает ничего:
// пустить весь трафик через VPS вместо этого было бы ровно тем, чего не
// просили.
//
// Сюда приходят, когда запускаться уже решено (config.ConnectOnStart), — у
// конфига старого формата это решила прежняя галочка, поэтому legacy здесь
// «да».
func (a *App) StartLaunchMode() error {
	cfg := a.Config()
	w := core.Want{Tunnel: cfg.BypassOnStart(true), Mesh: cfg.MeshOnStart(true)}
	switch {
	case !w.Tunnel && !w.Mesh:
		return nil
	case !w.Tunnel && !a.meshConfigured():
		return errNoMesh
	}
	a.machine.Restore(w)
	return a.settle(&w.Tunnel, &w.Mesh)
}
