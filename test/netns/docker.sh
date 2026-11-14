#!/usr/bin/env bash
# Стенд Docker на домашнем Linux (ТЗ 7 + строгий режим ТЗ 14) на настоящем
# ядре: свой dockerd в пространстве c, настоящий ssh_tunnel_vpn_linux там же,
# сервер B — настоящий sshd. Запуск: sudo RP=2 LAB=/tmp/sshlab
# test/netns/docker.sh (нужны $LAB/labd, $LAB/ssh_tunnel_vpn_linux и образ
# busybox:1.36 в Docker самой машины сборки — его загружаем в свой dockerd).
source "$(dirname "$0")/lab.sh"

BIN="$LAB/ssh_tunnel_vpn_linux"
CFG="$LAB/cfg-c"
SOCK=/run/docker-lab.sock
IMG=busybox:1.36
TOK=""
# Каждая команда docker — не дольше минуты: повиснет — увидим где.
D() { timeout 60 docker -H "unix://$SOCK" "$@"; }

cleanup() {
  set +e
  set +x
  stop_client TERM
  D rm -f tun dir hst str >/dev/null 2>&1
  if [ -f /run/docker-lab.pid ]; then
    local p; p=$(cat /run/docker-lab.pid)
    kill "$p" 2>/dev/null
    for _ in $(seq 1 15); do kill -0 "$p" 2>/dev/null || break; sleep 1; done
    kill -9 "$p" 2>/dev/null
    rm -f /run/docker-lab.pid
  fi
  # dockerd, убитый kill -9, оставляет контейнеры в containerd машины.
  if command -v ctr >/dev/null; then
    for t in $(timeout 10 ctr -n docker-lab task ls -q 2>/dev/null); do
      timeout 10 ctr -n docker-lab task kill -s SIGKILL "$t" 2>/dev/null
    done
  fi
  lab_down
  rm -rf /etc/netns/c "$LAB/etc-c"
}
trap cleanup EXIT

# Своя /etc машины c. Пространство монтирований ip netns exec отделяет
# только точки монтирования, а не сами файлы: без resolved клиент заменяет
# ссылку /etc/resolv.conf своим файлом (так и задумано на настоящей машине),
# а на общей /etc это DNS самой машины сборки — раннер GitHub терял связь, и
# задание повисало без журнала (причина зависания, ТЗ 19). Копия /etc
# привязывается поверх /etc только у клиента; resolv.conf в ней — ссылка, как
# на Ubuntu, чтобы проверить и возврат ссылки.
ETC_C="$LAB/etc-c"
prepare_etc_c() {
  rm -rf "$ETC_C"
  cp -a /etc "$ETC_C" 2>/dev/null || true
  rm -f "$ETC_C/resolv.conf"
  echo "nameserver 192.0.2.53" > "$ETC_C/resolv-lab.conf"
  ln -s /etc/resolv-lab.conf "$ETC_C/resolv.conf"
}
# in_c команда… — в пространстве c со своей /etc и без resolved машины.
in_c() {
  ns c sh -ec 'mount --bind "$0" /etc; [ ! -d /run/systemd/resolve ] || mount -t tmpfs tmpfs /run/systemd/resolve; exec "$@"' "$ETC_C" "$@"
}
host_resolv() { echo "$(readlink /etc/resolv.conf || true) $(md5sum < /etc/resolv.conf)"; }
HOST_RESOLV=$(host_resolv)
host_resolv_same() { [ "$(host_resolv)" = "$HOST_RESOLV" ]; }

