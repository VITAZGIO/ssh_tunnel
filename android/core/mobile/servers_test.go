package mobile

// Другие серверы телефона (ТЗ 20): дополнительные серверы сети устройств,
// цепочка выхода, лимитная связь.

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
)

// profilesJSON — список серверов так, как его отдаёт Settings.kt.
func profilesJSON(t *testing.T, list ...map[string]any) string {
	t.Helper()
	b, err := json.Marshal(list)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func srv(id, host string, extra map[string]any) map[string]any {
	p := map[string]any{"id": id, "name": "Сервер " + id, "host": host, "sshPort": 22, "user": "u",
		"keyPath": "/keys/" + id}
	for k, v := range extra {
		p[k] = v
	}
	return p
}

func extrasOf(t *Tunnel) []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []string
	for _, p := range t.extras {
		out = append(out, p.ID)
	}
	return out
}

// Те же правила, что на ПК: тот же ключ сети и включённая сеть — сервер сети;
// одинаковый адрес не дублируется; MeshOnlyThis — никого.
func TestSetProfilesДополнительныеСерверы(t *testing.T) {
	mesh := func(key string) map[string]any { return map[string]any{"meshEnabled": true, "meshKey": key} }
	tun := NewTunnel()
	js := profilesJSON(t,
		srv("a", "a.example", mesh("K")),
		srv("b", "b.example", mesh("K")),
		srv("c", "c.example", mesh("other")),                       // другая сеть
		srv("d", "d.example", map[string]any{"meshKey": "K"}),      // сеть выключена
		srv("e", "A.example", mesh("K")),                           // тот же адрес, что у a
		srv("f", "b.example", mesh("K")),                           // тот же адрес, что у b
		srv("g", "g.example", map[string]any{"meshEnabled": true}), // без ключа
		srv("h", "h.example", mesh("K")),
	)
	if err := tun.SetProfiles(js, "a", false); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(extrasOf(tun), ","); got != "b,h" {
		t.Fatalf("дополнительные серверы %q, ждали b,h", got)
	}
	only := profilesJSON(t, srv("a", "a.example", map[string]any{"meshEnabled": true, "meshKey": "K", "meshOnlyThis": true}),
		srv("b", "b.example", mesh("K")))
	tun.SetProfiles(only, "a", false)
	if got := extrasOf(tun); len(got) != 0 {
		t.Fatalf("«только этот сервер», а дополнительные: %v", got)
	}
	noMesh := profilesJSON(t, srv("a", "a.example", nil), srv("b", "b.example", mesh("K")))
	tun.SetProfiles(noMesh, "a", false)
	if got := extrasOf(tun); len(got) != 0 {
		t.Fatalf("у сервера сеть выключена, а дополнительные: %v", got)
	}
}

// Цепочка — по exitVia, по порядку, ключ у каждого сервера свой; больше двух
// — ошибка; удалённый сервер остаётся в цепочке (интернета не будет, но и
// молча короче она не станет).
func TestSetProfilesЦепочка(t *testing.T) {
	tun := NewTunnel()
	js := profilesJSON(t,
		srv("a", "a.example", map[string]any{"exitVia": []string{"c", "b"}}),
		srv("b", "b.example", nil),
		srv("c", "c.example", nil),
	)
	if err := tun.SetProfiles(js, "a", true); err != nil {
		t.Fatal(err)
	}
	tun.mu.Lock()
	hops := tun.exit
	tun.mu.Unlock()
	if len(hops) != 2 || hops[0].Host != "c.example" || hops[1].Host != "b.example" ||
		hops[1].KeyPath != "/keys/b" || !hops[1].UDPRelay {
		t.Fatalf("цепочка %+v", hops)
	}
	if err := tun.SetProfiles(profilesJSON(t, srv("a", "a", map[string]any{"exitVia": []string{"b", "c", "d"}})), "a", false); err == nil {
		t.Fatal("цепочка из трёх принята")
	}
	tun.SetProfiles(profilesJSON(t, srv("a", "a", map[string]any{"exitVia": []string{"gone"}})), "a", false)
	tun.mu.Lock()
	hops = tun.exit
	tun.mu.Unlock()
	if len(hops) != 1 || hops[0].Host != "" {
		t.Fatalf("удалённый сервер пропал из цепочки: %+v", hops)
	}
	if err := tun.SetProfiles("не json", "a", false); err == nil {
		t.Fatal("мусор принят")
	}
}

