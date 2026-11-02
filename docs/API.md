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
