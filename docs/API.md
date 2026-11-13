# API ядра

Всё, что умеет окно программы, делается через этот API — окно и само
пользуется только им. На нём же строятся команды PowerShell, MCP и сторонние
программы. Полный список адресов с описанием отдаёт сама программа:
`GET /api`.

## Где и с каким ключом

Работающая программа кладёт адрес и ключ в `api.json` в папке настроек
(Windows: `%APPDATA%\ssh_tunnel\api.json`, Linux: `~/.config/ssh_tunnel/api.json`):

```json
{ "url": "http://127.0.0.1:53412", "token": "…", "api": 2, "version": "v1.9.0", "pid": 1234 }
```

Ключ передаётся заголовком `Authorization: Bearer <ключ>` (или `X-Token`,
или параметром `?t=`). API слушает только `127.0.0.1` (серверная версия с
`-web-lan` — ещё и локальную сеть). Файл читает только сам пользователь.

## Главное: две кнопки

`GET /api/state` — состояние ядра:

```json
{
  "tunnel": { "want": true, "phase": "on" },
  "mesh":   { "want": false, "phase": "error", "error": "…", "errorKind": "no_response" },
  "meshAvailable": true,
  "mode": "bypass",
  "link": "connected",
  "detail": "203.0.113.1:22",
  "modules": [ { "id": "apply", "state": "error", "text": "…" } ],
  "seq": 42
}
```

- `phase`: `off`, `connecting` («Подключаюсь…»), `on`, `error` (выключилась
  сама, причина в `error`, код в `errorKind`).
- `mode`: `off` — «Выключено», `mesh` — «Только сеть устройств», `bypass` —
  «Обход», `bypass+mesh` — «Обход + сеть устройств».
- `link`: связь с сервером — `stopped`, `connecting`, `connected`,
  `reconnecting`, `error`.
- `meshLink`, `meshDirect` (только пока «Сеть устройств» включена): как
  живёт сеть — `online` (через сервер), `local` (сервера нет, на прямой связи
  `meshDirect` устройств), `error` (нет ни сервера, ни соседей),
  `connecting`. Сеть устройств работает и без сервера: тогда `tunnel` —
  `error` с причиной, `mesh` — `on`, `mode` — `mesh`, `link` —
  `reconnecting` (сервер ищется в фоне).
- `exit` (только у сервера с цепочкой выхода — см. «Цепочка выхода» ниже,
  появляется только при поднятой связи): `{"path":["Франкфурт","Лондон"],
  "state":"up"|"connecting"|"down","hop":"Лондон","error":"…",
  "errorKind":"…"}`. `down` — последний сервер цепочки недоступен: интернета
  нет, в обход через этот сервер трафик не идёт, хотя `tunnel.phase` может
  оставаться `on` (локальная часть и сеть устройств не зависят от цепочки).

`POST /api/state` с `{"tunnel": true}`, `{"mesh": false}` или обоими — «хочу».
Каждая кнопка меняет только своё. Ответ — состояние сразу после просьбы
(обычно `connecting`); чем кончилось — событием `core` или следующим `GET`.
`{"tunnel": false}` во время `connecting` — отмена подключения.

## События

`GET /events` — поток Server-Sent Events, по строке `data: {…}` на событие:
`core` (поле `core` — то же, что `GET /api/state`), `state` (связь), `conn`
(соединение: программа, цель, напрямую ли), `log`, `stats`, `speed`,
`vpssetup`. При подключении сначала приходит недавний журнал и текущее `core`.

## Плагины

`GET /api/addons?lang=ru` — каталог этой сборки и что добавлено на этом
устройстве (список свой на каждом устройстве, хранит ядро в `addons.json`):

