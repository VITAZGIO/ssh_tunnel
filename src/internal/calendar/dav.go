package calendar

// Клиент CalDAV/CardDAV (RFC 4791, 6352, 6578) поверх net/http: поиск
// календарей, синхронизация по sync-token, чтение пачкой, запись с ETag.
// Своё, а не библиотека: нужна малая часть протокола, а настольная версия
// держится на трёх зависимостях (docs/decisions.md, ТЗ 7).

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

// ErrConflict — сервер отказал по ETag (412): событие уже изменили в другом
// месте. Решает сервер: перечитываем и показываем его версию.
var ErrConflict = errors.New("событие уже изменили в другом месте — показана версия с сервера")

// errTokenInvalid — сервер забыл sync-token: нужна полная сверка.
var errTokenInvalid = errors.New("sync-token устарел")

// DialFunc — как открыть TCP-соединение до сервера (через ядро: сеть
// устройств, напрямую или через сервер — решает core.Decide).
type DialFunc func(ctx context.Context, addr string) (net.Conn, error)

type client struct {
	base *url.URL
	user string
	pass string
	http *http.Client
}

func newClient(rawURL, user, pass string, dial DialFunc) (*client, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("адрес вида http://nextcloud.mesh или https://cloud.example.com")
	}
	tr := &http.Transport{
		Proxy:               nil, // путь выбирает ядро, а не системный прокси
		TLSHandshakeTimeout: 15 * time.Second,
		IdleConnTimeout:     60 * time.Second,
	}
	if dial != nil {
		tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dial(ctx, addr)
		}
	}
	return &client{
		base: u, user: user, pass: pass,
		http: &http.Client{
			Transport: tr,
			Timeout:   60 * time.Second,
			// Перенаправления (/.well-known/caldav) — сами, с тем же методом.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

func (c *client) abs(href string) string {
	r, err := url.Parse(href)
	if err != nil {
		return href
	}
	return c.base.ResolveReference(r).String()
}

// do — запрос с логином; перенаправления (до 5) — тем же методом.
func (c *client) do(method, href, body string, hdr map[string]string) (*http.Response, error) {
	target := c.abs(href)
	for i := 0; i < 5; i++ {
		req, err := http.NewRequest(method, target, strings.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.SetBasicAuth(c.user, c.pass)
		if body != "" && hdr["Content-Type"] == "" {
			req.Header.Set("Content-Type", "application/xml; charset=utf-8")
		}
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := c.http.Do(req)
		if err != nil {
			var ue *url.Error
			if errors.As(err, &ue) {
				err = ue.Err
			}
			return nil, fmt.Errorf("Нет связи с %s: %v", c.base.Host, err)
		}
		if resp.StatusCode >= 300 && resp.StatusCode < 400 && resp.Header.Get("Location") != "" {
			loc, _ := url.Parse(resp.Header.Get("Location"))
			resp.Body.Close()
			nu, _ := url.Parse(target)
			target = nu.ResolveReference(loc).String()
			continue
		}
		if resp.StatusCode == http.StatusUnauthorized {
			resp.Body.Close()
			return nil, errors.New("Nextcloud не принял логин или пароль приложения")
		}
		return resp, nil
	}
	return nil, errors.New("слишком много перенаправлений")
}

// ── разбор multistatus ───────────────────────────────────────────────────

type msResponse struct {
	Href     string       `xml:"DAV: href"`
	Status   string       `xml:"DAV: status"`
	Propstat []msPropstat `xml:"DAV: propstat"`
}

type msPropstat struct {
	Status string `xml:"DAV: status"`
	Prop   msProp `xml:"DAV: prop"`
}

type msHref struct {
	Href string `xml:"DAV: href"`
}

type msProp struct {
	DisplayName  string `xml:"DAV: displayname"`
	ETag         string `xml:"DAV: getetag"`
	SyncToken    string `xml:"DAV: sync-token"`
	CTag         string `xml:"http://calendarserver.org/ns/ getctag"`
	Color        string `xml:"http://apple.com/ns/ical/ calendar-color"`
	Principal    msHref `xml:"DAV: current-user-principal"`
	CalHome      msHref `xml:"urn:ietf:params:xml:ns:caldav calendar-home-set"`
	CardHome     msHref `xml:"urn:ietf:params:xml:ns:carddav addressbook-home-set"`
	CalData      string `xml:"urn:ietf:params:xml:ns:caldav calendar-data"`
	AddrData     string `xml:"urn:ietf:params:xml:ns:carddav address-data"`
	ResourceType struct {
		Calendar    *struct{} `xml:"urn:ietf:params:xml:ns:caldav calendar"`
		AddressBook *struct{} `xml:"urn:ietf:params:xml:ns:carddav addressbook"`
	} `xml:"DAV: resourcetype"`
	Comps struct {
		Comp []struct {
			Name string `xml:"name,attr"`
		} `xml:"urn:ietf:params:xml:ns:caldav comp"`
	} `xml:"urn:ietf:params:xml:ns:caldav supported-calendar-component-set"`
	Privs struct {
		Priv []struct {
			Inner []byte `xml:",innerxml"`
		} `xml:"DAV: privilege"`
	} `xml:"DAV: current-user-privilege-set"`
}

func okStatus(s string) bool { return s == "" || strings.Contains(s, " 200") }

func (c *client) multistatus(method, href, depth, body string) ([]msResponse, error) {
	hdr := map[string]string{}
	if depth != "" {
		hdr["Depth"] = depth
	}
	resp, err := c.do(method, href, body, hdr)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusConflict ||
		resp.StatusCode == http.StatusPreconditionFailed {
		if bytes.Contains(data, []byte("valid-sync-token")) {
			return nil, errTokenInvalid
		}
	}
	if resp.StatusCode != 207 {
		return nil, fmt.Errorf("%s %s: сервер ответил %s", method, href, resp.Status)
	}
	var ms struct {
		Responses []msResponse `xml:"DAV: response"`
		SyncToken string       `xml:"DAV: sync-token"`
	}
	if err := xml.Unmarshal(data, &ms); err != nil {
		return nil, fmt.Errorf("ответ сервера не разобрать: %v", err)
	}
	if ms.SyncToken != "" {
		// Токен из REPORT sync-collection — отдельным «ответом» без href.
		ms.Responses = append(ms.Responses, msResponse{Propstat: []msPropstat{{Prop: msProp{SyncToken: ms.SyncToken}}}})
	}
	return ms.Responses, nil
}

// prop200 — свойства с кодом 200 из ответа.
func (r msResponse) prop200() msProp {
	for _, ps := range r.Propstat {
		if okStatus(ps.Status) {
			return ps.Prop
		}
	}
	return msProp{}
}

// ── поиск календарей ─────────────────────────────────────────────────────

// Collection — календарь или адресная книга на сервере.
type Collection struct {
	Href     string `json:"href"`
	Name     string `json:"name"`
	Color    string `json:"color,omitempty"`
	Contacts bool   `json:"contacts,omitempty"`
	ReadOnly bool   `json:"readOnly,omitempty"`
	CTag     string `json:"-"`
	// SyncToken — с какого места спрашивать изменения.
	SyncToken string `json:"-"`
}

const propfindPrincipal = `<?xml version="1.0"?><d:propfind xmlns:d="DAV:"><d:prop><d:current-user-principal/></d:prop></d:propfind>`

const propfindHomes = `<?xml version="1.0"?><d:propfind xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav" xmlns:card="urn:ietf:params:xml:ns:carddav"><d:prop><c:calendar-home-set/><card:addressbook-home-set/></d:prop></d:propfind>`

const propfindCollections = `<?xml version="1.0"?><d:propfind xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav" xmlns:cs="http://calendarserver.org/ns/" xmlns:ic="http://apple.com/ns/ical/"><d:prop><d:resourcetype/><d:displayname/><d:sync-token/><cs:getctag/><ic:calendar-color/><c:supported-calendar-component-set/><d:current-user-privilege-set/></d:prop></d:propfind>`

// principal — адрес учётной записи. Пустой путь в адресе — сначала
// /remote.php/dav (Nextcloud), потом /.well-known/caldav.
func (c *client) principal() (string, error) {
	tries := []string{c.base.Path}
	if c.base.Path == "" || c.base.Path == "/" {
		tries = []string{"/remote.php/dav/", "/.well-known/caldav"}
	}
	var lastErr error
	for _, href := range tries {
		rs, err := c.multistatus("PROPFIND", href, "0", propfindPrincipal)
		if err != nil {
			lastErr = err
			continue
		}
		for _, r := range rs {
			if p := r.prop200().Principal.Href; p != "" {
				return p, nil
			}
		}
		lastErr = errors.New("сервер не назвал учётную запись (current-user-principal)")
	}
	return "", lastErr
}

// Discover — все календари (с событиями) и, если contacts, адресные книги.
func (c *client) Discover(contacts bool) ([]Collection, error) {
	pr, err := c.principal()
	if err != nil {
		return nil, err
	}
	rs, err := c.multistatus("PROPFIND", pr, "0", propfindHomes)
	if err != nil {
		return nil, err
	}
	var homes []string
	for _, r := range rs {
		p := r.prop200()
		if p.CalHome.Href != "" {
			homes = append(homes, p.CalHome.Href)
		}
		if contacts && p.CardHome.Href != "" {
			homes = append(homes, p.CardHome.Href)
		}
	}
	if len(homes) == 0 {
		return nil, errors.New("у учётной записи нет календарей (calendar-home-set)")
	}
	var out []Collection
	for _, h := range homes {
		rs, err := c.multistatus("PROPFIND", h, "1", propfindCollections)
		if err != nil {
			return nil, err
		}
		for _, r := range rs {
			p := r.prop200()
			isCal := p.ResourceType.Calendar != nil
			isCard := p.ResourceType.AddressBook != nil
			if !isCal && !isCard {
				continue
			}
			if isCal && len(p.Comps.Comp) > 0 {
				hasEvents := false
				for _, cm := range p.Comps.Comp {
					hasEvents = hasEvents || strings.EqualFold(cm.Name, "VEVENT")
				}
				if !hasEvents {
					continue // только задачи (VTODO)
				}
			}
			name := p.DisplayName
			if name == "" {
				name = path.Base(strings.TrimSuffix(r.Href, "/"))
			}
			ro := len(p.Privs.Priv) > 0
			for _, pv := range p.Privs.Priv {
				in := string(pv.Inner)
				if strings.Contains(in, "write") || strings.Contains(in, "all") || strings.Contains(in, "bind") {
					ro = false
				}
			}
			col := Collection{Href: r.Href, Name: name, Contacts: isCard, ReadOnly: ro, CTag: p.CTag}
			if len(p.Color) >= 7 {
				col.Color = p.Color[:7] // Nextcloud пишет #RRGGBBAA
			}
			out = append(out, col)
		}
	}
	return out, nil
}

// ── синхронизация коллекции ──────────────────────────────────────────────

// changes — что изменилось в коллекции: новые/изменённые (href → etag) и
// удалённые href, новый токен.
type changes struct {
	Changed map[string]string
	Removed []string
	Token   string
	Full    bool // полная сверка: всё, чего нет в Changed, — удалено
}

const syncReport = `<?xml version="1.0"?><d:sync-collection xmlns:d="DAV:"><d:sync-token>%s</d:sync-token><d:sync-level>1</d:sync-level><d:prop><d:getetag/></d:prop></d:sync-collection>`

const propfindETags = `<?xml version="1.0"?><d:propfind xmlns:d="DAV:"><d:prop><d:getetag/><d:resourcetype/><d:sync-token/></d:prop></d:propfind>`

// Changes — по sync-token, а если сервер его не знает или не умеет —
// полный список ETag.
func (c *client) Changes(col Collection) (changes, error) {
	if col.SyncToken != "" {
		rs, err := c.multistatus("REPORT", col.Href, "", fmt.Sprintf(syncReport, xmlEscape(col.SyncToken)))
		if err == nil {
			ch := changes{Changed: map[string]string{}}
			for _, r := range rs {
				if r.Href == "" {
					ch.Token = r.prop200().SyncToken
					continue
				}
				if strings.Contains(r.Status, " 404") {
					ch.Removed = append(ch.Removed, r.Href)
					continue
				}
				if et := r.prop200().ETag; et != "" {
					ch.Changed[r.Href] = et
				}
			}
			return ch, nil
		}
		if !errors.Is(err, errTokenInvalid) {
			return changes{}, err
		}
	}
	rs, err := c.multistatus("PROPFIND", col.Href, "1", propfindETags)
	if err != nil {
		return changes{}, err
	}
	ch := changes{Changed: map[string]string{}, Full: true}
	for _, r := range rs {
		p := r.prop200()
		if sameHref(r.Href, col.Href) {
			ch.Token = p.SyncToken
			continue
		}
		if p.ETag != "" {
			ch.Changed[r.Href] = p.ETag
		}
	}
	return ch, nil
}

func sameHref(a, b string) bool {
	return strings.TrimSuffix(a, "/") == strings.TrimSuffix(b, "/")
}

func xmlEscape(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// Fetch читает объекты пачкой (calendar-multiget / addressbook-multiget):
// href → (etag, данные).
func (c *client) Fetch(col Collection, hrefs []string) (map[string][2]string, error) {
	out := map[string][2]string{}
	for len(hrefs) > 0 {
		n := len(hrefs)
		if n > 50 {
			n = 50
		}
		var b strings.Builder
		if col.Contacts {
			b.WriteString(`<?xml version="1.0"?><card:addressbook-multiget xmlns:d="DAV:" xmlns:card="urn:ietf:params:xml:ns:carddav"><d:prop><d:getetag/><card:address-data/></d:prop>`)
		} else {
			b.WriteString(`<?xml version="1.0"?><c:calendar-multiget xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><d:prop><d:getetag/><c:calendar-data/></d:prop>`)
		}
		for _, h := range hrefs[:n] {
			b.WriteString("<d:href>" + xmlEscape(h) + "</d:href>")
		}
		if col.Contacts {
			b.WriteString(`</card:addressbook-multiget>`)
		} else {
			b.WriteString(`</c:calendar-multiget>`)
		}
		rs, err := c.multistatus("REPORT", col.Href, "1", b.String())
		if err != nil {
			return nil, err
		}
		for _, r := range rs {
			p := r.prop200()
			data := p.CalData
			if col.Contacts {
				data = p.AddrData
			}
			if r.Href != "" && data != "" {
				out[r.Href] = [2]string{p.ETag, data}
			}
		}
		hrefs = hrefs[n:]
	}
	return out, nil
}

// Put пишет объект. etag "" — новый (If-None-Match: *). Возвращает новый
// ETag, если сервер его сказал.
func (c *client) Put(href, etag, data string) (string, error) {
	hdr := map[string]string{"Content-Type": "text/calendar; charset=utf-8"}
	if etag == "" {
		hdr["If-None-Match"] = "*"
	} else {
		hdr["If-Match"] = etag
	}
	resp, err := c.do("PUT", href, data, hdr)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusPreconditionFailed {
		return "", ErrConflict
	}
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("сервер не сохранил событие: %s", resp.Status)
	}
	return resp.Header.Get("ETag"), nil
}

// Delete удаляет объект, если его ETag не менялся.
func (c *client) Delete(href, etag string) error {
	hdr := map[string]string{}
	if etag != "" {
		hdr["If-Match"] = etag
	}
	resp, err := c.do("DELETE", href, "", hdr)
	if err != nil {
		return err
	}
	resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusPreconditionFailed:
		return ErrConflict
	case resp.StatusCode == http.StatusNotFound:
		return nil // уже удалено
	case resp.StatusCode >= 300:
		return fmt.Errorf("сервер не удалил событие: %s", resp.Status)
	}
	return nil
}
