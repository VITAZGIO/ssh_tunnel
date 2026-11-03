package panel

import (
	"encoding/json"
	"net"
	"net/http"
	"strings"
)

// Открытые без входа точки для проверки утечек (приложение «Проверка
// утечек» в программе): какой адрес видит сервер и страница WebRTC. Ничего
// не хранят и ничего не раскрывают, кроме адреса самого спрашивающего.
func (s *Server) diagRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/ip", handleSeenIP)
	mux.HandleFunc("/webrtc", handleWebRTC)
}

// seenIP — адрес спрашивающего. За обратным прокси (запрос пришёл с
// локального адреса) — из его заголовков.
func seenIP(r *http.Request) string {
	ip := clientIP(r)
	if p := net.ParseIP(strings.Trim(ip, "[]")); p != nil && p.IsLoopback() {
		if v := r.Header.Get("X-Real-IP"); v != "" {
			return v
		}
		if v := r.Header.Get("X-Forwarded-For"); v != "" {
			return strings.TrimSpace(strings.Split(v, ",")[0])
		}
	}
	return strings.Trim(ip, "[]")
}

func handleSeenIP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	_ = json.NewEncoder(w).Encode(map[string]string{"ip": seenIP(r)})
}

func handleWebRTC(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	page := strings.Replace(webrtcPage, "{{IP}}", htmlEscape(seenIP(r)), 1)
	_, _ = w.Write([]byte(page))
}

func htmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#39;")
	return r.Replace(s)
}

// webrtcPage — одна лёгкая страница без библиотек: браузер собирает свои
// адреса через WebRTC (STUN), страница сравнивает их с тем, что видит сервер.
const webrtcPage = `<!doctype html>
<html lang="ru"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>WebRTC — проверка утечки</title>
<style>
body{margin:0;background:#0d1017;color:#e8ebf2;font:15px/1.5 "Segoe UI",-apple-system,Roboto,sans-serif}
main{max-width:560px;margin:0 auto;padding:24px 16px}
h1{font-size:20px;margin:0 0 12px}
.card{background:#161a23;border:1px solid #272d3a;border-radius:12px;padding:12px 14px;margin:0 0 12px}
.ok{color:#2fbf71}.bad{color:#e5484d}.dim{color:#828b9e;font-size:13px}
code{font-family:ui-monospace,Consolas,monospace}
@media (prefers-color-scheme: light){body{background:#eef1f6;color:#141821}.card{background:#fff;border-color:#dde2ec}}
</style></head><body><main>
<h1>Проверка WebRTC / WebRTC check</h1>
<div class="card">Сервер видит адрес / The server sees: <code id="seen">{{IP}}</code></div>
<div class="card">Адреса из WebRTC / WebRTC addresses:<div id="ips" class="dim">…</div></div>
<div class="card" id="verdict">Проверяю… / Checking…</div>
<p class="dim">Страница ничего не сохраняет и никуда не отправляет. / This page stores and sends nothing.</p>
</main><script>
const seen = document.getElementById('seen').textContent.trim();
const found = new Set();
const priv = ip => /^(10\.|127\.|192\.168\.|172\.(1[6-9]|2\d|3[01])\.|169\.254\.|100\.(6[4-9]|[7-9]\d|1[01]\d|12[0-7])\.|fe80:|fc|fd|::1)/i.test(ip) || ip.endsWith('.local');
function done(){
  const pub = [...found].filter(ip => !priv(ip));
  const leak = pub.filter(ip => ip !== seen);
  document.getElementById('ips').innerHTML = found.size ? [...found].map(x => '<code>' + x + '</code>').join('<br>') : '—';
  const v = document.getElementById('verdict');
  if(!window.RTCPeerConnection){ v.textContent = 'WebRTC в браузере выключен — утечки нет. / WebRTC is off — no leak.'; v.className = 'card ok'; return; }
  if(leak.length){ v.textContent = 'Утечка: браузер выдаёт настоящий адрес ' + leak.join(', ') + '. / Leak: the browser reveals ' + leak.join(', ') + '.'; v.className = 'card bad'; }
  else { v.textContent = 'Утечки нет: наружу виден только адрес сервера. / No leak: only the server address is visible.'; v.className = 'card ok'; }
}
try {
  const pc = new RTCPeerConnection({iceServers: [{urls: 'stun:stun.l.google.com:19302'}]});
  pc.createDataChannel('x');
  pc.onicecandidate = e => {
    if(!e.candidate){ done(); return; }
    const m = e.candidate.candidate.match(/ ([0-9a-f.:]+|[\w-]+\.local) \d+ typ/i);
    if(m) found.add(m[1]);
  };
  pc.createOffer().then(o => pc.setLocalDescription(o));
  setTimeout(done, 5000);
} catch(e) { done(); }
</script></body></html>`