```json
{
  "catalog": [ { "id": "sample", "name": {"ru":"Образец","en":"Sample"}, "icon": "science",
                 "hint": {…}, "book": "app-sample", "background": true, "window": false,
                 "permissions": [], "page": "addons/sample.html" } ],
  "added":   [ { "id": "sample", "star": true, "on": true, "active": true, "seen": false } ],
  "windows": true,
  "platform": "windows"
}
```

- `unavailable` у приложения каталога — причина, почему оно не работает на
  этом устройстве (показывается серым). `active` — приложение работает:
  добавлено, доступно и (если фоновое) включено. Только такое открывается
  (`/addons/<id>.html`) и запускает свой модуль ядра.
- `POST /api/addons` с `{"op":"add|remove|star|on|seen|win","id":"…","value":bool}`
  — добавить, убрать, звёздочка, выключатель фонового, «подсказку видел»,
  «сразу в окне» (`win`: такое приложение из быстрого доступа и списка
  открывается отдельным окном).
  Ответ — то же, что `GET`.
- `POST /api/window` с `{"key":"book","title":"…","url":"/book.html?…"}` —
  отдельное окно (ПК). Место и размер окна запоминаются по `key`. `{"ok":false}`
  без ошибки — окно этого не умеет (браузер), страница открывает окно сама.
  `{"key":"…","close":true}` — закрыть отдельное окно (возврат плагина в
  главное); без нативных окон ответ тоже `{"ok":false}`.

## Книга

`GET /api/book?lang=ru|en` — все главы разом: `{"lang","chapters":[{"id","title","md"}],"github"}`.
Текст — Markdown из `docs/book/`, главы приложений каталога стоят перед
«Частыми проблемами».

## Телефон

На Android тот же API отдаёт сервер страниц ядра (`internal/uihost`, адрес и
ключ знает только само приложение): Книга и приложения — Go, остальное —
`AndroidApi.kt` (те же адреса и JSON, `/api/status` → `"os":"android"`).
Только на телефоне:

- `POST /api/key` `{"id":"<сервер>","key":"-----BEGIN …"}` — сохранить
  приватный ключ сервера (пути к файлу у человека на телефоне нет; в
  профиле — `keySaved`).
- `GET /api/processes` — установленные приложения `[{name: пакет, label}]`,
  `GET /api/appicon?pkg=<пакет>` — значок PNG.
- `/api/update/download` скачивает APK и открывает установщик.

Настольного (`/api/bootstart`, `/api/pickfile`, `/api/vpssetup/*`, `/api/window`
…) на телефоне нет — ответ `{"error":…}`, экран эти пункты не показывает.

## Диагностика

Отвечают, только пока соответствующее приложение добавлено и включено (иначе
404). Язык итогов — `?lang=ru|en`.

- `POST /api/diag/site` `{"site":"youtube.com"}` — проверка двумя путями
  (`direct`, `vpn`), у каждого шаги `dns`, `tcp`, `tls`, `http` с `ok`, `ms`,
  `kind` (что сломалось: `dns_spoof`, `tls_reset` — DPI, `http_stub` —
  заглушка…). `verdict` — код итога, `text` — итог фразой, `suggest` —
  `vpn`/`direct`: какое правило предложить (правило добавляется обычным
  `POST /api/config`). `GET` — 10 последних проверок и список «популярных».
- `POST /api/diag/site/popular` `{"list":[…]}` — сохранить список,
  `{"check":true}` — проверить все.
- `GET|POST /api/diag/meddlers` — кто трогает трафик (последний поиск / искать
  заново) и `disabled` — «Что я отключал».
  `POST /api/diag/meddlers/try {"id","site"}` — это и есть «Разрешаю»:
  остановить, перепроверить, не помогло — вернуть само. `/keep {"id"}` —
  отключить насовсем (только автозапуск), `/restore {"id"}`, `/reconcile
  {"id","site"}` — помирить (исключить сервер VPN в списках zapret).
- `GET|POST /api/diag/leaks` — утечки: что видят сайты, чей DNS, IPv6,
  `fixes` — что включить через `/api/config`, `webrtc` — страница на панели.
