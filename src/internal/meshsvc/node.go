package meshsvc

// Юнит и установка хоста плагинов сервера (ssh_tunnel_node, ТЗ 9):
// «Зеркальные папки», «Файлы устройства», «Общий буфер». Отдельный процесс
// без root — панель только ставит, запускает, останавливает и настраивает
// его (через internal/nodeapi.Client по управляющему сокету).

import (
	"fmt"
	"strings"

	"sshtunnel/internal/nodeapi"
)

// NodeUnitName — юнит systemd хоста плагинов.
const NodeUnitName = nodeapi.Unit

// NodeUnit — юнит хоста плагинов: как у meshd, DynamicUser даёт ему свой
// пользователь системы по имени юнита (ssh_tunnel_node) без входа и домашней
// папки; данные — только в StateDirectory, управляющий сокет для панели —
// в RuntimeDirectory. Запускается после meshd: без сети устройств плагины
// никого не опознают. Порты — только на localhost; наружу — только
// источники списка блокировки и проверка сайтов (ТЗ 15); системные вызовы
// — набор обычной службы (ТЗ 13).
func NodeUnit() string {
	return `[Unit]
Description=ssh_tunnel - плагины сервера (зеркальные папки, файлы, буфер, копии, машины, блокировка, проверка сайта)
After=network.target meshd.service
Wants=meshd.service

[Service]
ExecStart=` + nodeapi.Binary + `
Restart=always
RestartSec=2
DynamicUser=yes
StateDirectory=` + nodeapi.User + `
RuntimeDirectory=` + nodeapi.User + `
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
# ТЗ 13: защита строже — НЕ ПРОВЕРЕНО на настоящем systemd (в сборке его
# нет). Если служба не стартует, убери строки ниже до «[Install]» и
# сообщи: systemctl status ssh_tunnel_node, journalctl -u ssh_tunnel_node.
ProtectProc=invisible
ProcSubset=pid
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6
SystemCallFilter=@system-service
SystemCallErrorNumber=EPERM
# Наружу ходят два плагина ТЗ 15: «Блокировка рекламы» качает источники
# списка, «Проверить сайт» проверяет сайты из сети сервера. Поэтому сеть не
# ограничена адресами; порты плагинов всё равно слушают только 127.0.0.1.
PrivateTmp=yes
PrivateDevices=yes

[Install]
WantedBy=multi-user.target
`
}

// NodeInstallScript — установка или обновление хоста плагинов версии version
// (готовый файл со страницы этого релиза — он собирается вместе с
// остальной программой, на сервере не компилируется).
func NodeInstallScript(version string) string {
	return fmt.Sprintf(`set -euo pipefail
BIN=""
case "$(uname -m)" in
  x86_64)        BIN=ssh_tunnel_node ;;
  aarch64|arm64) BIN=ssh_tunnel_node_arm64 ;;
esac
if [ -z "$BIN" ]; then
  echo "неизвестная архитектура $(uname -m) — для неё готового файла нет" >&2
  exit 1
fi
if ! command -v curl >/dev/null; then
  echo "нужен curl" >&2
  exit 1
fi
curl -fsSL -o %s.new "%s"
chmod +x %s.new
mv %s.new %s
cat > /etc/systemd/system/%s <<'EOF_UNIT'
%s
EOF_UNIT
systemctl daemon-reload
systemctl enable %s
systemctl restart %s
echo "ssh_tunnel_node запущен"
`, nodeapi.Binary, downloadURL(version), nodeapi.Binary, nodeapi.Binary, nodeapi.Binary,
		NodeUnitName, strings.TrimRight(NodeUnit(), "\n"), NodeUnitName, NodeUnitName)
}

// NodeUninstallScript — снять службу; deleteData — стереть и данные
// (StateDirectory систематически удаляет и сам systemd при disable только
// если задано RemoveOnStop — мы его не ставим, поэтому стираем сами).
func NodeUninstallScript(deleteData bool) string {
	rm := ""
	if deleteData {
		rm = "rm -rf " + nodeapi.DataDir + "\n"
	}
	return fmt.Sprintf(`set -euo pipefail
systemctl disable --now %s >/dev/null 2>&1 || true
rm -f /etc/systemd/system/%s
rm -f %s
systemctl daemon-reload
%s`, NodeUnitName, NodeUnitName, nodeapi.Binary, rm)
}
