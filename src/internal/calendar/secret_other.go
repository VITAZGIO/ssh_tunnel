//go:build !windows

package calendar

// protect на Linux и Android: файл с правами 600 в личной папке программы
// (на Android — папка приложения, другим приложениям недоступна). Связку
// ключей Linux и Keystore Android подключим вместе с общим хранилищем
// секретов (ТЗ 4), см. docs/decisions.md.
func protect(data []byte) ([]byte, error)   { return data, nil }
func unprotect(data []byte) ([]byte, error) { return data, nil }
