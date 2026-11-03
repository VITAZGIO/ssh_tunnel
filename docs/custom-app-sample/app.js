// Своё приложение для ssh_tunnel: обычная страница. Ключ приложения — в
// адресе страницы (/custom/<id>/<ключ>/index.html), с ним и ходим к API.
// Ключ пускает только туда, что разрешено в manifest.json (здесь — "state").
const KEY = location.pathname.split('/')[3];
const LANG = new URLSearchParams(location.search).get('lang') === 'en' ? 'en' : 'ru';
const L = (ru, en) => LANG === 'en' ? en : ru;

async function api(path, body) {
  const r = await fetch(path, {
    method: body ? 'POST' : 'GET',
    headers: { 'Authorization': 'Bearer ' + KEY, 'Content-Type': 'application/json' },
    body: body ? JSON.stringify(body) : undefined,
  });
  const j = await r.json().catch(() => ({ error: r.status + ' ' + r.statusText }));
  if (!r.ok && !j.error) j.error = String(r.status);
  return j;
}

const MODES = { off: L('Выключено', 'Off'), mesh: L('Только сеть устройств', 'Device network only'),
  bypass: L('Обход', 'Bypass'), 'bypass+mesh': L('Обход + сеть устройств', 'Bypass + device network') };

async function refresh() {
  const st = await api('/api/state');
  document.getElementById('state').textContent = st.error || MODES[st.mode] || st.mode;
  // Разрешения «devices» нет — ядро ответит отказом, и это правильно.
  const dev = await api('/api/mesh');
  document.getElementById('devices').textContent = dev.error
    ? L('нет доступа: ', 'no access: ') + dev.error
    : L('видно (странно — разрешения не было)', 'visible (odd — no permission)');
}

// Свои данные приложения хранит ядро (/api/app/data); при удалении
// приложения они удаляются вместе с ним.
async function loadCount() {
  const d = await api('/api/app/data');
  document.getElementById('count').textContent = (d.data && d.data.count) || 0;
}
document.getElementById('inc').onclick = async () => {
  const n = Number(document.getElementById('count').textContent) + 1;
  await api('/api/app/data', { key: 'count', value: n });
  loadCount();
};

document.getElementById('title').textContent = L('Привет', 'Hello');
document.getElementById('refresh').onclick = refresh;
refresh();
loadCount();
