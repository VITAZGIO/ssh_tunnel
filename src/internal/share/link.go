package share

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// LinkPrefix — начало ссылки импорта. Её же несёт QR-код, который панель
// показывает при создании клиента: телефон открывает её сканером в
// приложении или системной камерой, на ПК её вставляют кнопкой «Вставить».
// Формат описан в docs/IMPORT_LINK.md.
const LinkPrefix = "sshtunnel://import?"

// linkVersion — версия содержимого параметра d. Новые необязательные поля
// добавляются без смены версии; меняется она, только если старое
// приложение прочитало бы новую ссылку неправильно.
const linkVersion = 1

// linkDoc — ровно то, что нужно для входа одного аккаунта, и ничего
// больше: порты прокси, фильтр и пул у нового сервера и так стандартные.
// Ключи короткие — от длины зависит плотность QR-кода.
type linkDoc struct {
	Name       string `json:"n,omitempty"`
	Host       string `json:"h"`
	SSHPort    int    `json:"p,omitempty"`
	User       string `json:"u"`
	Key        string `json:"k,omitempty"`
	Panel      string `json:"pn,omitempty"`
	ClientID   string `json:"c,omitempty"`
	DeviceName string `json:"dn,omitempty"`
}

// BuildLink собирает ссылку импорта sshtunnel://import?v=1&d=… из Doc.
// В неё попадают только поля входа (адрес, порт, пользователь, ключ) и
// привязка к панели; остальное приложение при импорте берёт по умолчанию.
func BuildLink(doc Doc) (string, error) {
	if doc.Host == "" {
		return "", errors.New("нет адреса сервера — ссылку не из чего собрать")
	}
	ld := linkDoc{
		Name: doc.Name, Host: doc.Host, SSHPort: doc.SSHPort, User: doc.User,
		Panel: doc.Panel, ClientID: doc.ClientID, DeviceName: doc.DeviceName,
	}
	if doc.SSHPort == 22 {
		ld.SSHPort = 0
	}
	if doc.KeyIncluded {
		ld.Key = doc.KeyContents
	}
	data, err := json.Marshal(ld)
	if err != nil {
		return "", fmt.Errorf("не удалось собрать ссылку: %w", err)
	}
	return fmt.Sprintf("%sv=%d&d=%s", LinkPrefix, linkVersion,
		base64.RawURLEncoding.EncodeToString(data)), nil
}

// IsLink — похож ли текст на ссылку импорта (регистр схемы не важен:
// некоторые сканеры и мессенджеры приводят её к верхнему).
func IsLink(text string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(text)), "sshtunnel://")
}

// ParseLink разбирает ссылку импорта в Doc с теми же значениями по
// умолчанию, что у нового сервера в программе (порты 1080/1081, пул 4,
// фильтр «всё через туннель»).
func ParseLink(text string) (Doc, error) {
	u, err := url.Parse(strings.TrimSpace(text))
	if err != nil || !strings.EqualFold(u.Scheme, "sshtunnel") || !strings.EqualFold(u.Host, "import") {
		return Doc{}, errors.New("это не ссылка импорта ssh_tunnel")
	}
	q := u.Query()
	if v := q.Get("v"); v != "" && v != fmt.Sprint(linkVersion) {
		return Doc{}, errors.New("ссылка из более новой версии ssh_tunnel — обнови программу")
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(q.Get("d"), "="))
	if err != nil || len(raw) == 0 {
		return Doc{}, errors.New("ссылка повреждена: скопируй её целиком")
	}
	var ld linkDoc
	if err := json.Unmarshal(raw, &ld); err != nil {
		return Doc{}, errors.New("ссылка повреждена: скопируй её целиком")
	}
	if ld.Host == "" {
		return Doc{}, errors.New("в ссылке нет адреса сервера")
	}
	if ld.SSHPort <= 0 {
		ld.SSHPort = 22
	}
	name := ld.Name
	if name == "" {
		name = ld.Host
	}
	return Doc{
		Format:      CurrentFormat,
		Name:        name,
		Host:        ld.Host,
		SSHPort:     ld.SSHPort,
		User:        ld.User,
		SocksPort:   1080,
		HTTPPort:    1081,
		PoolSize:    4,
		FilterMode:  "all",
		KeyIncluded: ld.Key != "",
		KeyContents: ld.Key,
		Panel:       ld.Panel,
		ClientID:    ld.ClientID,
		DeviceName:  ld.DeviceName,
	}, nil
}
