package core

// Круглая кнопка сама возвращается, когда сервер вернулся (ТЗ 20), — если
// погасла из-за потери сервера и человек её с тех пор не трогал.

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

func (d *localDriver) serverIs(up bool) {
	d.smu.Lock()
	defer d.smu.Unlock()
	if up {
		d.serverDown, d.bypassErr, d.gone = nil, nil, false
		return
	}
	err := errors.New("сервер не отвечает")
	d.bypassErr, d.gone = err, true
}

func saving(t *testing.T, d *localDriver) (*Machine, *atomic.Int32) {
	t.Helper()
	var saves atomic.Int32
	m := NewMachine(d, Options{Save: func(Want) { saves.Add(1) }})
	t.Cleanup(m.Shutdown)
	return m, &saves
}

// Сервера не было при включении: круглая погасла с ошибкой, сеть работает.
// Сервер появился — круглая включается сама, выбор человека не
// перезаписывается (Save не зовётся).
func TestКруглаяВозвращаетсяПослеСтартаБезСервера(t *testing.T) {
	down := errors.New("сервер не отвечает")
	d := &localDriver{fakeDriver: fakeDriver{meshAvail: true}, serverDown: down, bypassErr: down}
	m, saves := saving(t, d)
	m.Set(ptr(true), ptr(true))
	st := waitIdle(t, m)
	if st.Tunnel.Phase != PhaseError || st.Mesh.Phase != PhaseOn {
		t.Fatalf("без сервера: %+v", st)
	}
	if st := waitIdle(t, m); st.Tunnel.Phase != PhaseError {
		t.Fatal("круглая включилась, хотя сервера нет")
	}

	d.serverIs(true)
	m.State() // платформа спрашивает State на каждую смену связи
	st = waitIdle(t, m)
	if st.Tunnel.Phase != PhaseOn || st.Mode != ModeBypassMesh {
		t.Fatalf("сервер вернулся, а круглая: %+v", st)
	}
	if saves.Load() != 1 {
		t.Fatalf("Save позвали %d раз — возврат записан как нажатие человека", saves.Load())
	}
}

// Сервер пропал на ходу (ServerLost) и вернулся: круглая включается сама.
func TestКруглаяВозвращаетсяПослеПотериСервера(t *testing.T) {
	d := &localDriver{fakeDriver: fakeDriver{meshAvail: true}}
	m, _ := saving(t, d)
	m.Set(ptr(true), ptr(true))
	waitIdle(t, m)
	d.serverIs(false)
	m.ServerLost(errors.New("запасной не поднялся"))
	if st := waitIdle(t, m); st.Tunnel.Phase != PhaseError || st.Mode != ModeMesh {
		t.Fatalf("после потери: %+v", st)
	}
	d.serverIs(true)
	m.ServerBack() // и явный сигнал платформы работает
	if st := waitIdle(t, m); st.Tunnel.Phase != PhaseOn || st.Mode != ModeBypassMesh {
		t.Fatalf("после возврата: %+v", st)
	}
	// Второй раз то же самое — возврат не одноразовый.
	d.serverIs(false)
	m.ServerLost(errors.New("снова пропал"))
	waitIdle(t, m)
	d.serverIs(true)
	m.State()
	if st := waitIdle(t, m); st.Tunnel.Phase != PhaseOn {
		t.Fatalf("второй возврат: %+v", st)
	}
}

// Человек выключил круглую, пока сервера не было, — сервер вернулся, кнопка
// осталась выключенной.
func TestКруглаяНеПеребиваетНажатие(t *testing.T) {
	d := &localDriver{fakeDriver: fakeDriver{meshAvail: true}}
	m, _ := saving(t, d)
	m.Set(ptr(true), ptr(true))
	waitIdle(t, m)
	d.serverIs(false)
	m.ServerLost(errors.New("пропал"))
	waitIdle(t, m)
	m.Set(ptr(false), nil)
	waitIdle(t, m)
	d.serverIs(true)
	m.ServerBack()
	m.State()
	if st := waitIdle(t, m); st.Tunnel.Want || st.Tunnel.Phase != PhaseOff {
		t.Fatalf("нажатие человека перебито: %+v", st.Tunnel)
	}
}

// Человек сам включал круглую при мёртвом сервере (не вышло) — это его
// решение: возврат сервера кнопку не трогает, нажмёт ещё раз сам.
func TestКруглаяПослеНеудачногоНажатия(t *testing.T) {
	down := errors.New("сервер не отвечает")
	d := &localDriver{fakeDriver: fakeDriver{meshAvail: true}, serverDown: down, bypassErr: down}
	m, _ := saving(t, d)
	m.Set(nil, ptr(true))
	waitIdle(t, m)
	m.Set(ptr(true), nil)
	if st := waitIdle(t, m); st.Tunnel.Phase != PhaseError {
		t.Fatalf("%+v", st.Tunnel)
	}
	d.serverIs(true)
	m.State()
	if st := waitIdle(t, m); st.Tunnel.Want {
		t.Fatalf("круглая включилась без человека: %+v", st.Tunnel)
	}
}

// Всё выключили (и сеть тоже) — возвращать нечего, даже если сервер потом
// найдётся.
func TestКруглаяНеВозвращаетсяПослеВыключенияВсего(t *testing.T) {
	d := &localDriver{fakeDriver: fakeDriver{meshAvail: true}}
	m, _ := saving(t, d)
	m.Set(ptr(true), ptr(true))
	waitIdle(t, m)
	d.serverIs(false)
	m.ServerLost(errors.New("пропал"))
	waitIdle(t, m)
	m.Set(nil, ptr(false))
	waitIdle(t, m)
	d.serverIs(true)
	m.Set(nil, ptr(true))
	waitIdle(t, m)
	m.State()
	if st := waitIdle(t, m); st.Tunnel.Want {
		t.Fatalf("круглая вернулась после «выключить всё»: %+v", st.Tunnel)
	}
}

// Нажатие человека и возврат сервера одновременно: побеждает человек, как
// бы ни легли события (гонять с -count=20 и -race).
func TestКруглаяГонкаНажатияИВозврата(t *testing.T) {
	for i := 0; i < 20; i++ {
		d := &localDriver{fakeDriver: fakeDriver{meshAvail: true}}
		m := NewMachine(d, Options{})
		m.Set(ptr(true), ptr(true))
		waitIdle(t, m)
		d.serverIs(false)
		m.ServerLost(errors.New("пропал"))
		waitIdle(t, m)
		d.serverIs(true)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				m.State()
				m.ServerBack()
			}
		}()
		go func() {
			defer wg.Done()
			m.Set(ptr(false), nil)
		}()
		wg.Wait()
		for j := 0; j < 5; j++ {
			m.State()
			m.ServerBack()
		}
		st := waitIdle(t, m)
		if st.Tunnel.Want {
			t.Fatalf("проход %d: человек выключил, а круглая включена: %+v", i, st.Tunnel)
		}
		m.Shutdown()
	}
}
