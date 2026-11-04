package devapps

// Удалённый стол (RDP через guacd), Связь с телефоном, Пароли. Блокировка
// рекламы — без своего пакета: её делает слой VPN (android/core).
import (
	_ "sshtunnel/internal/machines"
	_ "sshtunnel/internal/phonelink"
	_ "sshtunnel/internal/rdp"
	_ "sshtunnel/internal/vault"
)
