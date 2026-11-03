package calendar

// Home Assistant как источник и приёмник календарей (замечание владельца 20,
// 03.11.2026): вместо CalDAV — его API по долгоживущему токену. Читаем
// REST-ом (/api/calendars), пишем по WebSocket (calendar/event/create,
// update, delete — в REST HA их нет). Снаружи это тот же backend, что и
// CalDAV: календарь — коллекция «ha/<entity_id>/», событие — объект .ics,
// так что синхронизация, экран, телефон и виджет не знают разницы.
//
// HA отдаёт повторяющиеся события уже развёрнутыми (каждое вхождение с
// recurrence_id), поэтому у нас каждое вхождение — отдельный объект без
// RRULE: развернуть второй раз было бы нечего.

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// backend — откуда календари: CalDAV (client) или Home Assistant (haClient).
type backend interface {
	Discover(contacts bool) ([]Collection, error)
	Changes(col Collection) (changes, error)
	Fetch(col Collection, hrefs []string) (map[string][2]string, error)
	Put(href, etag, data string) (string, error)
	Delete(href, etag string) error
}

// KindHA — Account.Kind для Home Assistant.
const KindHA = "ha"

func newBackend(acc Account, pass string, dial DialFunc) (backend, error) {
	if acc.Kind == KindHA {
		return newHA(acc.URL, pass, dial)
	}
	return newClient(acc.URL, acc.User, pass, dial)
}

type haClient struct {
	base  *url.URL
	token string
	dial  DialFunc
	http  *http.Client
}

// Что знаем о событиях HA с последней сверки — общее для всех клиентов:
// движок заводит клиента на каждое действие, а правке нужно знать uid.
var (
	haMu    sync.Mutex
	haCache = map[string][2]string{} // href → (etag, ics)
	haIDs   = map[string]haRef{}     // href → чьё это событие в HA
)

type haRef struct{ Entity, UID, RecurrenceID string }

func newHA(rawURL, token string, dial DialFunc) (*haClient, error) {
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(rawURL), "/"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("адрес вида http://homeassistant.local:8123")
	}
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("нужен долгоживущий токен Home Assistant (Профиль → Безопасность)")
	}
	tr := &http.Transport{Proxy: nil, TLSHandshakeTimeout: 15 * time.Second}
	if dial != nil {
		tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) { return dial(ctx, addr) }
	}
	return &haClient{base: u, token: strings.TrimSpace(token), dial: dial,
		http: &http.Client{Transport: tr, Timeout: 60 * time.Second}}, nil
}

