package mesh

// Профиль устройства в панели (ТЗ 16), с настоящими meshd: цепочка выхода
// из панели доходит до устройства и обратно, две панели — побеждает новая,
// одобрение и заморозка (прямое соединение рвётся, без сервера — тоже нет).

import (
	"context"
	"strconv"
	"testing"
	"time"

	"sshtunnel/internal/config"
)

type profDev struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Pending  bool   `json:"pending"`
	Frozen   bool   `json:"frozen"`
	Profiles []struct {
		Key string   `json:"k"`
		Via []string `json:"v"`
		T   int64    `json:"t"`
		By  string   `json:"b"`
	} `json:"profiles"`
}

type profState struct {
	Self struct {
		Name string `json:"name"`
	} `json:"self"`
	Networks []struct {
		ID      string    `json:"id"`
		Devices []profDev `json:"devices"`
	} `json:"networks"`
}

func profOf(t *testing.T, s *testMeshd, name string) (profDev, bool) {
	t.Helper()
	var st profState
	getJSON(t, adminClient(s.sock), "/v1/state", &st)
	for _, n := range st.Networks {
		for _, d := range n.Devices {
			if d.Name == name {
				return d, true
			}
		}
	}
	return profDev{}, false
}

// exitDevice — «приложение» вокруг клиента: свои профили и правило «новее
// побеждает» из config, как в app.onMeshExit.
type exitDevice struct {
	cfg    config.Config
	orders chan ExitOrder
}

func (d *exitDevice) refs() []ProfileRef {
	var out []ProfileRef
	for _, p := range d.cfg.Profiles {
		r := ProfileRef{Key: config.ServerKey(p), Name: p.Name, Active: p.ID == d.cfg.ActiveProfile, T: p.ExitT, By: p.ExitBy}
		for _, id := range p.ExitVia {
			h, _ := d.cfg.ProfileByID(id)
			r.Via = append(r.Via, config.ServerKey(h))
		}
		out = append(out, r)
	}
	return out
}

