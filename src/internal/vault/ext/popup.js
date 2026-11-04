const st = document.getElementById('st');
async function refresh(){
  const r = await chrome.runtime.sendMessage({type: 'state'});
  st.textContent = !r ? '…' : r.error === 'nokey' ? 'Вставь ключ сопряжения.' : r.error === 'badkey' ? 'Ключ не подходит — скопируй новый в плагине.'
    : r.error === 'offline' ? 'ssh_tunnel не запущен или плагин «Пароли» не добавлен.' : r.unlocked ? '✓ Подключено, пароли открыты.' : '✓ Подключено. Пароли закрыты — открой их в программе.';
}
chrome.storage.local.get('key').then(({key}) => { document.getElementById('key').value = key || ''; refresh(); });
document.getElementById('save').onclick = async () => { await chrome.storage.local.set({key: document.getElementById('key').value.trim()}); refresh(); };
