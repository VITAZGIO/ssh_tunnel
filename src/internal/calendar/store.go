package calendar

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// Account — куда ходить за календарями. Пароль приложения Nextcloud лежит
// отдельно и зашифрованным (secret_*.go), в calendar.json его нет.
type Account struct {
	URL  string `json:"url"`
	User string `json:"user"`
	// Contacts — синхронизировать и контакты (CardDAV). По умолчанию выкл.
	Contacts bool `json:"contacts,omitempty"`
	// Kind — чем подключён: "" — CalDAV (Nextcloud, Яндекс, iCloud…),
	// "ha" — Home Assistant по токену (ha.go; токен лежит вместо пароля).
	Kind string `json:"kind,omitempty"`
}

// item — объект на сервере (событие .ics или контакт .vcf) как есть.
type item struct {
	ETag string `json:"etag"`
	Data string `json:"data"`
}

type colState struct {
	Collection
	Token string `json:"token,omitempty"`
	// Hidden — не показывать в своём виде и виджете (синхронизируется всё
	// равно).
	Hidden bool            `json:"hidden,omitempty"`
	Items  map[string]item `json:"items"`
}

// state — всё, что календарь помнит на этом устройстве (calendar.json).
type state struct {
	Account     Account     `json:"account"`
	Collections []*colState `json:"collections"`
	// Widget — на ПК показывать виджет на рабочем столе (открывается при
	// запуске программы).
	Widget    bool      `json:"widget,omitempty"`
	LastSync  time.Time `json:"lastSync,omitempty"`
	LastError string    `json:"lastError,omitempty"`
	// Conflict — последняя правка не прошла: событие изменили в другом месте.
	Conflict string `json:"conflict,omitempty"`
	// Rev растёт на каждом изменении данных: по нему телефон понимает, что
	// пора переписать системный календарь и виджет.
	Rev int64 `json:"rev"`
}

func (st *state) col(href string) *colState {
	for _, c := range st.Collections {
		if sameHref(c.Href, href) {
			return c
		}
	}
	return nil
}

func statePath(dir string) string  { return filepath.Join(dir, "calendar.json") }
func secretPath(dir string) string { return filepath.Join(dir, "calendar.key") }

func loadState(dir string) *state {
	st := &state{}
	if b, err := os.ReadFile(statePath(dir)); err == nil {
		_ = json.Unmarshal(b, st)
	}
	for _, c := range st.Collections {
		if c.Items == nil {
			c.Items = map[string]item{}
		}
	}
	return st
}

// save пишет сразу и целиком: всё должно пережить перезапуск и падение.
func (st *state) save(dir string) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp := statePath(dir) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, statePath(dir))
}

func savePassword(dir, pass string) error {
	enc, err := protect([]byte(pass))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(secretPath(dir), enc, 0o600)
}

func loadPassword(dir string) string {
	b, err := os.ReadFile(secretPath(dir))
	if err != nil {
		return ""
	}
	p, err := unprotect(b)
	if err != nil {
		return ""
	}
	return string(p)
}
