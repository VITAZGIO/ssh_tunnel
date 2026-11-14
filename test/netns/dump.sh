#!/usr/bin/env bash
# Снимок состояния стенда и машины сборки (ТЗ 19): что запущено, сеть
# машины и каждого пространства, Docker стенда, хвосты журналов. Каждая
# команда — с пределом: снимок не должен повиснуть сам.
set -u
LAB=${LAB:-/tmp/sshlab}
t() { echo "---- $*"; timeout 10 "$@" 2>&1 | head -n 300; }

echo "==== снимок $(date +%T), раздел: $(cat "$LAB/out/section" 2>/dev/null)"
t uptime
t free -m
t df -h / /tmp
t ps -eo pid,ppid,pgid,stat,etime,pcpu,rss,args --forest
echo "==== машина сборки"
t ip -br addr
t ip route
t ip rule
t iptables-save
t nft list ruleset
t cat /etc/resolv.conf
t resolvectl status
for n in $(ip netns list | awk '{print $1}'); do
  echo "==== пространство $n"
  t ip -n "$n" -br addr
  t ip -n "$n" route show table all
  t ip -n "$n" rule
  t ip netns exec "$n" iptables-save
  t ip netns exec "$n" nft list ruleset
  t ip netns exec "$n" ss -tunap
done
if [ -S /run/docker-lab.sock ]; then
  echo "==== Docker стенда"
  t docker -H unix:///run/docker-lab.sock ps -a
  t docker -H unix:///run/docker-lab.sock network ls
fi
echo "==== dmesg"
t dmesg -T --level=err,warn
t sh -c 'dmesg -T | tail -n 60'
for f in "$LAB"/*.log; do
  [ -f "$f" ] || continue
  echo "==== $f"; tail -n 80 "$f"
done
