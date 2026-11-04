package addons

// Блокировка рекламы (слой VPN, android/core), Удалённый стол (internal/rdp),
// Связь с телефоном (internal/phonelink), Пароли (internal/vault).

func init() {
	catalog = append(catalog,
		Manifest{
			ID:   "adblock",
			Name: map[string]string{"ru": "Блокировка рекламы", "en": "Ad blocking"},
			Icon: "block",
			Hint: map[string]string{
				"ru": "Реклама и слежка не грузятся ни в одной программе: имя из списка не получает адреса. Статистика — что и сколько отсечено. Работает в режиме VPN (телефон, VPN-версия на Windows и Linux).",
				"en": "Ads and trackers don't load in any app. Statistics of what was blocked. Works in VPN mode (phone, VPN build on Windows and Linux).",
			},
			Book: "app-adblock", Page: "addons/adblock.html", Permissions: []string{},
		},
		Manifest{
			ID:   "rdp",
			Name: map[string]string{"ru": "Удалённый стол", "en": "Remote desktop"},
			Icon: "desktop_windows",
			Hint: map[string]string{
				"ru": "Рабочий стол Windows-машины по RDP — с ПК и с телефона, из дома и не из дома, без проброса портов. Нужен ретранслятор RDP (guacd) в локальной сети.",
				"en": "A Windows desktop over RDP from a PC or phone, without port forwarding. Needs an RDP relay (guacd) on the local network.",
			},
			Book: "app-rdp", Page: "addons/rdp.html", Needs: []string{"relay", "mesh"}, Permissions: []string{},
		},
		Manifest{
			ID:   "phonelink",
			Name: map[string]string{"ru": "Связь с телефоном", "en": "Phone link"},
			Icon: "phonelink",
			Hint: map[string]string{
				"ru": "Уведомления телефона — на ПК, «Найти телефон», пульт музыки, телефон как тачпад и клавиатура для ПК, заряд батареи. Добавь на обоих устройствах.",
				"en": "Phone notifications on the PC, find my phone, music remote, the phone as a PC touchpad and keyboard, battery level. Add on both devices.",
			},
			Book: "app-phonelink", Page: "addons/phonelink.html", Background: true, Needs: []string{"mesh"}, Permissions: []string{},
		},
		Manifest{
			ID:   "vault",
			Name: map[string]string{"ru": "Пароли", "en": "Passwords"},
			Icon: "lock",
			Hint: map[string]string{
				"ru": "Свои пароли под одним главным паролем — на всех своих устройствах. Подставляет логин и пароль в приложениях телефона и в браузере на ПК (Яндекс Браузер, Chrome).",
				"en": "Your passwords under one master password on all your devices. Fills logins in phone apps and in the PC browser.",
			},
			Book: "app-vault", Page: "addons/vault.html", Background: true, Permissions: []string{},
		},
	)
}