func TestЦепочкаИзПанелиДвеПанели(t *testing.T) {
	srv := startMeshds(t, 2)
	dev := &exitDevice{cfg: config.Default(), orders: make(chan ExitOrder, 8)}
	fra := dev.cfg.Profiles[0]
	fra.Name, fra.Host = "Франкфурт", "fra.example"
	dev.cfg.SetProfile(fra)
	lon := dev.cfg.AddProfile("Лондон", "")
	lon.Host = "lon.example"
	dev.cfg.SetProfile(lon)
	war := dev.cfg.AddProfile("Варшава", "")
	war.Host = "war.example"
	dev.cfg.SetProfile(war)

	c := startMulti(t, srv, Config{Key: NewKey(), Name: "pc", OnExit: func(o ExitOrder) { dev.orders <- o }})
	c.SetProfiles(dev.refs())
	nid := ""
	waitFor(t, func() bool {
		d, ok := profOf(t, srv[1], "pc")
		return ok && len(d.Profiles) == 3
	}, "второй сервер не узнал о профилях устройства")
	var st profState
	getJSON(t, adminClient(srv[0].sock), "/v1/state", &st)
	nid = st.Networks[0].ID
	devID := st.Networks[0].Devices[0].ID

	apply := func() ExitOrder {
		t.Helper()
		select {
		case o := <-dev.orders:
			if _, _, err := dev.cfg.ApplyExitOrder(config.ExitOrder{Entry: o.Entry, Via: o.Via, T: o.T, By: o.By}); err != nil {
				t.Fatal(err)
			}
			c.SetProfiles(dev.refs())
			return o
		case <-time.After(5 * time.Second):
			t.Fatal("цепочка из панели не дошла")
		}
		return ExitOrder{}
	}

	// Панель первого сервера: Франкфурт → Лондон.
	post(t, adminClient(srv[0].sock), "/v1/exit", map[string]any{"net": nid, "device": devID,
		"entry": "fra.example:22", "via": []string{"lon.example:22"}})
	oa := apply()
	if p, _ := dev.cfg.ProfileByID(fra.ID); len(p.ExitVia) != 1 || p.ExitVia[0] != lon.ID || p.ExitBy == "" {
		t.Fatalf("на устройстве: %+v", p)
	}
	// Вторая панель видит значение устройства и чья это правка.
	waitFor(t, func() bool {
		d, _ := profOf(t, srv[1], "pc")
		for _, p := range d.Profiles {
			if p.Key == "fra.example:22" && p.T == oa.T && p.By == oa.By && len(p.Via) == 1 {
				return true
			}
		}
		return false
	}, "вторая панель не видит цепочку от первой")

	// Панель второго сервера задаёт свою — она новее и побеждает.
	post(t, adminClient(srv[1].sock), "/v1/exit", map[string]any{"net": nid, "device": devID,
		"entry": "fra.example:22", "via": []string{"war.example:22"}})
	ob := apply()
	if ob.T <= oa.T {
		t.Fatalf("вторая правка не новее первой: %d ≤ %d", ob.T, oa.T)
	}
	if p, _ := dev.cfg.ProfileByID(fra.ID); len(p.ExitVia) != 1 || p.ExitVia[0] != war.ID {
		t.Fatalf("после второй панели: %+v", p)
	}
	// Человек на устройстве меняет сам — панели видят его значение.
	p, _ := dev.cfg.ProfileByID(fra.ID)
	p.ExitVia = nil
	config.TouchExit(&p, time.Now().Unix())
	dev.cfg.SetProfile(p)
	c.SetProfiles(dev.refs())
	for _, s := range srv {
		waitFor(t, func() bool {
			d, _ := profOf(t, s, "pc")
			for _, p := range d.Profiles {
				if p.Key == "fra.example:22" && p.By == "" && len(p.Via) == 0 && p.T > ob.T {
					return true
				}
			}
			return false
		}, "панель не видит правку человека")
	}
	// Старые приказы панелей больше не приходят.
	select {
	case o := <-dev.orders:
		t.Fatalf("лишний приказ: %+v", o)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestОдобрениеНаУстройстве(t *testing.T) {
	addr, sock := startMeshdAdmin(t)
	key := NewKey()
	a := startClient(t, addr, Config{Key: key, Name: "a", NoDirect: true})
	_ = a
	var st profState
	waitFor(t, func() bool { getJSON(t, adminClient(sock), "/v1/state", &st); return len(st.Networks) == 1 }, "нет сети")
	post(t, adminClient(sock), "/v1/net/approval", map[string]any{"net": st.Networks[0].ID, "on": true})

	b := startClient(t, addr, Config{Key: key, Name: "b", NoDirect: true})
	waitFor(t, func() bool { s := b.Status(); return len(s.Servers) == 1 && s.Servers[0].Hold == "pending" }, "b не знает, что ждёт")
	if p, n := peerByHost(b, "a"); n != 0 {
		t.Fatalf("ждущий видит других: %+v", p)
	}
	if _, n := peerByHost(a, "b"); n != 0 {
		t.Fatal("ждущего видят другие")
	}
	if _, err := b.DialPeer("a.mesh:80"); err == nil {
		t.Fatal("ждущий дозвонился")
	}
	d, _ := profOf(t, &testMeshd{sock: sock}, "b")
	post(t, adminClient(sock), "/v1/approve", map[string]any{"net": st.Networks[0].ID, "device": d.ID})
	waitFor(t, func() bool { _, n := peerByHost(b, "a"); return n == 1 && b.Status().Servers[0].Hold == "" }, "после одобрения b не видит a")
	waitFor(t, func() bool { _, n := peerByHost(a, "b"); return n == 1 }, "после одобрения a не видит b")
}

// Заморозка рвёт прямое соединение, и без сервера замороженный тоже никуда
// не попадает: другие уже убрали его из своего списка.
func TestЗаморозкаРвётПрямуюСвязь(t *testing.T) {
	addr, sock, _, kill := startMeshdSTUNKill(t)
	key := NewKey()
	a := newNode(t, addr, Config{Key: key, Name: "a", AllowIncoming: true})
	b := newNode(t, addr, Config{Key: key, Name: "b", AllowIncoming: true})
	a.start()
	b.start()
	waitFor(t, func() bool { return a.c.Status().State == "online" && b.c.Status().State == "online" }, "не подключились")
	aIP, bIP := a.ip(), b.ip()
	waitLong(t, 15*time.Second, func() bool { return peerStatus(a.c, bIP).Direct }, "прямое соединение не установилось")
	portA := echoService(t, "a")
	portB := echoService(t, "b")

	post(t, adminClient(sock), "/v1/freeze", map[string]any{"net": netID(key), "device": b.cfg.DeviceID, "on": true})
	waitFor(t, func() bool {
		_, n := peerByHost(a.c, "b")
		return n == 0 && !peerStatus(a.c, bIP).Direct
	}, "a не убрал замороженного и прямую связь с ним")
	waitFor(t, func() bool {
		s := b.c.Status()
		return s.Servers[0].Hold == "frozen" && !peerStatus(b.c, aIP).Direct
	}, "замороженный не узнал о заморозке или держит прямую связь")
	if _, err := a.c.DialPeer("b.mesh:" + strconv.Itoa(portB)); err == nil {
		t.Fatal("к замороженному дозвонились")
	}
	if _, err := b.c.DialPeer(aIP + ":" + strconv.Itoa(portA)); err == nil {
		t.Fatal("замороженный дозвонился")
	}

	// Сервера нет: маяки и запомненные адреса его не вернут.
	kill()
	waitFor(t, func() bool { return a.c.Status().State != "online" }, "a не заметил, что сервера нет")
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	for ctx.Err() == nil {
		if peerStatus(a.c, bIP).Direct || peerStatus(b.c, aIP).Direct {
			t.Fatal("без сервера замороженный снова на прямой связи")
		}
		time.Sleep(200 * time.Millisecond)
	}
	if _, err := b.c.DialPeer(aIP + ":" + strconv.Itoa(portA)); err == nil {
		t.Fatal("без сервера замороженный дозвонился")
	}
}
