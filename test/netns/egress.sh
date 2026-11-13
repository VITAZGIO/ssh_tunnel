#!/usr/bin/env bash
# Стенд выхода сервера A через сервер B (ТЗ 10) на настоящем ядре.
# Запуск: sudo RP=2 LAB=/tmp/sshlab test/netns/egress.sh (нужны собранные
# $LAB/labd, $LAB/ssh_tunnel_vpn_linux, $LAB/panel.test — см. workflow).
#
# Службы systemd выхода сервера запускает настоящий systemd, но в
# пространстве a: заранее кладём им дополнения NetworkNamespacePath. Без
# этого они поменяли бы маршруты самой машины сборки — поэтому до запуска
# проверяем, что дополнения подхватились.
source "$(dirname "$0")/lab.sh"

UNITS="ssh_tunnel_egress.service ssh_tunnel_egress-rules.service ssh_tunnel_egress-rollback.service"

cleanup() {
  set +e
  ip netns exec a sh /var/lib/ssh_tunnel_egress/off.sh >/dev/null 2>&1
  systemctl stop $UNITS ssh_tunnel_egress-rollback.timer >/dev/null 2>&1
  for u in $UNITS; do rm -rf "/etc/systemd/system/$u.d"; done
  systemctl daemon-reload
  lab_down
}
trap cleanup EXIT

echo "== стенд: rp_filter=$RP"
lab_down
lab_inet
lab_b
host a br0 203.0.113.10/24 203.0.113.1
ns a sysctl -qw net.ipv4.ip_forward=1

# «Контейнер» за DNAT на A (как опубликованный порт Docker): actr 172.31.0.2.
ip netns add actr
ns actr ip link set lo up
set_rp actr
ip -n a link add actr0 type veth peer name eth0 netns actr
ns a ip addr add 172.31.0.1/24 dev actr0
ns a ip link set actr0 up
ns a sysctl -qw net.ipv4.conf.actr0.rp_filter="$RP"
ns actr ip addr add 172.31.0.2/24 dev eth0
ns actr ip link set eth0 up
ns actr ip route add default via 172.31.0.1
ns a nft -f - <<'NFT'
table ip lab_docker {
  chain pre { type nat hook prerouting priority dstnat; policy accept;
    tcp dport 8081 dnat to 172.31.0.2:80 }
  chain post { type nat hook postrouting priority srcnat; policy accept;
    ip saddr 172.31.0.0/24 oifname != "actr0" masquerade }
}
NFT
ns a "$LAB/labd" -http 203.0.113.10:8080 > "$LAB/labd-a.log" 2>&1 &
ns actr "$LAB/labd" -http 172.31.0.2:80 > "$LAB/labd-actr.log" 2>&1 &
sed -e 's/203.0.113.20/203.0.113.10/' -e "s#sshd_b.pid#sshd_a.pid#" "$LAB/sshd_config" > "$LAB/sshd_config_a"
ns a /usr/sbin/sshd -f "$LAB/sshd_config_a" -E "$LAB/sshd_a.log"
eventually 10 ns inet curl -fsS -m 2 --interface 192.0.2.99 http://203.0.113.10:8081/

# Службы выхода — в пространстве a, без доступа к resolved машины сборки.
for u in $UNITS; do
  mkdir -p "/etc/systemd/system/$u.d"
  cat > "/etc/systemd/system/$u.d/lab-netns.conf" <<CONF
[Service]
NetworkNamespacePath=/run/netns/a
InaccessiblePaths=-/run/systemd/resolve
CONF
done
systemctl daemon-reload
for u in $UNITS; do
  got=$(systemctl show -p NetworkNamespacePath --value "$u")
  if [ "$got" != "/run/netns/a" ]; then
    echo "::error::$u: NetworkNamespacePath=«$got» — службы задели бы саму машину сборки, стоп"
    exit 1
  fi
done

echo "== сценарии выхода сервера (Go, внутри a)"
set +e
ns a env LAB="$LAB" LAB_SH="$HERE/lab.sh" LAB_VPN_BIN="$LAB/ssh_tunnel_vpn_linux" LAB_LINK="$(link_b)" \
  "$LAB/panel.test" -test.run '^TestEgressLab$' -test.v -test.timeout 25m
rc=$?
set -e
echo "== журнал sshd B (хвост)"; tail -n 20 "$LAB/sshd_b.log" || true
exit $rc