start_client() {
  rm -f "$CFG/ssh_tunnel/api.json"
  in_c env XDG_CONFIG_HOME="$CFG" DOCKER_HOST="unix://$SOCK" "$BIN" -web -web-listen 127.0.0.1:47800 \
    >> "$LAB/client-c.log" 2>&1 &
  eventually 20 test -s "$CFG/ssh_tunnel/api.json"
  TOK=$(jq -r .token "$CFG/ssh_tunnel/api.json")
}
stop_client() { # TERM или KILL
  local p
  p=$(pgrep -f "$BIN -web -web-listen" || true)
  if [ -n "$p" ]; then kill "-$1" $p 2>/dev/null || true; fi
  for _ in $(seq 1 15); do pgrep -f "$BIN -web" >/dev/null || return 0; sleep 1; done
}
api() {
  if [ -n "${2:-}" ]; then
    ns c curl -fsS -m 10 -H "Authorization: Bearer $TOK" -d "$2" "http://127.0.0.1:47800$1"
  else
    ns c curl -fsS -m 10 -H "Authorization: Bearer $TOK" "http://127.0.0.1:47800$1"
  fi
}
is_phase() { [ "$(api /api/state | jq -r ".$1.phase")" = "$2" ]; }
tunnel_on() { eventually 40 is_phase tunnel on; }

