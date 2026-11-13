// Package devapps подключает приложения «устройств сети» (ТЗ 4): Карта
// сети, SSH, Файлы устройства. Их пакеты регистрируют себя в addons из
// init; этот пакет импортируют те, кто отдаёт API приложений (internal/webui,
// internal/uihost), — addons сам их импортировать не может (цикл).
package devapps

import (
	_ "sshtunnel/internal/backup"
	_ "sshtunnel/internal/devfiles"
	_ "sshtunnel/internal/mirror"
	_ "sshtunnel/internal/netmap"
	_ "sshtunnel/internal/sshapp"
)
