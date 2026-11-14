#!/usr/bin/env bash
# Запуск сценария стенда так, чтобы его журнал дошёл до GitHub, что бы ни
# случилось (ТЗ 19):
#   - сценарий пишет в файл $LAB/out/suite.log и идёт в своём сеансе (setsid):
#     фоновые процессы стенда не держат трубу шага, а по пределу вся группа
#     убивается одним kill -9 -- -PGID;
#   - файл тут же выводится в журнал шага (tail -F);
#   - сторож машины сборки (guard.sh) снимает стенд, если машина сборки
#     начала болеть (сеть, DNS, память, диск, нагрузка), — раньше, чем
#     раннер GitHub потеряет связь и унесёт журнал с собой;
#   - на пределе — снимок состояния (dump.sh), уборка, код 124.
#
#   sudo LAB=/tmp/sshlab bash test/netns/run.sh docker 600
set -uo pipefail

SUITE=$1
LIMIT=${2:-600}
LAB=${LAB:-/tmp/sshlab}
HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
OUT="$LAB/out"
export LAB HERE

mkdir -p "$OUT"
: > "$OUT/suite.log"
rm -f "$OUT/abort" "$OUT/pgid" "$OUT/rc"
echo "$(date +%T) старт" > "$OUT/section"

say() { echo "### $(date +%T) run.sh: $*"; }

# Сторож машины сборки: отдельный сеанс, высокий приоритет.
setsid nice -n -10 bash "$HERE/guard.sh" "$OUT" > "$OUT/guard.log" 2>&1 < /dev/null &
GUARD=$!

# Сценарий: свой сеанс; его PGID = PID первого bash.
setsid bash -c 'echo $$ > "$1/pgid"; shift; bash "$@"; echo $? > "'"$OUT"'/rc"' _ "$OUT" \
  "$HERE/$SUITE.sh" >> "$OUT/suite.log" 2>&1 < /dev/null &
for _ in $(seq 1 50); do [ -s "$OUT/pgid" ] && break; sleep 0.1; done
PGID=$(cat "$OUT/pgid")
say "сценарий $SUITE, PGID $PGID, предел ${LIMIT} с"

tail -n +1 -F "$OUT/suite.log" 2>/dev/null &
TAIL=$!

alive() { kill -0 "$PGID" 2>/dev/null; }
end=$((SECONDS + LIMIT))
why=""
while alive; do
  if [ -e "$OUT/abort" ]; then why="сторож: $(cat "$OUT/abort")"; break; fi
  if [ "$SECONDS" -ge "$end" ]; then why="предел ${LIMIT} с"; break; fi
  sleep 1
done
sleep 1
kill "$TAIL" 2>/dev/null

if [ -n "$why" ]; then
  echo "::error::стенд снят ($why), раздел: $(cat "$OUT/section")"
  say "снимок состояния перед уборкой → dump-abort.txt"
  timeout 120 bash "$HERE/dump.sh" > "$OUT/dump-abort.txt" 2>&1
  rc=124
else
  rc=$(cat "$OUT/rc" 2>/dev/null || echo 1)
fi

# Всё, что осталось от группы сценария, — убить; потом уборка самого сценария.
kill -9 -- "-$PGID" 2>/dev/null
say "уборка"
LAB_CLEANUP_ONLY=1 timeout 120 bash "$HERE/$SUITE.sh" > "$OUT/cleanup.log" 2>&1 < /dev/null
tail -n 20 "$OUT/cleanup.log"

kill -- "-$GUARD" 2>/dev/null
say "код $rc"
exit "$rc"
