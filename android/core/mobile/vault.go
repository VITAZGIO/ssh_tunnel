package mobile

// «Пароли» для автозаполнения Android (VaultFill.kt): найти логины для
// сайта или приложения, открыть хранилище главным паролем, запомнить новый.

import (
	"encoding/json"

	"sshtunnel/internal/vault"
)

// VaultUnlocked — открыто ли хранилище паролей.
func VaultUnlocked() bool { return vault.Unlocked() }

// VaultUnlock открывает хранилище; "" — открыто, иначе текст ошибки.
func VaultUnlock(master string) string {
	if err := vault.Unlock(master); err != nil {
		return err.Error()
	}
	return ""
}

// VaultMatchJSON — записи для сайта или пакета приложения:
// [{"title","login","pass"}]; закрыто или нет — "[]".
func VaultMatchJSON(site string) string {
	type item struct {
		Title string `json:"title"`
		Login string `json:"login"`
		Pass  string `json:"pass"`
	}
	out := []item{}
	for _, e := range vault.Match(site) {
		out = append(out, item{e.Title, e.Login, e.Pass})
	}
	b, _ := json.Marshal(out)
	return string(b)
}

// VaultSave запоминает логин и пароль, введённые в приложении (запрос
// «Сохранить?» автозаполнения); "" — сохранено.
func VaultSave(site, login, pass string) string {
	id := ""
	for _, e := range vault.Match(site) {
		if e.Login == login {
			id = e.ID
		}
	}
	if _, err := vault.Put(vault.Entry{ID: id, Site: site, Login: login, Pass: pass}); err != nil {
		return err.Error()
	}
	return ""
}
