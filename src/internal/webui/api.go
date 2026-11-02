package webui

// API ядра. Всё, что умеет экран, делается через эти адреса — и только
// через них: экран сам ничего не хранит и ничего не решает. На этом же API
// строятся команды PowerShell, MCP и сторонние программы.
//
// Доступ: ключ (токен) в заголовке «Authorization: Bearer <ключ>» или
// «X-Token», либо параметром ?t= в адресе. Адрес и ключ работающей программы
// лежат в файле api.json в папке настроек (см. writeDiscovery) — его может
// прочитать только сам пользователь. Наружу API не открывается: слушает
// 127.0.0.1 (кроме серверной версии с -web-lan, где пускает только локальную
// сеть). Описание с примерами — docs/API.md.

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"

	"sshtunnel/internal/config"
	"sshtunnel/internal/updater"
)

// APIVersion — версия API. Растёт, когда меняется смысл существующих адресов;
// новые адреса версию не меняют.
const APIVersion = 2

// route — один адрес API: что делает и кто его обслуживает. Из этой же
// таблицы собирается самоописание GET /api — описание не может разойтись с
// тем, что есть на самом деле.
type route struct {
	Path    string `json:"path"`
	Methods string `json:"methods"`
	Desc    string `json:"desc"`
	h       http.HandlerFunc
}

func (s *Server) routes() []route {
	return []route{
		{"/api", "GET", "Самоописание: версия API и список всех адресов.", s.handleAPIIndex},
		{"/api/state", "GET, POST", "Состояние ядра: две кнопки (tunnel, mesh), режим, связь, модули. POST {\"tunnel\":bool,\"mesh\":bool} — «хочу» (любую можно не указывать).", s.handleState},
		{"/events", "GET", "Поток событий (Server-Sent Events): core — состояние ядра, state — связь, conn — соединения, log — журнал, stats, speed, vpssetup.", s.handleEvents},
		{"/api/status", "GET", "Всё для экрана разом: связь, настройки, счётчики, версия, режимы, реклама.", s.handleStatus},
		{"/api/start", "POST", "Включить круглую кнопку (туннель) и дождаться результата. Сеть устройств не трогается.", s.handleStart},
		{"/api/stop", "POST", "«Выключено»: выключить обе кнопки.", s.handleStop},
		{"/api/bypass", "POST", "Круглая кнопка: {\"on\":bool}. Ждёт результата.", s.handleBypass},
		{"/api/meshmode", "POST", "Кнопка «Сеть устройств»: {\"on\":bool}. Ждёт результата.", s.handleMeshMode},
		{"/api/config", "GET, POST", "Настройки целиком (config.Config). POST сохраняет; правила применяются сразу.", s.handleConfig},
		{"/api/profile/add", "POST", "Новый сервер {\"name\",\"flag\"}; активным не становится.", s.handleProfileAdd},
		{"/api/profile/remove", "POST", "Удалить сервер {\"id\"}.", s.handleProfileRemove},
		{"/api/profile/select", "POST", "Сделать сервер активным {\"id\"}; включённое переподключается к нему.", s.handleProfileSelect},
		{"/api/profile/export", "POST", "Сервер одним файлом (JSON) для переноса {\"id\",\"withKey\"}.", s.handleProfileExport},
		{"/api/profile/import", "POST", "Добавить сервер из файла экспорта и сделать активным.", s.handleProfileImport},
		{"/api/profile/latency", "GET", "Отклик всех серверов (мс).", s.handleProfileLatency},
		{"/api/settings/export", "GET", "Все настройки одним текстом (с ключами) — перенос на другое устройство.", s.handleSettingsExport},
		{"/api/settings/import", "POST", "Заменить все настройки выгрузкой.", s.handleSettingsImport},
		{"/api/appprofiles", "GET, POST", "Профили приложения: список; POST — сохранить текущие настройки профилем.", s.handleAppProfiles},
		{"/api/appprofiles/", "POST", "Действие над профилем: /api/appprofiles/<id>/apply | /delete | /rename.", s.handleAppProfilesAction},
		{"/api/androidkey", "POST", "Ключ и настройки для телефона (QR / текст).", s.handleAndroidKey},
		{"/api/vpsscript", "GET", "Скрипт настройки сервера для ручного запуска.", s.handleVPSScript},
		{"/api/vpssetup/start", "POST", "Мастер настройки VPS (ход — событиями vpssetup).", s.handleVpsSetupStart},
		{"/api/serverusers", "POST", "Пользователи на сервере: {\"op\":\"whoami\"|\"create\",…}.", s.handleServerUsers},
		{"/api/checkip", "GET", "Внешний адрес через туннель.", s.handleCheckIP},
		{"/api/speedtest", "POST", "Тест скорости туннеля (ход — событиями speed).", s.handleSpeedTest},
		{"/api/selfcheck", "POST", "Самопроверка цепочки до сервера по шагам.", s.handleSelfCheck},
		{"/api/scannet", "GET", "Сети этого компьютера и что с ними будет при включении туннеля.", s.handleScanNet},
		{"/api/lanaddrs", "GET", "Адреса компьютера в локальной сети.", s.handleLANAddrs},
		{"/api/mesh", "GET", "Сеть устройств: кто в сети, имена, адреса, прямая связь или через сервер.", s.handleMesh},
		{"/api/processes", "GET", "Запущенные программы (для правил по программам).", s.handleProcesses},
		{"/api/appicon", "GET", "Значок программы по пути: ?path=… (PNG, Windows).", s.handleAppIcon},
		{"/api/pickfile", "POST", "Системный диалог выбора программы (Windows).", s.handlePickFile},
		{"/api/openterminal", "POST", "Открыть PowerShell (Windows).", s.handleOpenTerminal},
		{"/api/genkey", "POST", "Создать SSH-ключ по пути и отдать открытую часть.", s.handleGenKey},
		{"/api/bootstart", "GET, POST", "Запуск при старте системы: состояние; POST {\"on\":bool}.", s.handleBootStart},
		{"/api/update/check", "GET", "Есть ли новая версия на GitHub.", s.handleUpdateCheck},
		{"/api/update/download", "POST", "Скачать новую версию в «Загрузки».", s.handleUpdateDownload},
		{"/api/adblock/update", "POST", "Скачать и применить списки блокировки рекламы {\"sources\"}.", s.handleAdBlockUpdate},
	}
}

