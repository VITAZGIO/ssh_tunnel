package panel

import (
	"encoding/base64"
	"errors"
	"fmt"

	qrcode "github.com/skip2/go-qrcode"

	"sshtunnel/internal/share"
)

// Значения по умолчанию для конфига клиента — те же порты и режим фильтра,
// что и у нового профиля в локальной программе (internal/config.defaultProfile):
// нет смысла придумывать другие числа только потому, что профиль собирает
// панель, а не сама программа при первом запуске.
const (
	defaultClientSocksPort = 1080
	defaultClientHTTPPort  = 1081
	defaultClientPoolSize  = 4
)

// BuildClientDoc собирает файл обмена настройками (internal/share, ТЗ-01)
// для одного клиента панели: приватный ключ клиента, адрес и порт SSH,
// которые панель считает адресом самого сервера, плюс поля Panel/ClientID/
// DeviceName версии 2 формата — по ним локальная панель и приложение на
// Android узнают, что сервер выдан именно этой панелью (ТЗ-12).
//
// sshHost — обязателен: без него конфиг не говорит клиенту, куда вообще
// подключаться. Задаётся флагами -ssh-host/-domain при запуске панели
// (cmd/ssh_tunnel_panel); если ни один не задан, вызывающий код должен
// сообщить об этом человеку заранее, а не звать эту функцию с пустой
// строкой.
func BuildClientDoc(c Client, sshHost string, sshPort int, panelURL string) (share.Doc, error) {
	if sshHost == "" {
		return share.Doc{}, errors.New("не задан адрес сервера для подключения " +
			"(флаг -ssh-host или -domain при запуске панели)")
	}
	if c.PrivateKey == "" {
		return share.Doc{}, errors.New("у клиента нет сохранённого приватного ключа")
	}
	if sshPort <= 0 {
		sshPort = 22
	}
	return share.Doc{
		Name:        c.Name,
		Host:        sshHost,
		SSHPort:     sshPort,
		User:        c.Username,
		SocksPort:   defaultClientSocksPort,
		HTTPPort:    defaultClientHTTPPort,
		PoolSize:    defaultClientPoolSize,
		FilterMode:  "all",
		KeyIncluded: true,
		KeyContents: c.PrivateKey,
		Panel:       panelURL,
		ClientID:    c.ID,
		DeviceName:  c.Name,
	}, nil
}

// ClientConfigPayload — то, что нужно панели, чтобы показать настройки
// клиента: сам JSON для копирования/скачивания и те же значения текстом — на
// случай, если их проще вписать руками. Ссылка импорта sshtunnel://… и её
// QR-код (PNG в base64) есть только в ответе на создание клиента: показывать
// их решено один раз, в мастере создания (ТЗ «QR в панели», п. 16).
type ClientConfigPayload struct {
	JSON        string     `json:"json"`
	Link        string     `json:"link,omitempty"`
	QRPngBase64 string     `json:"qrPngBase64,omitempty"`
	Host        string     `json:"host"`
	SSHPort     int        `json:"sshPort"`
	User        string     `json:"user"`
	PrivateKey  string     `json:"privateKey"`
	DeviceType  DeviceType `json:"deviceType"`
}

// BuildClientConfigPayload собирает ответ странице (server.go): JSON-файл
// обмена и отдельные текстовые поля для ручного ввода; с withLink — ещё
// ссылку импорта и её QR-код (только при создании клиента).
func BuildClientConfigPayload(c Client, sshHost string, sshPort int, panelURL string, withLink bool) (ClientConfigPayload, error) {
	doc, err := BuildClientDoc(c, sshHost, sshPort, panelURL)
	if err != nil {
		return ClientConfigPayload{}, err
	}
	payload, err := share.Build(doc)
	if err != nil {
		return ClientConfigPayload{}, err
	}
	out := ClientConfigPayload{
		JSON:        string(payload),
		Host:        doc.Host,
		SSHPort:     doc.SSHPort,
		User:        doc.User,
		PrivateKey:  c.PrivateKey,
		DeviceType:  c.DeviceType,
	}
	if withLink {
		link, err := share.BuildLink(doc)
		if err != nil {
			return ClientConfigPayload{}, err
		}
		// QR несёт ссылку, а не JSON: она короче (код реже, сканируется
		// легче), и системная камера телефона сразу предлагает открыть её в
		// приложении. 512 px — чтобы точки не сливались на экране ноутбука.
		png, err := qrcode.Encode(link, qrcode.Low, 512)
		if err != nil {
			return ClientConfigPayload{}, fmt.Errorf("не удалось собрать QR-код: %w", err)
		}
		out.Link = link
		out.QRPngBase64 = base64.StdEncoding.EncodeToString(png)
	}
	return out, nil
}
