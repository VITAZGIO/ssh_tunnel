package devapps

// ТЗ 15: серверные плагины со стороны устройства — список блокировки с
// сервера сети и «Проверить сайт» с сервера. Отдельным файлом — каталог
// дописывают параллельно.
import (
	_ "sshtunnel/internal/adblock"
	_ "sshtunnel/internal/sitesrv"
)