- `GET /api/diag/report` — файлы отчёта (секреты уже `***`);
  `POST {"op":"save"|"share"}` — zip в «Загрузки» / поделиться.

Панель VPS: `GET /ip` — `{"ip"}` спрашивающего, `GET /webrtc` — страница
проверки WebRTC. Обе без входа.

## Остальное

Настройки (`/api/config`), серверы (`/api/profile/*`), профили приложения,
сеть устройств, самопроверка, тест скорости, обновления, мастер настройки VPS
и прочее — см. `GET /api`: там у каждого адреса методы и короткое описание.
У опасных адресов (выключить сеть устройств, удалить, остановить чужую
программу, заменить настройки) в самоописании ещё поле `confirm` — что
именно опасно; MCP пишет это нейросети «спроси человека».

## Правила (ТЗ 8)

`GET /api/rules` — правила активного сервера без остальных настроек и
ключей: `{"profile","filterMode","filterApps","directHosts","tunnelHosts"}`.
`POST` `{"op":"add"|"remove","list":"direct"|"tunnel"|"apps","value":"youtube.com"}`
или `{"filterMode":"all"|"only"|"except"}` — применяется сразу, ответ
`{"ok":true,"rules",…}`. Те же поля, что в `/api/config` (профиль сервера).

DNS режима VPN (ТЗ 9) — там же, но общие для всех серверов (`/api/config`:
`fakeIP`, `realIPHosts`). В ответе `GET /api/rules` поля `fakeIP` (галочка
«Подставные адреса», по умолчанию `false` — программы получают настоящие
адреса, разрешённые через сервер) и `realIPHosts` («всегда настоящий адрес»).
`POST {"fakeIP":true|false}` — включить/выключить подставные адреса,
`{"op":"add"|"remove","list":"realip","value":".deepseek.com"}` — список.
Смена применяется сразу, без перезагрузки: ядро сбрасывает таблицу
подставных адресов, DNS-кэш системы и соединения из адаптера.
Команды: `ssh_tunnel rules fakeip on|off`, `rules add realip <имя>`; MCP —
`add_rule`/`remove_rule` со списком `realip`.

## Цепочка выхода

Профиль сервера может держать «Выход через» — до двух других серверов из
списка, по порядку: подключаемся к этому серверу, а в интернет выходим
только через последний в цепочке (см. `docs/VPN.md`). Поле профиля
`exitVia` — список `id` серверов; `[]` или отсутствие — без цепочки.

`POST /api/profile/exit` `{"id":"<сервер>","via":["<id>",…]}` — задать или
снять (`"via":[]`) цепочку, с проверкой (сервер есть, не сам на себя, без
повторов, у каждого указан адрес). Ответ `{"ok":true,"note","config"}`.
Путь и состояние живой цепочки — `exit` в `GET /api/state` (выше). Команда:
`ssh_tunnel exit <сервер> <через> [<через>]`, `exit <сервер> off`, `exit`
без аргументов — цепочки всех серверов.

Экспорт сервера (`/api/profile/export`, `/api/settings/export`) переносит
цепочку ссылками (адрес, порт, пользователь — без ключей чужих серверов);
импорт находит такой же сервер в списке получателя по этим полям. Не нашёл
— цепочка не ставится, и ответ говорит, какого сервера не хватило.

## Свои приложения (ТЗ 8)

Формат архива и разрешения — `docs/CUSTOM_APPS.md`.

- `POST /api/addons/file[?dry=1]` — тело zip: `dry` — проверить и показать
  разрешения, без него — поставить. Только ключом программы.
- В `GET /api/addons` у своих приложений `custom: true`, `page` и `iconUrl` —
  адреса `/custom/<id>/<ключ приложения>/…`, `permissions` — что разрешено.
  `{"op":"remove"}` удаляет своё приложение вместе с данными.
