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

`POST /api/state` с `{"tunnel": true}`, `{"mesh": false}` или обоими — «хочу».
Каждая кнопка меняет только своё. Ответ — состояние сразу после просьбы
(обычно `connecting`); чем кончилось — событием `core` или следующим `GET`.
`{"tunnel": false}` во время `connecting` — отмена подключения.

## События

`GET /events` — поток Server-Sent Events, по строке `data: {…}` на событие:
`core` (поле `core` — то же, что `GET /api/state`), `state` (связь), `conn`
(соединение: программа, цель, напрямую ли), `log`, `stats`, `speed`,
`vpssetup`. При подключении сначала приходит недавний журнал и текущее `core`.

## Дополнительные приложения

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
- `POST /api/addons` с `{"op":"add|remove|star|on|seen","id":"…","value":bool}`
  — добавить, убрать, звёздочка, выключатель фонового, «подсказку видел».
  Ответ — то же, что `GET`.
- `POST /api/window` с `{"key":"book","title":"…","url":"/book.html?…"}` —
  отдельное окно (ПК). Место и размер окна запоминаются по `key`. `{"ok":false}`
  без ошибки — окно этого не умеет (браузер), страница открывает окно сама.

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
