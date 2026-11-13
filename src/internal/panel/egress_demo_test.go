package panel

import (
	"testing"
	"time"

	"sshtunnel/internal/share"
)

func TestDemoEgressApplyConfirmOff(t *testing.T) {
	h := NewDemoHandler("test")
	data, err := share.Build(share.Doc{Name: "Б", Host: "203.0.113.50", SSHPort: 22, User: "tun_demo",
		KeyIncluded: true, KeyContents: "демо-ключ"})
	if err != nil {
		t.Fatal(err)
	}

	if code, out := demoCall(t, h, "GET", "/api/egress", nil); code != 200 || out["phase"] != EgressOff {
		t.Fatalf("исходная фаза должна быть off: %d %v", code, out)
	}

	if code, out := demoCall(t, h, "POST", "/api/egress/apply", map[string]string{"link": string(data), "mode": EgressModeDirect}); code != 200 {
		t.Fatalf("apply: %d %v", code, out)
	}
	_, out := demoCall(t, h, "GET", "/api/egress", nil)
	if out["phase"] != EgressApplying {
		t.Fatalf("после apply фаза должна быть applying: %v", out)
	}
	token, _ := out["token"].(string)
	if token == "" {
		t.Fatal("нет токена")
	}

	if code, out := demoCall(t, h, "POST", "/api/egress/confirm", map[string]string{"token": "не тот"}); code == 200 {
		t.Fatalf("неверный токен должен быть отказом: %v", out)
	}
	if code, out := demoCall(t, h, "POST", "/api/egress/confirm", map[string]string{"token": token}); code != 200 {
		t.Fatalf("confirm: %d %v", code, out)
	}
	_, out = demoCall(t, h, "GET", "/api/egress", nil)
	if out["phase"] != EgressOn {
		t.Fatalf("после confirm фаза должна быть on: %v", out)
	}
	if out["exitIp"] == nil || out["exitIp"] == "" {
		t.Fatal("нет адреса снаружи")
	}

	if code, _ := demoCall(t, h, "POST", "/api/egress/off", map[string]string{}); code != 200 {
		t.Fatal("off должен пройти")
	}
	_, out = demoCall(t, h, "GET", "/api/egress", nil)
	if out["phase"] != EgressOff {
		t.Fatalf("после off фаза должна быть off: %v", out)
	}
}

func TestDemoEgressОткатБезПодтверждения(t *testing.T) {
	h := NewDemoHandler("test")
	data, _ := share.Build(share.Doc{Name: "Б", Host: "203.0.113.50", SSHPort: 22, User: "tun_demo",
		KeyIncluded: true, KeyContents: "демо-ключ"})
	if code, _ := demoCall(t, h, "POST", "/api/egress/apply", map[string]string{"link": string(data)}); code != 200 {
		t.Fatal("apply должен пройти")
	}
	// Напрямую стейт-машину не потрогать — ждём настоящее окно показа
	// (demoEgressWindowSec) через саму ручку.
	deadline := time.Now().Add(demoEgressWindowSec*time.Second + 3*time.Second)
	for time.Now().Before(deadline) {
		_, out := demoCall(t, h, "GET", "/api/egress", nil)
		if out["phase"] == EgressOff {
			if out["rolledBackAt"] == nil {
				t.Fatal("откат должен отметиться rolledBackAt")
			}
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("окно подтверждения в показе не истекло за отведённое время")
}