- Своё приложение ходит к API своим ключом (`app_…`) и только по своим
  разрешениям, иначе `403 {"error"}`. `GET|POST /api/app/data` — его данные.

## Команды (ТЗ 8)

`ssh_tunnel.exe <команда>` (Linux — `ssh_tunnel_linux`, VPN —
`ssh_tunnel_vpn.exe`) — обёртка над этим API, адрес и ключ — из `api.json`
(код — `src/internal/apicli`). `--json` — ответ как есть.

```powershell
ssh_tunnel.exe state                         # что сейчас
ssh_tunnel.exe tunnel off                    # выключить туннель (сеть устройств не трогается)
ssh_tunnel.exe mesh on
ssh_tunnel.exe rules add direct youtube.com  # всегда напрямую
ssh_tunnel.exe apps add calendar
ssh_tunnel.exe check site youtube.com
ssh_tunnel.exe diag site site=youtube.com    # любой адрес: слова пути + ключ=значение
ssh_tunnel.exe api POST /api/state '{"mesh":false}'
$s = ssh_tunnel.exe state --json | ConvertFrom-Json
```

Любой адрес из `GET /api` — команда: слова пути после `/api/`, дальше
`ключ=значение` или один JSON (есть поля и адрес умеет POST — POST, иначе
GET с полями в адресе). `help` печатает все адреса работающей версии. Коды
выхода: 0, 1 — ошибка, 2 — не так вызвано, 3 — программа не запущена.

## MCP (ТЗ 8)

`ssh_tunnel.exe mcp` — MCP-сервер по stdio (JSON-RPC построчно), наружу не
слушает. Работает, только пока в настройках включена галочка «Управление
нейросетью (MCP)» (`config.mcpEnabled`, по умолчанию выкл., проверяется на
каждом вызове). Инструменты: `get_state`, `set_tunnel`, `set_mesh`,
`get_rules`, `add_rule`, `remove_rule`, `list_apps`, `app_action` и
`api_<путь>` на каждый адрес `GET /api` (`method`, `body`, `query`,
`suffix`). Опасные — с «ОПАСНО — спроси человека» в описании и
`destructiveHint`. Подключение к Claude — глава Книги «Команды и нейросеть».

## PowerShell

```powershell
$a = Get-Content "$env:APPDATA\ssh_tunnel\api.json" | ConvertFrom-Json
$h = @{ Authorization = "Bearer $($a.token)" }
Invoke-RestMethod "$($a.url)/api/state" -Headers $h                    # что сейчас
Invoke-RestMethod "$($a.url)/api/state" -Headers $h -Method Post `
  -ContentType 'application/json' -Body '{"tunnel":true}'              # включить туннель
Invoke-RestMethod "$($a.url)/api" -Headers $h | Select-Object -Expand routes   # все адреса
```

## curl (Linux)

```bash
A=~/.config/ssh_tunnel/api.json
URL=$(jq -r .url $A); TOK=$(jq -r .token $A)
curl -s -H "Authorization: Bearer $TOK" $URL/api/state
curl -s -H "Authorization: Bearer $TOK" -d '{"mesh":true}' $URL/api/state
```

## Календарь

Отвечает, только пока добавлено и включено приложение «Календарь» (иначе
404). Пароль приложения Nextcloud в ответах не бывает никогда.

- `GET /api/calendar` — аккаунт (`url`, `user`, `contacts`), `hasPassword`,
  календари (`href`, `name`, `color`, `readOnly`, `shown`, `count`), `busy`,
  `lastSync`, `lastError`, `conflict`, `widget`, `rev` (растёт на каждом
  изменении данных).
- `POST /api/calendar/account` — `{"url","user","password","contacts":bool}`:
  проверить вход и сохранить (`password` пустой — прежний);
  `{"forget":true}` — отключить и забыть скачанное.
- `POST /api/calendar/sync` — сверить сейчас (ответ — после сверки).
- `GET /api/calendar/events?from=…&to=…` — события видимых календарей,
  повторы развёрнуты. `from`/`to` — дата `2026-11-01` или RFC 3339; `&all=1`
  — и скрытых.
- `POST /api/calendar/event` — `{"cal","href","title","start","end","allDay","location","description"}`:
  без `href` — новое событие в календаре `cal`. Время — RFC 3339; у события
  на весь день — полночь UTC, `end` — следующий день после последнего. Ответ
  `{"ok":true,"href"}` или `{"error","conflict":true}` — событие изменили в
  другом месте, показана версия сервера.
- `POST /api/calendar/delete` — `{"href"}` (у повторяющегося — вся серия).
- `POST /api/calendar/settings` — `{"cal","shown":bool}` или `{"widget":bool}`.

```powershell
Invoke-RestMethod "$($a.url)/api/calendar/events?from=2026-11-01&to=2026-11-08" -Headers $h |
  Select-Object -Expand events | Format-Table start, title
