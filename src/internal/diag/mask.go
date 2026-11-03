package diag

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"
)

// Маскирование секретов в отчёте: по списку полей И по виду значения
// («похоже на ключ»), плюс известные секреты (ключ API из api.json, ключ
// сети устройств) заменяются в любом тексте. Тест — report_test.go.

const masked = "***"

// secretField — имя поля, значение которого всегда секрет.
var secretField = regexp.MustCompile(`(?i)(pass|secret|token|key|private|auth|cookie|credential|session)`)

// notSecretField — поля с «key» в имени, которые секретом не являются
// (путь к файлу ключа — не ключ).
var notSecretField = map[string]bool{"keyPath": true, "keepAliveSec": true}

var (
	pemRe    = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]+-----[\s\S]*?-----END [A-Z0-9 ]+-----`)
	sshKeyRe = regexp.MustCompile(`\b(ssh-(?:ed25519|rsa|dss)|ecdsa-sha2-[a-z0-9-]+)\s+[A-Za-z0-9+/=]{20,}`)
	// Длинная строка из букв и цифр (base64, hex) — похоже на ключ или токен.
	tokenRe = regexp.MustCompile(`[A-Za-z0-9+/_\-=]{32,}`)
)

// looksSecret — значение похоже на ключ или токен.
func looksSecret(s string) bool {
	if pemRe.MatchString(s) || sshKeyRe.MatchString(s) {
		return true
	}
	for _, m := range tokenRe.FindAllString(s, -1) {
		if mixed(m) {
			return true
		}
	}
	return false
}

// mixed — в строке и буквы, и цифры (а не путь из одних букв).
func mixed(s string) bool {
	var letters, digits bool
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			digits = true
		case r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z':
			letters = true
		}
	}
	return letters && digits
}

// maskText прячет секреты в произвольном тексте (журнал, сводка).
func maskText(s string, known []string) string {
	for _, k := range known {
		if len(k) >= 6 {
			s = strings.ReplaceAll(s, k, masked)
		}
	}
	s = pemRe.ReplaceAllString(s, masked)
	s = sshKeyRe.ReplaceAllString(s, "$1 "+masked)
	return tokenRe.ReplaceAllStringFunc(s, func(m string) string {
		if mixed(m) {
			return masked
		}
		return m
	})
}

// maskJSON прячет секреты в JSON-документе и собирает их значения (чтобы
// потом вычистить те же значения и из текстов).
func maskJSON(v any, found *[]string) any {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			val := t[k]
			if s, ok := val.(string); ok && s != "" && secretField.MatchString(k) && !notSecretField[k] {
				*found = append(*found, s)
				t[k] = masked
				continue
			}
			t[k] = maskJSON(val, found)
		}
		return t
	case []any:
		for i := range t {
			t[i] = maskJSON(t[i], found)
		}
		return t
	case string:
		if looksSecret(t) {
			*found = append(*found, t)
			return masked
		}
		return t
	}
	return v
}

// maskDoc — любое значение → JSON с замаскированными секретами.
func maskDoc(v any, found *[]string) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var doc any
	if json.Unmarshal(b, &doc) != nil {
		return nil
	}
	doc = maskJSON(doc, found)
	out, _ := json.MarshalIndent(doc, "", "  ")
	return out
}
