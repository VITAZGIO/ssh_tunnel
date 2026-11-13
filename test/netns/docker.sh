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
D() { docker -H "unix://$SOCK" "$@"; }

cleanup() {
  set +e
  stop_client TERM
  D rm -f t d h s >/dev/null 2>&1
  [ -f /run/docker-lab.pid ] && kill "$(cat /run/docker-lab.pid)" 2>/dev/null
  sleep 2
  lab_down
  rm -rf /etc/netns/c
}
trap cleanup EXIT

start_client() {
  rm -f "$CFG/ssh_tunnel/api.json"
  ns c sh -c "[ -d /run/systemd/resolve ] && mount -t tmpfs tmpfs /run/systemd/resolve; exec env XDG_CONFIG_HOME='$CFG' DOCKER_HOST='unix://$SOCK' '$BIN' -web -web-listen 127.0.0.1:47800" \
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
ip_of() { D inspect -f '{{.NetworkSettings.IPAddress}}' "$1"; }
our_rules() { ip -n c rule | grep -E 'lookup 709[4-6]' || true; }
count_rules() { ip -n c rule | grep -E 'lookup 709[4-6]' | grep -cE "$1" || true; }
t_is_b() { [ "$(cecho t 2>/dev/null)" = 203.0.113.20 ]; }

echo "== стенд: rp_filter=$RP"
lab_down
lab_inet
lab_b
host c br1 192.168.50.2/24 192.168.50.1
host lan br1 192.168.50.3/24 192.168.50.1
mkdir -p /etc/netns/c
echo "nameserver 192.0.2.53" > /etc/netns/c/resolv.conf

echo "== свой dockerd в пространстве c"
rm -rf /var/lib/docker-lab /run/docker-lab
# ip netns exec монтирует свой /sys — без cgroup внутри; Docker она нужна.
ns c sh -c "mountpoint -q /sys/fs/cgroup || mount -t cgroup2 none /sys/fs/cgroup; [ -d /run/systemd/resolve ] && mount -t tmpfs tmpfs /run/systemd/resolve; exec dockerd --data-root /var/lib/docker-lab --exec-root /run/docker-lab --pidfile /run/docker-lab.pid -H unix://$SOCK --containerd-namespace docker-lab --containerd-plugins-namespace docker-lab-plugins --exec-opt native.cgroupdriver=cgroupfs --cgroup-parent docker-lab --dns 192.0.2.53 --ip6tables=false" \
  > "$LAB/dockerd-c.log" 2>&1 &
eventually 60 D info || { tail -n 40 "$LAB/dockerd-c.log"; exit 1; }
docker save "$IMG" | D load >/dev/null
ns c "$LAB/labd" -http 0.0.0.0:9090 > "$LAB/labd-c.log" 2>&1 &

echo "== клиент ssh_tunnel в c"
rm -rf "$CFG"
ns c env XDG_CONFIG_HOME="$CFG" "$BIN" -host 203.0.113.20 -user tunlab -key "$LAB/key" -filter all \
  -direct 192.0.2.81,.d.test-lab.net -save
jq '.profiles[].meshEnabled=true | .profiles[].meshKey="lab-mesh-key-0123456789abcdef"' \
  "$CFG/ssh_tunnel/config.json" > "$CFG/c.json" && mv "$CFG/c.json" "$CFG/ssh_tunnel/config.json"
echo "203.0.113.20 $(cut -d' ' -f1,2 "$LAB/b_host.pub")" > "$CFG/ssh_tunnel/known_hosts"
start_client
api /api/addons '{"op":"add","id":"docker"}' >/dev/null
api /api/state '{"tunnel":true,"mesh":true}' >/dev/null
tunnel_on || { echo "::error::туннель не поднялся"; api /api/state; tail -n 60 "$LAB/client-c.log"; exit 1; }

run_ctr t -p 8080:80
run_ctr d
run_ctr h
run_ctr s
api /api/docker/set '{"container":"t","route":"tunnel"}' >/dev/null
api /api/docker/set '{"container":"d","route":"direct"}' >/dev/null
api /api/docker/set '{"container":"s","route":"tunnel","strict":true}' >/dev/null
sleep 2
echo "-- ip rule и nat в c:"; our_rules; ns c iptables -t nat -S | grep -E 'SSH_TUNNEL|MASQ' || true
echo "-- /api/docker:"; api /api/docker | jq -c '.kernel, [.containers[] | {name, effective, strict, published}]'

echo "== туннель включён"
expect "«через сервер» виден адресом B" 203.0.113.20 cecho t
expect "«напрямую» виден домашним адресом" 192.168.50.2 cecho d
expect "«как компьютер» идёт как компьютер (через B)" 203.0.113.20 cecho h
expect "строгий «через сервер» — через B" 203.0.113.20 cecho s
expect "«всегда напрямую» к «через сервер» не применяется" 203.0.113.20 cecho t 192.0.2.81
expect "у самого компьютера «всегда напрямую» действует" 192.168.50.2 cecho h 192.0.2.81
check "DNS «через сервер» отвечает" cdns t name1.test-lab.net
expect "DNS «через сервер» разрешён через B" 1 dnslog 1.1.1.1 203.0.113.20 name1.test-lab.net
expect "DNS «через сервер» не у провайдера" 0 dnslog 192.0.2.53 192.168.50.2 name1.test-lab.net
check "DNS имени из «всегда напрямую» у «через сервер» отвечает" cdns t direct1.d.test-lab.net
expect "…и не у провайдера (строгий DNS)" 0 dnslog 192.0.2.53 192.168.50.2 direct1.d.test-lab.net
check "DNS «напрямую» отвечает" cdns d name2.test-lab.net
expect "контейнер ↔ контейнер" ok D exec t wget -qO- -T 5 "http://$(ip_of d)/"
expect "контейнер → компьютер" "$(ip_of t)" D exec t wget -qO- -T 5 http://172.17.0.1:9090/
expect "опубликованный порт из домашней сети" ok ns lan curl -fsS -m 5 http://192.168.50.2:8080/
check "исключение из MASQUERADE стоит" sh -c "ip netns exec c iptables -t nat -S SSH_TUNNEL_DOCKER | grep -q -- '-s $(ip_of t)/32 -j ACCEPT'"

echo "== туннель выключен (сеть устройств включена)"
api /api/state '{"tunnel":false}' >/dev/null
leak=0
for _ in $(seq 1 12); do
  if [ "$(cecho t 2>/dev/null || true)" = "192.168.50.2" ]; then leak=1; fi
  sleep 0.5
done
if [ $leak = 0 ]; then pass "в секунды после «Выключить» «через сервер» напрямую не вышел"; else fail "«через сервер» вышел напрямую сразу после «Выключить»"; fi
eventually 20 is_phase tunnel off || true
echo "-- состояние: $(api /api/state | jq -c '{tunnel:.tunnel.phase, mesh:.mesh.phase, mode}')"
refute "«через сервер» без туннеля — без интернета" cecho t
refute "«через сервер» без туннеля — без DNS" cdns t name3.test-lab.net
expect "…и запрос имени никуда не ушёл" 0 sh -c "grep -c 'name3.test-lab.net' $LAB/dns.log || true"
expect "«напрямую» работает" 192.168.50.2 cecho d
expect "«как компьютер» — как компьютер (напрямую)" 192.168.50.2 cecho h

echo "== туннель снова включён, контейнер пересоздан"
api /api/state '{"tunnel":true}' >/dev/null
tunnel_on || fail "туннель не поднялся снова"
run_ctr t -p 8080:80
if eventually 10 t_is_b; then pass "пересозданный «через сервер» — через B"; else fail "пересозданный «через сервер» не через B"; fi

echo "== kill -9 клиента"
stop_client KILL
sleep 2
refute "после kill -9 «через сервер» без интернета" cecho t
refute "после kill -9 «через сервер» без DNS" cdns t name4.test-lab.net
refute "после kill -9 строгий без интернета" cecho s
expect "после kill -9 «напрямую» работает" 192.168.50.2 cecho d
echo "-- правила после kill -9:"; our_rules
start_client
api /api/state '{"tunnel":true,"mesh":true}' >/dev/null
tunnel_on || fail "после перезапуска туннель не поднялся"
if eventually 15 t_is_b; then pass "после перезапуска «через сервер» снова через B"; else fail "после перезапуска «через сервер» не через B"; fi

echo "== чистая остановка"
stop_client TERM
sleep 1
echo "-- правила после остановки:"; our_rules
expect "после остановки остались только правила строгого (3)" 3 count_rules '^940[23]:'
expect "обычных правил контейнеров нет" 0 count_rules '^940[01]:'
refute "цепочки исключения NAT нет" ns c iptables -t nat -S SSH_TUNNEL_DOCKER
expect "«через сервер» без программы — как компьютер" 192.168.50.2 cecho t
refute "строгий без программы — без интернета" cecho s
check "строгий без программы ходит в домашнюю сеть" D exec s ping -c1 -W2 192.168.50.3

echo "== снять галочку (VPN выключен)"
start_client
api /api/state '{"tunnel":false,"mesh":false}' >/dev/null
sleep 2
api /api/docker/set '{"container":"s","strict":false}' >/dev/null
sleep 2
expect "галочка снята — строгий идёт как компьютер" 192.168.50.2 cecho s
stop_client TERM
expect "после всего правил нет" 0 count_rules '.'
expect "после всего таблица 7094 пуста" 0 sh -c "ip -n c route show table 7094 | wc -l"

echo "== журнал клиента (хвост)"; tail -n 40 "$LAB/client-c.log" || true
echo "== итог: провалов $FAILS"
[ "$FAILS" = 0 ]
