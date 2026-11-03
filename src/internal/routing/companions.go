package routing

// Сайт — это не одно имя: страница yandex.ru тянет картинки и скрипты с
// yastatic.net, видео rutube.ru идёт с rtbcdn.ru. Человек вписывает в список
// сам сайт и ждёт, что он весь пойдёт как сказано; а спутники уходили другой
// дорогой (через сервер) — сайт грузился медленно или видео не шло вовсе.
// Поэтому у известных сайтов правило сразу покрывает и их спутники.

// companions — сайт → имена, с которых он грузит своё (с поддоменами).
var companions = map[string][]string{
	"yandex.ru":    {"yandex.net", "yastatic.net", "yandex.com", "ya.ru", "yandex.by", "yandex.kz", "yandexcloud.net", "yandex-team.ru"},
	"ya.ru":        {"yandex.ru", "yandex.net", "yastatic.net"},
	"kinopoisk.ru": {"kinopoisk.net", "yastatic.net", "yandex.net"},
	"dzen.ru":      {"dzeninfra.ru", "yastatic.net"},
	"rutube.ru":    {"rtbcdn.ru", "rutubelist.ru", "rutube.pro"},
	"vk.com":       {"userapi.com", "vk-cdn.net", "vkuser.net", "vk.me", "mycdn.me", "vkuseraudio.net", "vkuservideo.net"},
	"vk.ru":        {"userapi.com", "vk-cdn.net", "vkuser.net", "mycdn.me"},
	"vkvideo.ru":   {"userapi.com", "vk-cdn.net", "vkuser.net", "mycdn.me", "vkuservideo.net"},
	"ok.ru":        {"mycdn.me", "okcdn.ru"},
	"mail.ru":      {"imgsmail.ru", "mradx.net", "mycdn.me"},
	"gosuslugi.ru": {"esia.gosuslugi.ru"},
}

// withCompanions — записи списка плюс спутники известных сайтов из них.
func withCompanions(names []string) []string {
	out := names
	seen := make(map[string]bool, len(names))
	for _, n := range names {
		seen[n] = true
	}
	for _, n := range names {
		for _, c := range companions[n] {
			if !seen[c] {
				seen[c] = true
				out = append(out, c)
			}
		}
	}
	return out
}