func (h *haClient) get(path string, out any) error {
	req, err := http.NewRequest(http.MethodGet, h.base.String()+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+h.token)
	resp, err := h.http.Do(req)
	if err != nil {
		return fmt.Errorf("Home Assistant не ответил: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return errors.New("Home Assistant не принял токен")
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Home Assistant ответил %s", resp.Status)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(out)
}

func (h *haClient) Discover(bool) ([]Collection, error) {
	var list []struct {
		EntityID string `json:"entity_id"`
		Name     string `json:"name"`
	}
	if err := h.get("/api/calendars", &list); err != nil {
		return nil, err
	}
	out := []Collection{}
	for _, c := range list {
		name := c.Name
		if name == "" {
			name = c.EntityID
		}
		out = append(out, Collection{Href: "ha/" + c.EntityID + "/", Name: name})
	}
	return out, nil
}

// haTime — время события HA: {"dateTime": …} или {"date": …}.
type haTime struct {
	DateTime string `json:"dateTime,omitempty"`
	Date     string `json:"date,omitempty"`
}

func (t haTime) parse() (time.Time, bool) {
	if t.Date != "" {
		d, err := time.ParseInLocation("2006-01-02", t.Date, time.Local)
		return d, err == nil
	}
	d, err := time.Parse(time.RFC3339, t.DateTime)
	return d, err == nil
}

type haEvent struct {
	Summary      string `json:"summary"`
	Description  string `json:"description"`
	Location     string `json:"location"`
	Start        haTime `json:"start"`
	End          haTime `json:"end"`
	UID          string `json:"uid"`
	RecurrenceID string `json:"recurrence_id"`
}

// Changes — все события окна (год назад … два вперёд): у HA нет ни ETag,
// ни токена изменений, поэтому полная сверка, ETag — хеш самого события.
func (h *haClient) Changes(col Collection) (changes, error) {
	entity := strings.TrimSuffix(strings.TrimPrefix(col.Href, "ha/"), "/")
	now := time.Now()
	q := url.Values{"start": {now.AddDate(-1, 0, 0).UTC().Format(time.RFC3339)},
		"end": {now.AddDate(2, 0, 0).UTC().Format(time.RFC3339)}}
	var evs []haEvent
	if err := h.get("/api/calendars/"+url.PathEscape(entity)+"?"+q.Encode(), &evs); err != nil {
		return changes{}, err
	}
	ch := changes{Changed: map[string]string{}, Full: true}
	haMu.Lock()
	defer haMu.Unlock()
	for _, ev := range evs {
		uid := ev.UID
		if uid == "" {
			uid = ev.Summary + "|" + ev.Start.DateTime + ev.Start.Date
		}
		key := uid
		if ev.RecurrenceID != "" {
			key += "@" + ev.RecurrenceID
		}
		sum := sha1.Sum([]byte(key))
		href := col.Href + hex.EncodeToString(sum[:8]) + ".ics"
		b, _ := json.Marshal(ev)
		tag := sha1.Sum(b)
		etag := hex.EncodeToString(tag[:])
		ch.Changed[href] = etag
		haCache[href] = [2]string{etag, haICS(uid+"|"+ev.RecurrenceID, ev)}
		haIDs[href] = haRef{entity, ev.UID, ev.RecurrenceID}
	}
	return ch, nil
}

// haICS — событие HA как VEVENT.
func haICS(uid string, ev haEvent) string {
	start, _ := ev.Start.parse()
	end, ok := ev.End.parse()
	if !ok {
		end = start.Add(time.Hour)
	}
	ed := Edit{Title: ev.Summary, Location: ev.Location, Description: ev.Description,
		Start: start, End: end, AllDay: ev.Start.Date != ""}
	return BuildEvent(uid, ed, time.Now())
}

func (h *haClient) Fetch(_ Collection, hrefs []string) (map[string][2]string, error) {
	haMu.Lock()
	defer haMu.Unlock()
	out := map[string][2]string{}
	for _, href := range hrefs {
		if v, ok := haCache[href]; ok {
			out[href] = v
		}
	}
	return out, nil
}

// haFields — поля события для записи в HA.
func haFields(data string) (map[string]any, error) {
	evs := ParseEvents(data)
	if len(evs) == 0 {
		return nil, errors.New("пустое событие")
	}
	e := evs[0]
	f := map[string]any{"summary": e.Title}
	if e.Description != "" {
		f["description"] = e.Description
	}
	if e.Location != "" {
		f["location"] = e.Location
	}
	if e.AllDay {
		f["dtstart"] = e.Start.Format("2006-01-02")
		end := e.End
		if !end.After(e.Start) {
			end = e.Start.AddDate(0, 0, 1)
		}
		f["dtend"] = end.Format("2006-01-02")
	} else {
		f["dtstart"] = e.Start.Local().Format(time.RFC3339)
		end := e.End
		if !end.After(e.Start) {
			end = e.Start.Add(time.Hour)
		}
		f["dtend"] = end.Local().Format(time.RFC3339)
	}
	return f, nil
}

// Put: новое (etag "") — create, иначе update. ETag вернётся следующей
// сверкой (HA его не сообщает).
func (h *haClient) Put(href, etag, data string) (string, error) {
	fields, err := haFields(data)
	if err != nil {
		return "", err
	}
	haMu.Lock()
	ref, known := haIDs[href]
	haMu.Unlock()
	if etag != "" && !known {
		return "", errors.New("событие не найдено в Home Assistant — обнови календарь")
	}
	if etag == "" {
		entity := strings.SplitN(strings.TrimPrefix(href, "ha/"), "/", 2)[0]
		return "", h.ws(map[string]any{"type": "calendar/event/create", "entity_id": entity, "event": fields})
	}
	msg := map[string]any{"type": "calendar/event/update", "entity_id": ref.Entity, "uid": ref.UID, "event": fields}
	if ref.RecurrenceID != "" {
		msg["recurrence_id"] = ref.RecurrenceID
	}
	return "", h.ws(msg)
}

func (h *haClient) Delete(href, _ string) error {
	haMu.Lock()
	ref, ok := haIDs[href]
	haMu.Unlock()
	if !ok {
		return errors.New("событие не найдено в Home Assistant — обнови календарь")
	}
	msg := map[string]any{"type": "calendar/event/delete", "entity_id": ref.Entity, "uid": ref.UID}
	if ref.RecurrenceID != "" {
		msg["recurrence_id"] = ref.RecurrenceID
	}
	return h.ws(msg)
}

// ws — одна команда по WebSocket API HA: вход по токену, команда, ответ.
// Свой маленький клиент (текстовые кадры), чтобы не тянуть библиотеку.
func (h *haClient) ws(cmd map[string]any) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	host := h.base.Host
	tlsOn := h.base.Scheme == "https"
	if h.base.Port() == "" {
		if tlsOn {
			host += ":443"
		} else {
			host += ":80"
		}
	}
	var conn net.Conn
	var err error
	if h.dial != nil {
		conn, err = h.dial(ctx, host)
	} else {
		conn, err = (&net.Dialer{}).DialContext(ctx, "tcp", host)
	}
	if err != nil {
		return fmt.Errorf("Home Assistant не ответил: %w", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(30 * time.Second))
	if tlsOn {
		tc := tls.Client(conn, &tls.Config{ServerName: h.base.Hostname()})
		if err := tc.Handshake(); err != nil {
			return err
		}
		conn = tc
	}
	key := make([]byte, 16)
	rand.Read(key)
	path := strings.TrimRight(h.base.Path, "/") + "/api/websocket"
	fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"+
		"Sec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n", path, h.base.Host, base64.StdEncoding.EncodeToString(key))
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		return fmt.Errorf("Home Assistant не открыл WebSocket (%s)", resp.Status)
	}
	read := func() (map[string]any, error) {
		b, err := wsRead(br)
		if err != nil {
			return nil, err
		}
		var m map[string]any
		return m, json.Unmarshal(b, &m)
	}
	send := func(v any) error {
		b, _ := json.Marshal(v)
		return wsWrite(conn, b)
	}
	if _, err := read(); err != nil { // auth_required
		return err
	}
	if err := send(map[string]string{"type": "auth", "access_token": h.token}); err != nil {
		return err
	}
	m, err := read()
	if err != nil {
		return err
	}
	if m["type"] != "auth_ok" {
		return errors.New("Home Assistant не принял токен")
	}
	cmd["id"] = 1
	if err := send(cmd); err != nil {
		return err
	}
	for {
		m, err := read()
		if err != nil {
			return err
		}
		if m["type"] != "result" {
			continue
		}
		if ok, _ := m["success"].(bool); ok {
			return nil
		}
		if e, ok := m["error"].(map[string]any); ok {
			return fmt.Errorf("Home Assistant: %v", e["message"])
		}
		return errors.New("Home Assistant отказал")
	}
}

