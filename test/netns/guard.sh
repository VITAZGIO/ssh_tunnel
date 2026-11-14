#!/usr/bin/env bash
# Сторож машины сборки на время стенда (ТЗ 19). Стенд не должен трогать саму
# машину: её сеть, DNS, правила. Сторож:
#   - в начале снимает «отпечаток» сети машины (адреса, маршруты, правила,
#     iptables, nftables, resolv.conf) и каждые 5 с сравнивает: изменилось —
#     пишет, что именно, когда и в каком разделе был стенд;
#   - проверяет здоровье: маршрут по умолчанию, DNS машины, HTTPS до
#     github.com, память, диск, нагрузку. Две проверки подряд плохо — пишет
#     причину в $OUT/abort, run.sh снимает стенд.
# Вызов: guard.sh КАТАЛОГ_OUT (запускает run.sh).
set -u
OUT=$1
LAB=${LAB:-/tmp/sshlab}
# Всё сказанное сторожем — ещё и в журнал сценария: он идёт в журнал шага
# вживую, а guard.log мог бы и не успеть.
exec > >(tee -a "$OUT/suite.log") 2>&1
note() { echo "$(date +%T) [$(cut -c10- "$OUT/section" 2>/dev/null)] $*"; }

fingerprint() {
  {
    echo "## addr"; ip -o addr show | awk '{print $2, $3, $4}'
    echo "## link"; ip -o link show | awk -F': ' '{print $2}' | cut -d@ -f1
    echo "## route"; ip route show table all | grep -v '^local\|^broadcast\|^multicast\|^ff00\|^fe80\|^anycast'
    echo "## rule"; ip rule; ip -6 rule
    echo "## iptables"; iptables-save 2>/dev/null | grep -v '^#' | sed 's/\[[0-9]*:[0-9]*\]//'
    echo "## nft"; nft list ruleset 2>/dev/null | sed -E 's/counter packets [0-9]+ bytes [0-9]+/counter/'
    echo "## resolv.conf"; cat /etc/resolv.conf
    echo "## resolved"; resolvectl dns 2>/dev/null; resolvectl domain 2>/dev/null
    echo "## sysctl"; sysctl -n net.ipv4.ip_forward net.ipv4.conf.all.rp_filter 2>/dev/null
    echo "## mounts"; awk '{print $2, $3}' /proc/self/mounts | grep -E '^/(etc|run/systemd|run/netns|sys/fs/cgroup) ' || true
  } 2>/dev/null
}

health() { # пусто — здоров; иначе причина
  ip route show default | grep -q . || { echo "у машины сборки нет маршрута по умолчанию"; return; }
  timeout 5 getent ahostsv4 github.com >/dev/null || { echo "DNS машины сборки не отвечает (github.com)"; return; }
  timeout 8 curl -sS -o /dev/null -m 6 https://github.com/ 2>/dev/null || { echo "HTTPS до github.com не идёт"; return; }
  local mem; mem=$(awk '/MemAvailable/ {print int($2/1024)}' /proc/meminfo)
  [ "$mem" -ge 300 ] || { echo "памяти осталось ${mem} МБ"; return; }
  local disk; disk=$(df -Pm / | awk 'NR==2 {print $4}')
  [ "$disk" -ge 1024 ] || { echo "на диске осталось ${disk} МБ"; return; }
  local load cpus; load=$(cut -d' ' -f1 /proc/loadavg | cut -d. -f1); cpus=$(nproc)
  [ "$load" -lt $((cpus * 4)) ] || { echo "нагрузка $load при $cpus ядрах"; return; }
  local big; big=$(find "$LAB" -maxdepth 2 -name '*.log' -size +200M 2>/dev/null | head -n1)
  [ -z "$big" ] || { echo "журнал $big больше 200 МБ"; return; }
}

fingerprint > "$OUT/host-before.txt"
note "отпечаток машины сборки снят"
cp "$OUT/host-before.txt" "$OUT/host-last.txt"
bad=0 n=0
while :; do
  sleep 5
  n=$((n + 1))
  fingerprint > "$OUT/host-now.txt"
  if ! cmp -s "$OUT/host-now.txt" "$OUT/host-last.txt"; then
    note "МАШИНА СБОРКИ ИЗМЕНИЛАСЬ:"
    diff "$OUT/host-last.txt" "$OUT/host-now.txt" | head -n 40
    cp "$OUT/host-now.txt" "$OUT/host-last.txt"
  fi
  # Здоровье с сетью — раз в 10 с, чтобы не долбить github.com.
  [ $((n % 2)) = 0 ] || continue
  why=$(health)
  if [ -z "$why" ]; then bad=0; continue; fi
  bad=$((bad + 1))
  note "плохо ($bad): $why"
  if [ "$bad" -ge 2 ] && [ ! -e "$OUT/abort" ]; then
    echo "$why" > "$OUT/abort"
    note "прошу снять стенд"
  fi
done
