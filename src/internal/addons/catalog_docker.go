package addons

// ТЗ 7 (заход 2): Docker на Linux — выбор по контейнерам, как они ходят в
// интернет. Логика — internal/docker, правила ядра — vpn/vpnlayer
// (docker_linux.go), страница — uiassets/addons/docker.html, глава Книги —
// docs/book/<язык>/app-docker.md. Отдельным файлом, как catalog_diag.go.

func init() {
	catalog = append(catalog, Manifest{
		ID:   "docker",
		Name: map[string]string{"ru": "Docker", "en": "Docker"},
		Icon: "deployed_code",
		Hint: map[string]string{
			"ru": "Для каждого контейнера: как компьютер, через сервер или напрямую. «Через сервер» без туннеля — без интернета, мимо не уходит. Нужна VPN-версия для Linux.",
			"en": "For each container: like the computer, via the server or direct. \"Via the server\" has no internet without the tunnel — nothing leaks past it. Needs the Linux VPN version.",
		},
		Book: "app-docker", Page: "addons/docker.html", Background: true, Platforms: []string{"linux"}, Permissions: []string{},
	})
}