// wsWrite — текстовый кадр от клиента (с маской, как требует протокол).
func wsWrite(w io.Writer, payload []byte) error {
	hdr := []byte{0x81}
	n := len(payload)
	switch {
	case n < 126:
		hdr = append(hdr, 0x80|byte(n))
	case n < 1<<16:
		hdr = append(hdr, 0x80|126, byte(n>>8), byte(n))
	default:
		hdr = append(hdr, 0x80|127)
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], uint64(n))
		hdr = append(hdr, b[:]...)
	}
	mask := make([]byte, 4)
	rand.Read(mask)
	hdr = append(hdr, mask...)
	buf := make([]byte, n)
	for i := range payload {
		buf[i] = payload[i] ^ mask[i%4]
	}
	_, err := w.Write(append(hdr, buf...))
	return err
}

// wsRead — данные одного сообщения (кадры-продолжения склеиваются).
func wsRead(r *bufio.Reader) ([]byte, error) {
	var msg []byte
	for {
		var h [2]byte
		if _, err := io.ReadFull(r, h[:]); err != nil {
			return nil, err
		}
		n := uint64(h[1] & 0x7f)
		switch n {
		case 126:
			var b [2]byte
			if _, err := io.ReadFull(r, b[:]); err != nil {
				return nil, err
			}
			n = uint64(binary.BigEndian.Uint16(b[:]))
		case 127:
			var b [8]byte
			if _, err := io.ReadFull(r, b[:]); err != nil {
				return nil, err
			}
			n = binary.BigEndian.Uint64(b[:])
		}
		if n > 16<<20 {
			return nil, errors.New("слишком большой ответ Home Assistant")
		}
		var mask [4]byte
		if h[1]&0x80 != 0 {
			if _, err := io.ReadFull(r, mask[:]); err != nil {
				return nil, err
			}
		}
		b := make([]byte, n)
		if _, err := io.ReadFull(r, b); err != nil {
			return nil, err
		}
		if h[1]&0x80 != 0 {
			for i := range b {
				b[i] ^= mask[i%4]
			}
		}
		op := h[0] & 0x0f
		if op == 0x8 {
			return nil, errors.New("Home Assistant закрыл соединение")
		}
		if op == 0x9 || op == 0xA { // ping/pong — пропускаем
			continue
		}
		msg = append(msg, b...)
		if h[0]&0x80 != 0 {
			return msg, nil
		}
	}
}