func (s *Server) handleAPIIndex(w http.ResponseWriter, r *http.Request) {
	rs := s.routes()
	sort.Slice(rs, func(i, j int) bool { return rs[i].Path < rs[j].Path })
	writeJSON(w, map[string]any{
		"api":     APIVersion,
		"version": updater.Version,
		"auth":    "Authorization: Bearer <token> | X-Token: <token> | ?t=<token>",
		"routes":  rs,
	})
}

// handleState — главное API ядра: GET отдаёт состояние, POST — «хочу».
// Ответ POST — состояние сразу после просьбы (обычно «подключаюсь»); чем всё
// кончилось, приходит событием core или следующим GET.
func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var req struct {
			Tunnel *bool `json:"tunnel"`
			Mesh   *bool `json:"mesh"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, map[string]string{"error": "нужен JSON вида {\"tunnel\":true,\"mesh\":false}"})
			return
		}
		if err := s.app.Want(req.Tunnel, req.Mesh); err != nil {
			writeJSON(w, map[string]any{"error": err.Error(), "state": s.app.CoreState()})
			return
		}
	}
	writeJSON(w, s.app.CoreState())
}

// discovery — содержимое api.json: где искать API работающей программы.
type discovery struct {
	URL     string `json:"url"`
	Token   string `json:"token"`
	API     int    `json:"api"`
	Version string `json:"version"`
	PID     int    `json:"pid"`
}

// DiscoveryPath — где лежит api.json.
func DiscoveryPath() string { return filepath.Join(config.Dir(), "api.json") }

// writeDiscovery кладёт адрес и ключ API в api.json: по нему команды
// PowerShell, MCP и другие программы находят работающую программу. Файл в
// папке настроек пользователя — чужие учётные записи его не прочтут.
func (s *Server) writeDiscovery() {
	host := s.ln.Addr().String()
	d := discovery{URL: "http://" + displayAddr(host), Token: s.token, API: APIVersion, Version: updater.Version, PID: os.Getpid()}
	b, _ := json.MarshalIndent(d, "", "  ")
	_ = os.MkdirAll(config.Dir(), 0o700)
	_ = os.WriteFile(DiscoveryPath(), b, 0o600)
}

// removeDiscovery убирает api.json, если он ещё наш.
func (s *Server) removeDiscovery() {
	b, err := os.ReadFile(DiscoveryPath())
	if err != nil {
		return
	}
	var d discovery
	if json.Unmarshal(b, &d) == nil && d.Token == s.token {
		os.Remove(DiscoveryPath())
	}
}