cecho() { D exec "$1" wget -qO- -T 5 "http://${2:-192.0.2.80}/"; }
cdns() { D exec "$1" nslookup "$2" 2>&1 | grep -q '192.0.2.8[01]'; }
dnslog() { grep -c " $1 $2 $3\$" "$LAB/dns.log" || true; }
run_ctr() { # имя [доп. флаги docker run]
  local n=$1; shift
  D rm -f "$n" >/dev/null 2>&1 || true
  D run -d --name "$n" "$@" "$IMG" sh -c 'mkdir -p /www && echo ok > /www/index.html && exec httpd -f -p 80 -h /www' >/dev/null
}
# Docker 29 убрал NetworkSettings.IPAddress — адрес только в Networks.
ip_of() { D inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$1"; }
# Как ядро c ведёт пакет из контейнера.
route_of() { ip -n c route get "${2:-192.0.2.80}" from "$(ip_of "$1")" iif docker0 2>&1; }
show_c() {
  echo "-- ip rule c:"; ip -n c rule
  for t in 7094 7095 7096 main; do echo "-- table $t:"; ip -n c route show table "$t"; done
  for n in tun dir hst str; do echo "-- путь $n: $(route_of "$n")"; done
}
our_rules() { ip -n c rule | grep -E 'lookup 709[4-6]' || true; }
count_rules() { ip -n c rule | grep -E 'lookup 709[4-6]' | grep -cE "$1" || true; }
t_is_b() { [ "$(cecho tun 2>/dev/null)" = 203.0.113.20 ]; }

# run.sh после аварии: только уборка.
[ -z "${LAB_CLEANUP_ONLY:-}" ] || exit 0

section "стенд: rp_filter=$RP"
# Где мы: каждая строка сценария с временем — чтобы найти, что повисло.
export PS4='+ $(date +%T) docker.sh:${LINENO}: '
set -x
hang_point start
lab_down
lab_inet
lab_b
host c br1 192.168.50.2/24 192.168.50.1
host lan br1 192.168.50.3/24 192.168.50.1
mkdir -p /etc/netns/c
echo "nameserver 192.0.2.53" > /etc/netns/c/resolv.conf

section "свой dockerd в пространстве c"
rm -rf /var/lib/docker-lab /run/docker-lab
# Свой daemon.json: у машины сборки в /etc/docker/daemon.json свой DNS.
echo '{"dns":["192.0.2.53"],"ip6tables":false}' > "$LAB/daemon.json"
# Только сеть пространства c (nsenter --net), а не ip netns exec: у того своё
# пространство монтирований, и корень контейнера, смонтированный dockerd, не
# виден containerd машины сборки (контейнер «без sh»).
nsenter --net=/run/netns/c dockerd --data-root /var/lib/docker-lab --exec-root /run/docker-lab \
  --pidfile /run/docker-lab.pid -H "unix://$SOCK" --containerd-namespace docker-lab \
  --containerd-plugins-namespace docker-lab-plugins --exec-opt native.cgroupdriver=cgroupfs \
  --cgroup-parent docker-lab --config-file "$LAB/daemon.json" \
  > "$LAB/dockerd-c.log" 2>&1 &
eventually 60 D info || { tail -n 40 "$LAB/dockerd-c.log"; exit 1; }
timeout 120 docker save "$IMG" | D load >/dev/null
ns c "$LAB/labd" -http 0.0.0.0:9090 > "$LAB/labd-c.log" 2>&1 &
hang_point dockerd

section "клиент ssh_tunnel в c"
rm -rf "$CFG"
prepare_etc_c
in_c env XDG_CONFIG_HOME="$CFG" "$BIN" -host 203.0.113.20 -user tunlab -key "$LAB/key" -filter all \
  -direct 192.0.2.81,.d.test-lab.net -save
jq '.profiles[].meshEnabled=true | .profiles[].meshKey="lab-mesh-key-0123456789abcdef"' \
  "$CFG/ssh_tunnel/config.json" > "$CFG/c.json" && mv "$CFG/c.json" "$CFG/ssh_tunnel/config.json"
echo "203.0.113.20 $(cut -d' ' -f1,2 "$LAB/b_host.pub")" > "$CFG/ssh_tunnel/known_hosts"
start_client
api /api/addons '{"op":"add","id":"docker"}' >/dev/null
api /api/state '{"tunnel":true,"mesh":true}' >/dev/null
tunnel_on || { echo "::error::туннель не поднялся"; api /api/state; tail -n 60 "$LAB/client-c.log"; exit 1; }

hang_point tunnel
run_ctr tun -p 8080:80
run_ctr dir
run_ctr hst
run_ctr str
api /api/docker/set '{"container":"tun","route":"tunnel"}' >/dev/null
api /api/docker/set '{"container":"dir","route":"direct"}' >/dev/null
api /api/docker/set '{"container":"str","route":"tunnel","strict":true}' >/dev/null
sleep 2
echo "-- ip rule и nat в c:"; our_rules; ns c iptables -t nat -S | grep -E 'SSH_TUNNEL|MASQ' || true
echo "-- /api/docker:"; api /api/docker | jq -c '.kernel, [.containers[] | {name, effective, strict, published}]'
show_c
check "resolv.conf машины сборки не тронут" host_resolv_same
check "resolv.conf машины c — DNS программы" grep -q 198.18.0.53 "$ETC_C/resolv.conf"

section "туннель включён"
expect "«через сервер» виден адресом B" 203.0.113.20 cecho tun
expect "«напрямую» виден домашним адресом" 192.168.50.2 cecho dir
expect "«как компьютер» идёт как компьютер (через B)" 203.0.113.20 cecho hst
expect "строгий «через сервер» — через B" 203.0.113.20 cecho str
expect "«всегда напрямую» к «через сервер» не применяется" 203.0.113.20 cecho tun 192.0.2.81
expect "«как компьютер» к «всегда напрямую» — напрямую" 192.168.50.2 cecho hst 192.0.2.81
expect "сам компьютер к «всегда напрямую» — напрямую" 192.168.50.2 ns c curl -fsS -m 8 http://192.0.2.81/check "маршрут «напрямую» — в своей таблице, не в main" sh -c "ip -n c route show table 7097 | grep -q 192.0.2.81 && ! ip -n c route show table main | grep -q 192.0.2.81"
check "DNS «через сервер» отвечает" cdns tun name1.test-lab.net
expect "DNS «через сервер» разрешён через B" 1 dnslog 1.1.1.1 203.0.113.20 name1.test-lab.net
expect "DNS «через сервер» не у провайдера" 0 dnslog 192.0.2.53 192.168.50.2 name1.test-lab.net
check "DNS имени из «всегда напрямую» у «через сервер» отвечает" cdns tun direct1.d.test-lab.net
expect "…и не у провайдера (строгий DNS)" 0 dnslog 192.0.2.53 192.168.50.2 direct1.d.test-lab.net
check "DNS «напрямую» отвечает" cdns dir name2.test-lab.net
expect "контейнер ↔ контейнер" ok D exec tun wget -qO- -T 5 "http://$(ip_of dir)/"
expect "контейнер → компьютер" "$(ip_of tun)" D exec tun wget -qO- -T 5 http://172.17.0.1:9090/
expect "опубликованный порт из домашней сети" ok ns lan curl -fsS -m 5 http://192.168.50.2:8080/
check "исключение из MASQUERADE стоит" sh -c "ip netns exec c iptables -t nat -S SSH_TUNNEL_DOCKER | grep -q -- '-s $(ip_of tun)/32 -j ACCEPT'"
check "исключение из DROP Docker в raw стоит первым" sh -c "ip netns exec c iptables -t raw -S PREROUTING | grep -m1 -- '^-A PREROUTING' | grep -q SSH_TUNNEL_DOCKER_RAW"

section "туннель выключен (сеть устройств включена)"
api /api/state '{"tunnel":false}' >/dev/null
leak=0
for _ in $(seq 1 12); do
  if [ "$(cecho tun 2>/dev/null || true)" = "192.168.50.2" ]; then leak=1; fi
  sleep 0.5
done
if [ $leak = 0 ]; then pass "в секунды после «Выключить» «через сервер» напрямую не вышел"; else fail "«через сервер» вышел напрямую сразу после «Выключить»"; fi
eventually 20 is_phase tunnel off || true
echo "-- состояние: $(api /api/state | jq -c '{tunnel:.tunnel.phase, mesh:.mesh.phase, mode}')"
refute "«через сервер» без туннеля — без интернета" cecho tun
refute "«через сервер» без туннеля — без DNS" cdns tun name3.test-lab.net
expect "…и запрос имени никуда не ушёл" 0 sh -c "grep -c 'name3.test-lab.net' $LAB/dns.log || true"
expect "«напрямую» работает" 192.168.50.2 cecho dir
expect "«как компьютер» — как компьютер (напрямую)" 192.168.50.2 cecho hst

section "туннель снова включён, контейнер пересоздан"
api /api/state '{"tunnel":true}' >/dev/null
tunnel_on || fail "туннель не поднялся снова"
run_ctr tun -p 8080:80
if eventually 10 t_is_b; then pass "пересозданный «через сервер» — через B"; else fail "пересозданный «через сервер» не через B"; fi

section "kill -9 клиента"
stop_client KILL
sleep 2
refute "после kill -9 «через сервер» без интернета" cecho tun
refute "после kill -9 «через сервер» без DNS" cdns tun name4.test-lab.net
refute "после kill -9 строгий без интернета" cecho str
expect "после kill -9 «напрямую» работает" 192.168.50.2 cecho dir
echo "-- правила после kill -9:"; our_rules
start_client
api /api/state '{"tunnel":true,"mesh":true}' >/dev/null
tunnel_on || fail "после перезапуска туннель не поднялся"
if eventually 15 t_is_b; then pass "после перезапуска «через сервер» снова через B"; else fail "после перезапуска «через сервер» не через B"; fi

section "чистая остановка"
stop_client TERM
sleep 1
echo "-- правила после остановки:"; our_rules
expect "после остановки остались только правила строгого (3)" 3 count_rules '^940[23]:'
expect "обычных правил контейнеров нет" 0 count_rules '^940[01]:'
refute "цепочки исключения NAT нет" ns c iptables -t nat -S SSH_TUNNEL_DOCKER
refute "цепочки исключения raw нет" ns c iptables -t raw -S SSH_TUNNEL_DOCKER_RAW
check "resolv.conf машины c снова ссылка, как была" test "$(readlink "$ETC_C/resolv.conf")" = /etc/resolv-lab.conf
check "resolv.conf машины сборки не тронут и после всего" host_resolv_same
expect "«через сервер» без программы — как компьютер" 192.168.50.2 cecho tun
refute "строгий без программы — без интернета" cecho str
check "строгий без программы ходит в домашнюю сеть" D exec str ping -c1 -W2 192.168.50.3

section "снять галочку (VPN выключен)"
start_client
api /api/state '{"tunnel":false,"mesh":false}' >/dev/null
sleep 2
api /api/docker/set '{"container":"str","strict":false}' >/dev/null
sleep 2
expect "галочка снята — строгий идёт как компьютер" 192.168.50.2 cecho str
stop_client TERM
expect "после всего правил нет" 0 count_rules '.'
expect "после всего таблица 7094 пуста" 0 sh -c "ip -n c route show table 7094 | wc -l"

echo "== журнал клиента (хвост)"; tail -n 40 "$LAB/client-c.log" || true
section "итог: провалов $FAILS"
[ "$FAILS" = 0 ]
