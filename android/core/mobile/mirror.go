package mobile

// Зеркальные папки (ТЗ 6): само приложение общее (sshtunnel/internal/mirror)
// и работает в ядре, пока жива служба VPN. Отсюда телефону нужно только
// уведомление о конфликте.

import "sshtunnel/internal/mirror"

// MirrorListener — сообщения зеркальных папок для уведомления (конфликт).
type MirrorListener interface {
	OnMirror(text string)
}

// SetMirrorListener — nil снимает.
func SetMirrorListener(l MirrorListener) {
	if l == nil {
		mirror.SetNotify(nil)
		return
	}
	mirror.SetNotify(func(text string) { l.OnMirror(text) })
}