```

## Устройства сети (ТЗ 4)

Отвечают, только пока приложение добавлено и включено.

- `GET /api/netmap` — карта: `{"state","self","nodes":[{name,host,ip,platform,os,online,lastSeen,direct,rttMs,server}]}`.
- `GET /api/ssh` — серверы этого устройства и открытый ключ программы;
  `POST /api/ssh` `{"op":"save","server":{…}}`, `{"op":"delete"|"forgethost","id"}`.
  `GET /api/ssh/devices` — устройства сети для анкеты.
  `POST /api/ssh/install` `{"id","password"}` — поставить ключ (пароль не хранится).
  `POST /api/ssh/open` `{"id"}` — внешний терминал (`{"builtin":true}` — его нет).
  `POST /api/ssh/term` (`start`/`in`/`resize`/`close`) и `GET /api/ssh/term/out?sid=`
  (SSE) — встроенный терминал.
- `GET|POST /api/files/shares` — свои открытые папки (`add`, `remove`,
  `write`); `GET /api/files/devices`; `/api/files/remote?peer=<198.19.x.y>&x=shares|list|get|part|put|op`
  — файлы другого устройства (Range передаётся); `POST /api/files/save`
  `{"peer","s","p"}` — скачать в «Загрузки» с докачкой.

## Зеркальные папки (ТЗ 6)

Отвечают, только пока приложение добавлено и включено. Раздаваемая папка
этого устройства — `id` вида `hub:<id>`, подключённая — её `id`.

- `GET /api/mirror` — `{"serve":[{id,name,path,ignore,files,conflicts}],
  "subs":[{id,peer,host,folder,name,path,mode,status:{state,pending,conflicts,files,error,lastOk}}],
  "found":[{peer,host,folders:[{id,name}],joined}],"self","suggest","platform","devnet"}`.
  `state`: `ok` | `pending` | `conflict` | `offline` | `error`.
- `POST /api/mirror`: `{"op":"serve","path","name"}`, `{"op":"unserve","id"}`,
  `{"op":"ignore","id","list":[шаблоны]}`, `{"op":"join","peer","host","folder","name","path","mode":"both|recv|send"}`,
  `{"op":"leave","id"}` (файлы остаются), `{"op":"mode","id","mode"}`,
  `{"op":"sync","id"}`, `{"op":"resolve","id","path":<копия конфликта>,"keep":"orig|copy|both"}`,
  `{"op":"restore","id","blob"}`.
- `GET /api/mirror/versions?id=[&p=]` — версии и корзина (30 дней), новые сверху.
- `GET /api/mirror/file?id=&p=` или `?id=&blob=` — текст файла или версии
  для просмотра (до 2 МБ).

Между устройствами — свой протокол на порту 47836 сети устройств
(`internal/mirror/proto.go`).

## Трафик, сканер, буфер (ТЗ 5)

Отвечают, только пока приложение добавлено (и, если фоновое, включено).

- `GET /api/traffic?period=day|week|month&net=all|wifi|mobile` — расход:
  `days` (`date`, `server`, `direct`, `mesh` — байты), `total`, `top`
  (программы: `name`, байты по путям, `up`, `down`, `total`), `hasNet`
  (телефон: есть деление Wi-Fi / мобильная), `limit` (`on`, `gb`, `period`,
  `scope`, `used`, `pct`). `name` — exe на ПК, пакет на телефоне, `""` —
  без программы, `ssh_tunnel` — сама программа.
- `POST /api/traffic/limit` `{"on":bool,"gb":50,"period":"month|day","scope":"all|server|mobile"}`
  — лимит: только предупреждения на 80% и 100%, VPN не выключается.
- `GET /api/lanscan` — как идёт сканирование и кто найден: `running`,
  `subnet`, `iface`, `done`/`total`, `hosts` (`ip`, `mac` — пусто, если
  система не дала, `randomMac`, `name`, `vendor`, `rttMs`, `ports` — открытые
  из 22/80/443, `self`). `POST {}` — сканировать заново. Ничего не хранится.
- `GET /api/clipboard?after=<seq>[&wait=1]` — общий буфер: `items` (новые
  первыми: `id`, `seq`, `kind` text|image|file, `name`, `size`, `preview`,
  `from`, `out`, `sent`, `gone`), `seq`, `max`, `mesh`. `wait=1` — ждать
  новой записи до 25 с.
- `GET /api/clipboard/data?id=[&dl=1]` — содержимое записи.
- `POST /api/clipboard/push?kind=text|image|file&name=&mime=` — тело —
  содержимое (до 50 МБ): в историю и всем устройствам сети, кто в сети.
  Ответ `{"ok":true,"item"}` или `{"error","tooBig"}`.
- `POST /api/clipboard/delete` `{"id"}` или `{"all":true}`.

Между устройствами буфер ходит по сети устройств на порт 47837
(`POST /clip?id=&kind=&name=&mime=`); нет службы на порту — устройство не
участвует.

```powershell
Invoke-RestMethod "$($a.url)/api/clipboard/push?kind=text" -Headers $h -Method Post -Body 'привет'
(Invoke-RestMethod "$($a.url)/api/traffic?period=month" -Headers $h).top | Select-Object -First 5 name, total
```

## Docker (ТЗ 7, Linux)

Отвечают, только пока плагин «Docker» добавлен и включён (иначе 404). Правила
ядра ставит только VPN-версия (`ssh_tunnel_vpn_linux`); в обычной выбор
сохраняется, но не действует (`vpn:false`).

- `GET /api/docker` — `available` (видно ли Docker API), `error` (почему нет:
  нет сокета, нет прав), `vpn`, `kernel` (`up` — адаптер VPN поднят, `full` —
  туннель включён, `rules` — сколько правил в ядре, `error`), `containers`
  (`id`, `name`, `image`, `state`, `project`, `service`, `netMode`
  bridge|host|none|container, `parent`, `networks[{name, ip, ip6}]`, `choice`
  — выбор для самого контейнера, `label` — что велит метка
  `ssh_tunnel.route`, `effective` — что действует, `from`
  label|container|project|default|parent|fixed, `switchable`, `why`/`whyEn`),
  `projects[{name, choice, count}]`.
- `POST /api/docker/set` `{"container":"<имя или ID>","route":"host|tunnel|direct"}`
  или `{"project":"<проект compose>","route":…}` — весь проект разом (выбор
  отдельных служб проекта снимается). Контейнер в сети хоста или другого
  контейнера — 400 с причиной.

```bash
ssh_tunnel_linux docker                                 # список (GET /api/docker)
ssh_tunnel_linux docker set container=web route=tunnel  # через сервер
ssh_tunnel_linux docker set project=shop route=direct   # весь проект напрямую
```
