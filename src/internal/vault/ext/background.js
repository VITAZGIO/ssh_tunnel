// Ходит к ssh_tunnel на этом компьютере (127.0.0.1:47840) с ключом сопряжения
// из плагина «Пароли». Страницы сайтов сюда не достают — только через это.
const BASE = 'http://127.0.0.1:47840';
async function call(path, body){
  const {key} = await chrome.storage.local.get('key');
  if(!key) return {error: 'nokey'};
  try {
    const r = await fetch(BASE + path, body ? {method: 'POST', headers: {'X-Vault-Key': key, 'Content-Type': 'application/json'}, body: JSON.stringify(body)}
      : {headers: {'X-Vault-Key': key}});
    if(r.status === 401) return {error: 'badkey'};
    return await r.json();
  } catch(e) { return {error: 'offline'}; }
}
chrome.runtime.onMessage.addListener((m, sender, reply) => {
  if(m.type === 'fill') call('/fill?origin=' + encodeURIComponent(m.origin)).then(reply);
  else if(m.type === 'save') call('/save', {origin: m.origin, login: m.login, pass: m.pass}).then(reply);
  else if(m.type === 'state') call('/state').then(reply);
  return true;
});
