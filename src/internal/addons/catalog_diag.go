package addons

// Диагностика (ТЗ 3): логика — internal/diag, страницы —
// uiassets/addons/<id>.html, главы Книги — docs/book/<язык>/app-<id>.md.
// Отдельным файлом, чтобы каталог дописывали параллельно, не мешая друг
// другу. На Android пока недоступны: там нужны свои проверки службы VPN —
// вместе с переводом телефона на общий интерфейс.

var diagPlatforms = []string{"windows", "linux"}

func init() {
	catalog = append(catalog,
		Manifest{
			ID:   "sitecheck",
			Name: map[string]string{"ru": "Проверить сайт", "en": "Check a site"},
			Icon: "travel_explore",
			Hint: map[string]string{
				"ru": "Адрес → «Проверить»: сразу два пути — напрямую и через VPN, по шагам. Итог одной фразой и кнопка правила.",
				"en": "Address → \"Check\": two paths at once — direct and via VPN, step by step. A one-line verdict and a rule button.",
			},
			Book: "app-sitecheck", Page: "addons/sitecheck.html", Platforms: diagPlatforms, Needs: []string{"internet"}, Permissions: []string{},
		},
		Manifest{
			ID:   "meddlers",
			Name: map[string]string{"ru": "Кто трогает трафик", "en": "Who touches traffic"},
			Icon: "policy",
			Hint: map[string]string{
				"ru": "Ищет обходы DPI, чужие VPN, прокси, DNS, hosts и антивирусы. Останавливает только с твоего разрешения и сам возвращает, если не помогло.",
				"en": "Finds DPI bypasses, other VPNs, proxies, DNS, hosts and antiviruses. Stops things only with your permission and restores them if it did not help.",
			},
			Book: "app-meddlers", Page: "addons/meddlers.html", Platforms: diagPlatforms, Needs: []string{"internet"}, Permissions: []string{},
		},
		Manifest{
			ID:   "leaks",
			Name: map[string]string{"ru": "Проверка утечек", "en": "Leak check"},
			Icon: "visibility",
			Hint: map[string]string{
				"ru": "Какой адрес видят сайты, чей DNS отвечает, нет ли IPv6 мимо туннеля. Молча проверяет при каждом подключении и пишет только при утечке.",
				"en": "Which address sites see, whose DNS answers, whether IPv6 bypasses the tunnel. Checks silently on every connection, speaks up only on a leak.",
			},
			Book: "app-leaks", Page: "addons/leaks.html", Background: true, Platforms: diagPlatforms, Needs: []string{"internet"}, Permissions: []string{},
		},
		Manifest{
			ID:   "report",
			Name: map[string]string{"ru": "Отчёт для поддержки", "en": "Support report"},
			Icon: "bug_report",
			Hint: map[string]string{
				"ru": "Один файл: журнал, настройки без паролей и ключей, результаты проверок. Сначала покажет, что внутри.",
				"en": "One file: log, settings without passwords and keys, check results. Shows what is inside first.",
			},
			Book: "app-report", Page: "addons/report.html", Platforms: diagPlatforms, Permissions: []string{},
		},
	)
}
