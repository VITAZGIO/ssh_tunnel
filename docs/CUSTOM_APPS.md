# Своё приложение: формат

Своё дополнительное приложение — zip-архив с веб-страницей. Ставится в окне
(«Дополнительные приложения» → «+» → «Из файла») или командой
`ssh_tunnel.exe apps install app.zip --yes`. Образец — `docs/custom-app-sample/`
(заархивировать содержимое папки). Код — `src/internal/addons/custom.go`.

## Архив

- `manifest.json` в корне (или в единственной папке верхнего уровня).
- Страница (`page`, по умолчанию `index.html`), скрипты, стили, картинки,
  шрифты. Разрешены только `.html .htm .js .mjs .css .json .txt .md .svg
  .png .jpg .jpeg .gif .webp .ico .woff .woff2` — **никакого нативного
  кода**: архив с другим файлом не ставится.
- Архив до 20 МБ, распакованное — до 50 МБ.

## manifest.json

```json
{
  "id": "hello",
  "name": { "ru": "Привет", "en": "Hello" },
  "hint": { "ru": "Пара строк о том, что делает", "en": "…" },
  "icon": "icon.svg",
  "page": "index.html",
  "version": "1.0",
  "window": false,
  "permissions": ["state"]
}
```

- `id` — 2–32 знака: `a-z`, `0-9`, `-`; не id приложения каталога. Тот же
  `id` при повторной установке — обновление: файлы и разрешения новые, ключ
  и данные прежние.
- `name`, `hint` — строка или `{"ru","en"}`.
- `icon` — SVG или PNG из архива.
- `window` — на ПК сразу открывать в отдельном окне.

## Разрешения

| id | что на экране | адреса API |
|---|---|---|
| `state` | видеть, включены ли туннель и сеть устройств | `GET /api/state` |
| `control` | включать и выключать их | `POST /api/state` |
| `devices` | видеть устройства сети | `GET /api/mesh`, `/api/netmap`, `/api/files/devices`, `/api/ssh/devices` |
| `rules` | менять правила | `GET, POST /api/rules` |
| `files` | файлы других устройств, свои открытые папки | `GET, POST /api/files/…` |
| `traffic` | расход трафика | `GET /api/traffic` |
| `clipboard` | общий буфер | `GET, POST /api/clipboard…` |
| `calendar` | календарь | `GET, POST /api/calendar…` |
| `sitecheck` | проверять сайты | `GET, POST /api/diag/site…` |

Всегда доступно: `GET /api` (самоописание) и `/api/app/data` (свои данные).
Остальное — `403 {"error":"у приложения нет разрешения «…»"}`. Адреса
приложений каталога (`files`, `traffic`, …) отвечают, только пока это
приложение добавлено и включено.

## Как работает

- Страница отдаётся по `/custom/<id>/<ключ приложения>/<файл>` с
  `Content-Security-Policy: sandbox allow-scripts …` и открывается в iframe
  с `sandbox` без `allow-same-origin`: у неё свой непрозрачный origin, к окну
  программы, его ключу и хранилищу доступа нет. Запросы — только к адресу
  программы (`connect-src`).
- К API приложение ходит **своим ключом** (`app_…`, третья часть пути
  страницы) в `Authorization: Bearer`. Ключ из `api.json` ему не достаётся.
  Браузер шлёт такие запросы с `Origin: null` — API отвечает им с CORS, а
  пускает по ключу и разрешениям.
- Данные: `GET /api/app/data` → `{"data":{…}}`, `POST {"key","value"}`
  (`value: null` — удалить), до 1 МБ на приложение.
- Хранится в `<папка настроек>/custom/<id>/` (`app.json` — описание и ключ,
  `files/`, `data.json`). «Убрать» удаляет папку целиком.
- На плитке — пометка «не из каталога», значок — из архива.

```js
const KEY = location.pathname.split('/')[3];
const st = await fetch('/api/state', { headers: { Authorization: 'Bearer ' + KEY } }).then(r => r.json());
```

## API установки

`POST /api/addons/file?dry=1`, тело — zip: проверить и показать, что
просит (`{"id","name","permissions":[{"id","text"}],"files","update"}`).
Без `dry` — поставить (ответ — как `GET /api/addons` плюс `"installed"`).
Убрать — `POST /api/addons {"op":"remove","id"}`. Только с ключом программы.
