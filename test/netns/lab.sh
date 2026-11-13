#!/usr/bin/env bash
# Общая часть стенда: сетевые пространства имён и пары veth вместо машин.
# Ничего не ходит в настоящую сеть: «интернет» — пространство inet.
#
#   inet  br0 203.0.113.1/24   — «интернет»: A, B и роутер дома
#         br1 192.168.50.1/24  — «домашняя сеть» (inet её же роутер, NAT)
#         lo  192.0.2.80, .81  — HTTP-эхо (отвечает адресом пришедшего)
#             192.0.2.53       — DNS провайдера дома
#             1.1.1.1, 8.8.8.8 — DNS, которые спрашивает сервер B
#             192.0.2.99       — «кто-то в интернете» (входящие к A)
#   b     203.0.113.20         — сервер B: настоящий sshd
#   a     203.0.113.10         — сервер A (выход через B, ТЗ 10)
#   c     192.168.50.2         — домашний Linux с Docker (ТЗ 7)
#   lan   192.168.50.3         — другая машина домашней сети
#
# RP — rp_filter во всех пространствах (0, 1 или 2).

set -eEuo pipefail
trap 'echo "::error::стенд упал: ${BASH_SOURCE[0]##*/}:$LINENO: $BASH_COMMAND"' ERR

LAB=${LAB:-/tmp/sshlab}
RP=${RP:-2}
HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
mkdir -p "$LAB"

ns() { local n=$1; shift; ip netns exec "$n" "$@"; }

FAILS=0
pass() { echo "  ok   $*"; }
fail() { echo "::error::$*"; echo "  FAIL $*"; FAILS=$((FAILS+1)); }

# check "описание" команда… — команда должна завершиться успешно.
check() { local d=$1; shift; if "$@" >/dev/null 2>&1; then pass "$d"; else fail "$d"; fi; }
# refute "описание" команда… — команда должна не пройти.
refute() { local d=$1; shift; if "$@" >/dev/null 2>&1; then fail "$d"; else pass "$d"; fi; }
# expect "описание" ожидание команда… — вывод команды (первая строка) равен ожиданию.
expect() {
  local d=$1 want=$2; shift 2
  local got
  got=$("$@" 2>/dev/null | head -n1 | tr -d '\r' || true)
  if [ "$got" = "$want" ]; then pass "$d ($got)"; else fail "$d: ждали «$want», получили «$got»"; fi
}
# eventually СЕК команда… — ждать, пока команда не пройдёт.
eventually() {
  local t=$1; shift
  for _ in $(seq 1 "$t"); do "$@" >/dev/null 2>&1 && return 0; sleep 1; done
  return 1
}

set_rp() { # пространство
  ns "$1" sysctl -qw net.ipv4.conf.all.rp_filter="$RP" net.ipv4.conf.default.rp_filter="$RP"
}

# host ИМЯ МОСТ АДРЕС/МАСКА ШЛЮЗ — машина в своём пространстве.
host() {
  local n=$1 br=$2 addr=$3 gw=$4
  ip netns add "$n"
  ns "$n" ip link set lo up
  set_rp "$n"
  ip -n inet link add "v-$n" type veth peer name eth0 netns "$n"
  ip -n inet link set "v-$n" master "$br" up
  ns "$n" ip addr add "$addr" dev eth0
  ns "$n" ip link set eth0 up
  ns "$n" sysctl -qw net.ipv4.conf.eth0.rp_filter="$RP"
  ns "$n" ip route add default via "$gw"
}

lab_inet() {
  ip netns add inet
  ns inet ip link set lo up
  ns inet sysctl -qw net.ipv4.ip_forward=1 net.ipv4.conf.all.rp_filter=0 net.ipv4.conf.default.rp_filter=0
  for br in br0 br1; do ns inet ip link add "$br" type bridge; ns inet ip link set "$br" up; done
  ns inet ip addr add 203.0.113.1/24 dev br0
  ns inet ip addr add 192.168.50.1/24 dev br1
  for a in 192.0.2.80 192.0.2.81 192.0.2.53 192.0.2.99 1.1.1.1 8.8.8.8; do
    ns inet ip addr add "$a/32" dev lo
  done
  ns inet nft -f - <<'NFT'
table ip lab_home {
  chain post { type nat hook postrouting priority srcnat; policy accept;
    ip saddr 192.168.50.0/24 oifname "br0" masquerade
  }
}
NFT
  : > "$LAB/dns.log"
  ns inet "$LAB/labd" -http 192.0.2.80:80,192.0.2.81:80 \
    -dns 192.0.2.53:53,1.1.1.1:53,8.8.8.8:53 -a direct=192.0.2.81 \
    -log "$LAB/dns.log" > "$LAB/labd-inet.log" 2>&1 &
  echo $! > "$LAB/labd-inet.pid"
  eventually 10 ns inet curl -fsS -m 2 http://192.0.2.80/
}

# Сервер B: настоящий OpenSSH в пространстве b, вход по ключу.
lab_b() {
  host b br0 203.0.113.20/24 203.0.113.1
  id tunlab >/dev/null 2>&1 || useradd -m -s /bin/sh tunlab
  # Без пароля, но не заблокирован: с UsePAM no sshd не пускает «!» в shadow.
  usermod -p '*' tunlab
  rm -f "$LAB/key" "$LAB/key.pub" "$LAB/b_host" "$LAB/b_host.pub"
  ssh-keygen -q -t ed25519 -N '' -f "$LAB/key"
  ssh-keygen -q -t ed25519 -N '' -f "$LAB/b_host"
  install -d -m 755 /etc/ssh/lab
  install -m 644 "$LAB/key.pub" /etc/ssh/lab/authorized_keys
  cat > "$LAB/sshd_config" <<CFG
Port 22
ListenAddress 203.0.113.20
HostKey $LAB/b_host
PidFile $LAB/sshd_b.pid
AuthorizedKeysFile /etc/ssh/lab/authorized_keys
PasswordAuthentication no
KbdInteractiveAuthentication no
UsePAM no
StrictModes no
AllowTcpForwarding yes
PermitTunnel no
X11Forwarding no
LogLevel VERBOSE
CFG
  mkdir -p /run/sshd
  start_b
}

start_b() {
  ns b /usr/sbin/sshd -f "$LAB/sshd_config" -E "$LAB/sshd_b.log"
  eventually 10 ns b bash -c 'exec 3<>/dev/tcp/203.0.113.20/22'
}

# Убить B: sshd и все его сеансы (сервер «упал»).
kill_b() {
  for p in $(ip netns pids b); do kill -9 "$p" 2>/dev/null || true; done
}

# Ссылка клиента на B, как её выдаёт панель (с ключом).
link_b() {
  python3 - "$LAB/key" <<'PY'
import base64, json, sys
key = open(sys.argv[1]).read()
d = {"n": "B", "h": "203.0.113.20", "u": "tunlab", "k": key}
print("sshtunnel://import?v=1&d=" + base64.urlsafe_b64encode(json.dumps(d).encode()).decode().rstrip("="))
PY
}

lab_down() {
  for n in a b c lan actr inet; do
    for p in $(ip netns pids "$n" 2>/dev/null); do kill -9 "$p" 2>/dev/null || true; done
    ip netns del "$n" 2>/dev/null || true
  done
}

# echo_from ПРОСТРАНСТВО — каким адресом это пространство видно в «интернете».
echo_from() { ns "$1" curl -fsS -m 5 http://192.0.2.80/; }
