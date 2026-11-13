package panel

import (
	"reflect"
	"testing"

	"sshtunnel/internal/nodeapi"
)

func TestПредупрежденияПлагинов(t *testing.T) {
	all := nodeapi.Config{Mirror: true, Files: true, Clipboard: true, Backup: true}
	codes := func(ws []NodeWarning) []string {
		out := []string{}
		for _, w := range ws {
			out = append(out, w.Code)
		}
		return out
	}
	if got := codes(pluginWarnings(MeshRoleSecondary, true, "", AccessSettings{}, nil, all)); !reflect.DeepEqual(got, []string{WarnSecondary}) {
		t.Fatalf("побочный: %v", got)
	}
	if got := codes(pluginWarnings(MeshRoleOff, true, "", AccessSettings{}, nil, all)); !reflect.DeepEqual(got, []string{WarnNoMesh}) {
		t.Fatalf("без сети: %v", got)
	}
	if got := pluginWarnings(MeshRoleMain, true, "", AccessSettings{}, nil, all); len(got) != 0 {
		t.Fatalf("всё открыто, а предупреждения: %v", got)
	}
	// Ничего не включено — и мешать нечему.
	if got := pluginWarnings(MeshRoleMain, false, "22", AccessSettings{Enabled: true}, nil, nodeapi.Config{}); len(got) != 0 {
		t.Fatalf("плагины выключены: %v", got)
	}
	ws := pluginWarnings(MeshRoleMain, false, "", AccessSettings{}, nil, all)
	if len(ws) != 1 || ws[0].Code != WarnServerOff || !ws[0].Fix {
		t.Fatalf("доступ выключен: %+v", ws)
	}
	ws = pluginWarnings(MeshRoleMain, true, "22,47835-47836", AccessSettings{}, nil, all)
	if len(ws) != 1 || ws[0].Code != WarnPortsBlocked || ws[0].Ports != "47837,47838" {
		t.Fatalf("порты: %+v", ws)
	}
	// Права: у «Гостя» нет правила к серверам на нужные порты.
	clients := []Client{
		{Name: "ПК", Username: "tun_1", State: StateActive, Groups: []string{"семья"}},
		{Name: "Гость", Username: "tun_2", State: StateActive, Groups: []string{"гости"}},
		{Name: "Заморожен", Username: "tun_3", State: StateFrozen},
	}
	acc := AccessSettings{Enabled: true, Rules: []AccessRule{
		{From: "семья", To: GroupServers},
		{From: "гости", To: GroupServers, Ports: "47835"},
	}}
	ws = pluginWarnings(MeshRoleMain, true, "", acc, clients, all)
	if len(ws) != 1 || ws[0].Code != WarnAccessRules || ws[0].Fix || !reflect.DeepEqual(ws[0].Users, []string{"Гость"}) {
		t.Fatalf("права: %+v", ws)
	}
	acc.Rules = append(acc.Rules, AccessRule{From: GroupAll, To: GroupAll, Ports: "47835-47838"})
	if ws = pluginWarnings(MeshRoleMain, true, "", acc, clients, all); len(ws) != 0 {
		t.Fatalf("правило «все → все» на эти порты: %+v", ws)
	}
}

func TestИсправлениеПортовСервера(t *testing.T) {
	all := nodeapi.Config{Mirror: true, Files: true, Clipboard: true, Backup: true}
	cases := []struct {
		access bool
		ports  string
		want   string
	}{
		{true, "", ""},                         // всё и так открыто — не сужаем
		{false, "", "47835,47836,47837,47838"}, // был выключен: только порты плагинов, не «все»
		{true, "22", "22,47835,47836,47837,47838"},
		{true, "22,47836", "22,47836,47835,47837,47838"},
		{true, "47835-47838", "47835-47838"},
	}
	for _, c := range cases {
		if got := fixServerPorts(c.access, c.ports, all); got != c.want {
			t.Errorf("%v %q → %q, ждали %q", c.access, c.ports, got, c.want)
		}
	}
}