func meshServers(t *testing.T, tun *Tunnel) []string {
	t.Helper()
	var keys []string
	if err := json.Unmarshal([]byte(tun.MeshServersJSON()), &keys); err != nil {
		t.Fatal(err)
	}
	return keys
}

// Дополнительные серверы держатся, пока включена «Сеть устройств», и только
// тогда: кнопка выключена, слив, Stop — ни одного лишнего соединения.
func TestДополнительныеСерверыПоКнопке(t *testing.T) {
	tun, _ := offlineTunnel(t, true)
	js := profilesJSON(t,
		srv("a", "127.0.0.1", map[string]any{"meshEnabled": true, "meshKey": "K"}),
		srv("b", "127.0.0.2", map[string]any{"meshEnabled": true, "meshKey": "K"}),
	)
	if err := tun.SetProfiles(js, "a", false); err != nil {
		t.Fatal(err)
	}
	if got := meshServers(t, tun); len(got) != 0 {
		t.Fatalf("до запуска уже держим %v", got)
	}
	if err := tun.StartCore(); err != nil {
		t.Fatal(err)
	}
	if got := meshServers(t, tun); len(got) != 1 || got[0] != "b" {
		t.Fatalf("после запуска %v, ждали [b]", got)
	}
	tun.SetModes(false, false)
	if got := meshServers(t, tun); len(got) != 0 {
		t.Fatalf("сеть выключена, а держим %v", got)
	}
	tun.SetModes(false, true)
	if got := meshServers(t, tun); len(got) != 1 {
		t.Fatalf("сеть включена обратно, а держим %v", got)
	}
	// Серверы поменялись на ходу — сразу.
	tun.SetProfiles(profilesJSON(t, srv("a", "127.0.0.1", map[string]any{"meshEnabled": true, "meshKey": "K", "meshOnlyThis": true}),
		srv("b", "127.0.0.2", map[string]any{"meshEnabled": true, "meshKey": "K"})), "a", false)
	if got := meshServers(t, tun); len(got) != 0 {
		t.Fatalf("«только этот сервер», а держим %v", got)
	}
	tun.SetProfiles(js, "a", false)
	tun.Drain(1)
	if got := meshServers(t, tun); len(got) != 0 {
		t.Fatalf("слив, а держим %v", got)
	}
	tun.Stop()
	if got := meshServers(t, tun); len(got) != 0 {
		t.Fatalf("после Stop держим %v", got)
	}
}

// Цепочка доходит до туннеля: экран видит её путь, а пока сервер цепочки
// недоступен, состояние не «up».
func TestЦепочкаДоходитДоТуннеля(t *testing.T) {
	tun, _ := offlineTunnel(t, true)
	if tun.ExitJSON() != "" {
		t.Fatal("цепочки нет, а ExitJSON не пуст")
	}
	tun.mu.Lock()
	key := tun.cfg.KeyPath
	tun.mu.Unlock()
	js := profilesJSON(t,
		srv("a", "127.0.0.1", map[string]any{"exitVia": []string{"b"}, "keyPath": key}),
		srv("b", "127.0.0.1", map[string]any{"keyPath": key}),
	)
	if err := tun.SetProfiles(js, "a", false); err != nil {
		t.Fatal(err)
	}
	if err := tun.StartCore(); err != nil {
		t.Fatal(err)
	}
	var ex struct {
		Path  []string
		State string
	}
	if err := json.Unmarshal([]byte(tun.ExitJSON()), &ex); err != nil {
		t.Fatalf("ExitJSON %q: %v", tun.ExitJSON(), err)
	}
	if len(ex.Path) == 0 || ex.Path[len(ex.Path)-1] != "Сервер b" || ex.State == "up" {
		t.Fatalf("цепочка для экрана: %+v", ex)
	}
	// Через сервер без последнего сервера цепочки не уходит ничего.
	tun.mu.Lock()
	core := tun.core
	tun.mu.Unlock()
	if c, err := core.Dial("tcp", "example.com:80"); err == nil {
		c.Close()
		t.Fatal("соединение ушло, хотя сервера цепочки нет")
	}
}

// Лимитная связь доходит до резервных копий; переключения с разных потоков
// не ломают друг друга (гонять с -race и -count=20).
func TestЛимитнаяСвязь(t *testing.T) {
	SetMetered(true)
	if !Metered() {
		t.Fatal("не запомнилось")
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(on bool) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				SetMetered(on)
			}
		}(i%2 == 0)
	}
	wg.Wait()
	SetMetered(false)
	if Metered() {
		t.Fatal("Wi-Fi, а связь лимитная")
	}
}
