package diag

// Все итоговые фразы диагностики — здесь, на двух языках. Код итога
// стабилен (API, история), текст — по языку интерфейса.

// verdict — код итога проверки сайта и какое правило предложить.
func verdict(d, v Path) (code, suggest string) {
	vOff := v.Fail == kOff
	switch {
	case d.OK && v.OK:
		return "ok_both", ""
	case d.OK && vOff:
		return "direct_ok_vpn_off", ""
	case !d.OK && vOff:
		if d.Fail == kDNSFail {
			return "no_such", ""
		}
		return "direct_fail_vpn_off", ""
	case d.OK && !v.OK:
		return "vpn_fail", "direct"
	case !d.OK && v.OK:
		switch d.Fail {
		case kDNSSpoof:
			return "blocked_dns", "vpn"
		case kTLSReset, kTLSTimeout:
			return "blocked_dpi", "vpn"
		case kHTTPStub:
			return "blocked_stub", "vpn"
		case kTLSCert:
			return "mitm", "vpn"
		}
		return "blocked", "vpn"
	case d.Fail == kDNSFail && v.Fail == kDNSFail:
		return "no_such", ""
	}
	return "down", ""
}

var phrases = map[string][2]string{
	"ok_both":             {"Сайт открывается и напрямую, и через VPN.", "The site opens both directly and via VPN."},
	"direct_ok_vpn_off":   {"Напрямую открывается. Через VPN не проверено: туннель выключен.", "Opens directly. Not checked via VPN: the tunnel is off."},
	"direct_fail_vpn_off": {"Напрямую не открывается. Включи туннель, чтобы проверить через VPN.", "Does not open directly. Turn the tunnel on to check via VPN."},
	"vpn_fail":            {"Напрямую открывается, а через VPN нет: сайт не пускает адрес сервера.", "Opens directly but not via VPN: the site does not let the server's address in."},
	"blocked":             {"Провайдер блокирует, через VPN открывается.", "Your ISP blocks it, it opens via VPN."},
	"blocked_dns":         {"Провайдер подменяет DNS, через VPN открывается.", "Your ISP spoofs DNS, it opens via VPN."},
	"blocked_dpi":         {"Провайдер блокирует (обрывает защищённое соединение), через VPN открывается.", "Your ISP blocks it (cuts the secure connection), it opens via VPN."},
	"blocked_stub":        {"Провайдер блокирует (показывает свою заглушку), через VPN открывается.", "Your ISP blocks it (shows its block page), it opens via VPN."},
	"mitm":                {"Напрямую подменяется сертификат (антивирус или провайдер), через VPN открывается.", "The certificate is replaced directly (antivirus or ISP), it opens via VPN."},
	"no_such":             {"Такого сайта нет: имя не находится.", "No such site: the name does not resolve."},
	"down":                {"Сайт не отвечает ни напрямую, ни через VPN — похоже, он лежит сам.", "The site does not respond either directly or via VPN — it seems to be down itself."},
}

// phrase — итог одной фразой на языке lang (ru по умолчанию).
func phrase(code, lang string) string {
	p, ok := phrases[code]
	if !ok {
		return code
	}
	if lang == "en" {
		return p[1]
	}
	return p[0]
}

// tr — фраза на нужном языке.
func tr(lang, ru, en string) string {
	if lang == "en" {
		return en
	}
	return ru
}
