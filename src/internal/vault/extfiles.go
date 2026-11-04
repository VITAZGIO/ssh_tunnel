package vault

// Файлы расширения браузера — внутри программы; «Установить в браузер»
// выкладывает их папкой в «Загрузки», а браузер подключает её как
// распакованное расширение (Яндекс Браузер: browser://extensions → режим
// разработчика → «Загрузить распакованное»).

import (
	"embed"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
)

//go:embed ext/*
var extFiles embed.FS

func installExt() (string, error) {
	if runtime.GOOS == "android" {
		return "", errors.New("расширение — для браузера на компьютере")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, "Downloads", "ssh_tunnel-passwords-extension")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	err = fs.WalkDir(extFiles, "ext", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := extFiles.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, filepath.Base(p)), b, 0o644)
	})
	return dir, err
}
