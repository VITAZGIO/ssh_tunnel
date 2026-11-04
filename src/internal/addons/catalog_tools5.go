package addons

// ТЗ 5: Расход трафика (internal/traffic), Сканер сети (internal/lanscan),
// Общий буфер обмена (internal/clipshare). Страницы — uiassets/addons/<id>.html,
// главы Книги — docs/book/<язык>/app-<id>.md. Отдельным файлом, чтобы каталог
// дописывали параллельно.

func init() {
	catalog = append(catalog,
		Manifest{
			ID:   "traffic",
			Name: map[string]string{"ru": "Расход трафика", "en": "Data usage"},
			Icon: "data_usage",
			Hint: map[string]string{
				"ru": "Сколько ушло через VPN, напрямую и в сеть устройств — по дням и по программам. Хранит 3 месяца. Лимит — по желанию, только предупреждает.",
				"en": "How much went via VPN, directly and to the device network — by day and by app. Keeps 3 months. An optional limit only warns.",
			},
			Book: "app-traffic", Page: "addons/traffic.html", Background: true, Window: true, Permissions: []string{},
		},
		Manifest{
			ID:   "lanscan",
			Name: map[string]string{"ru": "Сканер сети", "en": "Network scanner"},
			Icon: "radar",
			Hint: map[string]string{
				"ru": "Кто в твоей сети (адрес, MAC, имя, тип по портам), все Wi-Fi вокруг с загрузкой каналов и уровень сигнала, пока ходишь по дому.",
				"en": "Who is on your network (address, MAC, name, type by ports), all Wi-Fi around with channel load, and signal level as you walk around.",
			},
			Book: "app-lanscan", Page: "addons/lanscan.html", Window: true, Permissions: []string{},
		},
		Manifest{
			ID:   "wol",
			Name: map[string]string{"ru": "Включить по сети", "en": "Wake on LAN"},
			Icon: "power_settings_new",
			Hint: map[string]string{
				"ru": "Включить компьютер кнопкой (Wake-on-LAN) — и из дома, и с работы: пакет шлют твои устройства в той же локалке (через сеть устройств). WoL должен быть включён в BIOS.",
				"en": "Turn a computer on with a button (Wake-on-LAN): a list of your machines by MAC. Works on the same local network; WoL must be on in BIOS.",
			},
			Book: "app-wol", Page: "addons/wol.html", Background: true, Permissions: []string{},
		},
		Manifest{
			ID:   "clipboard",
			Name: map[string]string{"ru": "Общий буфер", "en": "Shared clipboard"},
			Icon: "content_paste",
			Hint: map[string]string{
				"ru": "Скопировал на ПК — вставил на телефоне. Текст, картинки и файлы до 50 МБ, только по сети устройств. С телефона — «Поделиться → ssh_tunnel» или плитка в шторке.",
				"en": "Copy on the PC — paste on the phone. Text, images and files up to 50 MB, only over the device network. From the phone — Share → ssh_tunnel or the quick tile.",
			},
			Book: "app-clipboard", Page: "addons/clipboard.html", Background: true, Needs: []string{"mesh"}, Permissions: []string{},
		},
	)
}
