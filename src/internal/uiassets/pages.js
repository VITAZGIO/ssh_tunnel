/* Общее для страниц Книги и дополнительных приложений: ключ API, язык, тема,
   отдельные окна, внешние ссылки. Страницы одни и те же на ПК, Linux и
   Android; где платформы различаются — решается здесь, по тому, что умеет
   окно (ответ /api/addons: windows) и есть ли мост Android. */
const Q = new URLSearchParams(location.search);
const T = Q.get('t') || '';
const LANG = Q.get('lang') === 'en' ? 'en' : (Q.get('lang') ? 'ru' :
  ((navigator.language || '').toLowerCase().startsWith('ru') ? 'ru' : 'en'));
// На телефоне страницу открывает своя активность и даёт мост SSHTunnelAndroid.
const ANDROID = !!window.SSHTunnelAndroid;
// Встроено в главное окно ПК (iframe) — «назад» возвращает туда.
const EMBED = window.parent !== window;
// Отдельное окно: «назад» и «⧉ В окне» не нужны.
const SOLO = Q.get('solo') === '1';

document.documentElement.lang = LANG;
if(Q.get('theme')) document.documentElement.dataset.theme = Q.get('theme');
if(ANDROID) document.documentElement.classList.add('android');

const $ = id => document.getElementById(id);
const L = (ru, en) => LANG === 'en' ? en : ru;
const appName = m => (m.name && (m.name[LANG] || m.name.ru)) || m.id;
const appHint = m => (m.hint && (m.hint[LANG] || m.hint.ru)) || '';
const esc = s => String(s).replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
const icon = (n, cls) => `<span class="mi ${cls || ''}" style="--i:url(/icons/${esc(n)}.svg)"></span>`;
// appIcon — значок приложения: у своего — файл из его архива.
const appIcon = (m, cls) => m.iconUrl ? `<img class="${cls || ''}" src="${esc(m.iconUrl)}" alt="">`
  : icon(m.custom ? 'extension' : m.icon, cls);

const api = (path, body) => fetch(path + (path.includes('?') ? '&' : '?') + 'lang=' + LANG, {
  method: body ? 'POST' : 'GET',
  headers: {'X-Token': T, 'Content-Type': 'application/json'},
  body: body ? JSON.stringify(body) : undefined,
}).then(r => r.json());

// pageURL — адрес своей страницы с ключом, языком и темой.
function pageURL(file, extra){
  const p = new URLSearchParams({t: T, lang: LANG});
  const th = document.documentElement.dataset.theme;
  if(th) p.set('theme', th);
  for(const [k, v] of Object.entries(extra || {})) p.set(k, v);
  return '/' + file + '?' + p.toString();
}

// openWindow открывает отдельное окно. Окно ПК умеет само (ядро помнит место
// и размер); не умеет (браузер на Linux, VPN-служба Windows) — окно браузера,
// место помнит он сам через localStorage. На телефоне окон нет: страница
// открывается тут же. Возвращает true, если открылось отдельно.
async function openWindow(key, title, url){
  if(ANDROID){ location.href = url; return false; }
  try {
    const r = await api('/api/window', {key, title, url});
    if(r && r.ok) return true;
  } catch(e) {}
  let feat = 'popup=yes,width=820,height=640';
  try {
    const g = JSON.parse(localStorage.getItem('win:' + key) || 'null');
    if(g) feat = `popup=yes,width=${g.w},height=${g.h},left=${g.x},top=${g.y}`;
  } catch(e) {}
  const w = window.open(url, 'sshtunnel_' + key, feat);
  if(!w){ location.href = url; return false; }
  return true;
}

// Окно браузера запоминает своё место само (нативные окна ПК помнит ядро).
if(SOLO && !ANDROID){
  window.addEventListener('beforeunload', () => {
    try {
      localStorage.setItem('win:' + (Q.get('key') || 'book'), JSON.stringify(
        {x: window.screenX, y: window.screenY, w: window.outerWidth, h: window.outerHeight}));
    } catch(e) {}
  });
}

// openExternal — ссылка наружу (GitHub): в системном браузере.
function openExternal(url){
  if(ANDROID && window.SSHTunnelAndroid.openExternal){ window.SSHTunnelAndroid.openExternal(url); return; }
  window.open(url, '_blank', 'noopener');
}

// closePage — «назад» со страницы: в главное окно, в активность телефона или
// просто закрыть отдельное окно.
function closePage(){
  if(EMBED){ window.parent.postMessage({sshtunnel: 'close'}, '*'); return; }
  // На телефоне Книга, открытая со страницы приложения, — шаг назад, а не
  // выход из экрана.
  if(ANDROID){ if(history.length > 1) history.back(); else window.SSHTunnelAndroid.close(); return; }
  if(history.length > 1) history.back(); else window.close();
}

// changed — сказать главному окну, что список приложений изменился
// (звёздочки → быстрый доступ).
function changed(){
  if(EMBED) window.parent.postMessage({sshtunnel: 'changed'}, '*');
}

// openBook — Книга на нужной главе: на ПК отдельным окном, на телефоне тут же.
function openBook(chapter){
  const url = pageURL('book.html', chapter ? {ch: chapter, solo: '1', key: 'book'} : {solo: '1', key: 'book'});
  if(ANDROID){ location.href = pageURL('book.html', chapter ? {ch: chapter} : {}); return; }
  openWindow('book', L('Книга', 'Book') + ' — ssh_tunnel', url);
}
