package sshapp

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"runtime"
	"time"

	"sshtunnel/internal/addons"
	"sshtunnel/internal/devnet"
)

// Routes — адреса API приложения. Окно VPN-версии Windows ставит их у себя
// (addons.AppRoutes("ssh")): терминал и ключ — дело пользователя, а не
// службы.
func init() {
	addons.RegisterRoutes("ssh",
		addons.Route{Path: "/api/ssh", Methods: "GET, POST",
			Desc: "SSH: серверы этого устройства и открытый ключ программы. POST {\"op\":\"save\",\"server\":{id?,name,host,port,user}} | {\"op\":\"delete\"|\"forgethost\",\"id\"}.",
			H:    handleList},
		addons.Route{Path: "/api/ssh/devices", Methods: "GET",
			Desc: "Устройства сети устройств — выбор адреса в анкете сервера.",
			H:    handleDevices},
		addons.Route{Path: "/api/ssh/install", Methods: "POST",
			Desc: "Установить ключ программы на сервер {\"id\",\"password\"}: пароль используется один раз и не сохраняется.",
			H:    handleInstall},
		addons.Route{Path: "/api/ssh/open", Methods: "POST",
			Desc: "Открыть терминал с входом на сервер {\"id\"} (ПК). {\"builtin\":true} — внешнего нет, открывай встроенный.",
			H:    handleOpen},
		addons.Route{Path: "/api/ssh/term", Methods: "POST",
			Desc: "Встроенный терминал: {\"op\":\"start\",\"id\",\"cols\",\"rows\"} → {\"sid\"}; {\"op\":\"in\",\"sid\",\"d\":base64}; {\"op\":\"resize\",\"sid\",\"cols\",\"rows\"}; {\"op\":\"close\",\"sid\"}.",
			H:    handleTerm},
		addons.Route{Path: "/api/ssh/term/out", Methods: "GET",
			Desc: "Вывод встроенного терминала ?sid=: поток SSE, data: {\"d\":base64} или {\"end\":true,\"error\"}.",
			H:    handleTermOut},
	)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, err error) {
	out := map[string]any{"error": err.Error()}
	if errors.Is(err, ErrHostKeyChanged) {
		out["code"] = "hostkey_changed"
	}
	writeJSON(w, out)
}

// external — умеет ли это устройство открыть внешний терминал.
func external() bool {
	_, ok := findSSH()
	return ok && runtime.GOOS != "android"
}

func handleList(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var req struct {
			Op     string `json:"op"`
			ID     string `json:"id"`
			Server Server `json:"server"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			fail(w, errors.New("нужен JSON {\"op\"}"))
			return
		}
		var err error
		switch req.Op {
		case "save":
			_, err = Save(req.Server)
		case "delete":
			err = Delete(req.ID)
		case "forgethost":
			err = setHostKey(req.ID, "")
		default:
			err = errors.New("op: save, delete или forgethost")
		}
		if err != nil {
			fail(w, err)
			return
		}
	}
	pub, err := PublicKey()
	out := map[string]any{"servers": Servers(), "pub": pub, "external": external(), "platform": runtime.GOOS}
	if err != nil {
		out["keyError"] = err.Error()
	}
	writeJSON(w, out)
}

func server(id string) (Server, error) {
	s, ok := Get(id)
	if !ok {
		return s, errors.New("сервер не найден")
	}
	return s, nil
}

func handleInstall(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID       string `json:"id"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Password == "" {
		fail(w, errors.New("нужен пароль"))
		return
	}
	s, err := server(req.ID)
	if err != nil {
		fail(w, err)
		return
	}
	info, err := InstallKey(s, req.Password)
	req.Password = ""
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "fingerprint": info.Fingerprint, "newHostKey": info.New})
}

func handleOpen(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	s, err := server(req.ID)
	if err != nil {
		fail(w, err)
		return
	}
	if err := Open(s); err != nil {
		if errors.Is(err, ErrNoTerminal) {
			writeJSON(w, map[string]any{"builtin": true})
			return
		}
		fail(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func handleTerm(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Op   string `json:"op"`
		ID   string `json:"id"`
		SID  string `json:"sid"`
		D    string `json:"d"`
		Cols int    `json:"cols"`
		Rows int    `json:"rows"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, errors.New("нужен JSON {\"op\"}"))
		return
	}
	if req.Op == "start" {
		s, err := server(req.ID)
		if err != nil {
			fail(w, err)
			return
		}
		t, info, err := StartTerm(s, req.Cols, req.Rows)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, map[string]any{"sid": t.id, "fingerprint": info.Fingerprint, "newHostKey": info.New})
		return
	}
	t := getTerm(req.SID)
	if t == nil {
		fail(w, errors.New("сессия закончилась"))
		return
	}
	var err error
	switch req.Op {
	case "in":
		err = t.input(req.D)
	case "resize":
		err = t.resize(req.Cols, req.Rows)
	case "close":
		t.finish(nil)
	default:
		err = errors.New("op: start, in, resize или close")
	}
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func handleTermOut(w http.ResponseWriter, r *http.Request) {
	t := getTerm(r.URL.Query().Get("sid"))
	if t == nil {
		http.Error(w, "сессия закончилась", http.StatusNotFound)
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "нет потока", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	t.mu.Lock()
	t.readers++
	t.mu.Unlock()
	defer func() {
		t.mu.Lock()
		t.readers--
		t.idleAt = time.Now()
		t.mu.Unlock()
	}()
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		data, ended, errText := t.take()
		if len(data) > 0 {
			fmt.Fprintf(w, "data: {\"d\":%q}\n\n", base64.StdEncoding.EncodeToString(data))
		}
		if ended {
			b, _ := json.Marshal(map[string]any{"end": true, "error": errText})
			fmt.Fprintf(w, "data: %s\n\n", b)
			fl.Flush()
			return
		}
		fl.Flush()
		select {
		case <-r.Context().Done():
			return
		case <-t.wake:
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
		}
	}
}

func handleDevices(w http.ResponseWriter, r *http.Request) {
	type dev struct {
		Name     string `json:"name"`
		Host     string `json:"host"`
		IP       string `json:"ip"`
		Platform string `json:"platform"`
		Online   bool   `json:"online"`
	}
	out := []dev{}
	for _, p := range devnet.Status().Peers {
		if p.Self || p.Platform == "server" {
			continue
		}
		out = append(out, dev{p.Name, p.Host, p.IP.String(), p.Platform, p.Online})
	}
	writeJSON(w, map[string]any{"devices": out})
}
