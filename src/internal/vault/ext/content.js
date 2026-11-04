// Поле пароля на странице → под ним список своих логинов для этого сайта.
(() => {
  let box = null;
  const close = () => { if(box){ box.remove(); box = null; } };
  const fillInput = (el, v) => {
    const set = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value').set;
    set.call(el, v);
    el.dispatchEvent(new Event('input', {bubbles: true}));
    el.dispatchEvent(new Event('change', {bubbles: true}));
  };
  const userFor = pass => {
    const inputs = [...(pass.form || document).querySelectorAll('input')].filter(i => i.type !== 'hidden' && i.offsetParent !== null);
    const i = inputs.indexOf(pass);
    for(let j = i - 1; j >= 0; j--) if(/^(text|email|tel)$/.test(inputs[j].type || 'text')) return inputs[j];
    return null;
  };
  async function show(pass){
    close();
    const r = await chrome.runtime.sendMessage({type: 'fill', origin: location.origin});
    if(!r || r.error === 'nokey' || r.error === 'offline' || (!r.locked && !(r.items || []).length)) return;
    const rect = pass.getBoundingClientRect();
    box = document.createElement('div');
    box.style.cssText = `position:fixed;left:${rect.left}px;top:${rect.bottom + 4}px;z-index:2147483647;min-width:${Math.max(220, rect.width)}px;
      background:#151a24;color:#e8ebf2;border:1px solid #2a3142;border-radius:10px;box-shadow:0 8px 24px rgba(0,0,0,.4);font:13px system-ui,sans-serif;padding:4px`;
    if(r.locked){
      box.textContent = '🔒 Пароли ssh_tunnel закрыты — открой их в программе';
      box.style.padding = '10px 12px';
    } else for(const it of r.items){
      const b = document.createElement('button');
      b.type = 'button';
      b.textContent = '🔑 ' + (it.login || it.title);
      b.style.cssText = 'display:block;width:100%;text-align:left;background:transparent;color:inherit;border:0;padding:8px 10px;border-radius:7px;cursor:pointer;font:inherit';
      b.onmouseenter = () => b.style.background = '#232a39';
      b.onmouseleave = () => b.style.background = 'transparent';
      b.onmousedown = e => { e.preventDefault(); const u = userFor(pass); if(u && it.login) fillInput(u, it.login); fillInput(pass, it.pass); close(); };
      box.appendChild(b);
    }
    document.body.appendChild(box);
  }
  document.addEventListener('focusin', e => { if(e.target.matches && e.target.matches('input[type=password]')) show(e.target); });
  document.addEventListener('focusout', () => setTimeout(close, 150));
  // Отправили форму с паролем — предложить запомнить (новый сайт или новый пароль).
  document.addEventListener('submit', async e => {
    const pass = e.target.querySelector && e.target.querySelector('input[type=password]');
    if(!pass || !pass.value) return;
    const u = userFor(pass), login = u ? u.value : '';
    const r = await chrome.runtime.sendMessage({type: 'fill', origin: location.origin});
    if(!r || r.locked || r.error) return;
    if((r.items || []).some(it => it.login === login && it.pass === pass.value)) return;
    if(confirm(`ssh_tunnel: запомнить пароль${login ? ' для ' + login : ''} на ${location.hostname}?`))
      chrome.runtime.sendMessage({type: 'save', origin: location.origin, login, pass: pass.value});
  }, true);
})();
