package meshsvc

import "fmt"

// UDPRelayUnitName — служба ретранслятора UDP (sshtunnel/cmd/udprelay):
// слушает только 127.0.0.1:47830, клиенты достают до неё через SSH и
// гонят через неё UDP — звонки, игры, QUIC (видео YouTube).
const UDPRelayUnitName = "udprelay.service"

// UDPRelayInstallScript — установка ретранслятора из панели сервера (bash
// от root): готовый файл из релиза той же версии, что и панель, юнит
// systemd и исключение в брандмауэре пользователей туннеля. Без сборки на
// месте, в отличие от мастера настройки VPS: панель сама ставится из
// релиза, значит, и ретранслятор для этой архитектуры там есть.
func UDPRelayInstallScript(version string) string {
	return fmt.Sprintf(`set -euo pipefail
case "$(uname -m)" in
  x86_64)        BIN=udprelay ;;
  aarch64|arm64) BIN=udprelay_arm64 ;;
  *) echo "Для архитектуры $(uname -m) готового ретранслятора нет — поставь его мастером настройки VPS" >&2; exit 1 ;;
esac
echo "скачиваю ретранслятор"
curl -fsSL -o /usr/local/bin/udprelay.new "%s"
chmod +x /usr/local/bin/udprelay.new
mv -f /usr/local/bin/udprelay.new /usr/local/bin/udprelay
cat > /etc/systemd/system/%s <<'EOF_UNIT'
[Unit]
Description=ssh_tunnel - ретранслятор UDP (только 127.0.0.1)
After=network.target

[Service]
ExecStart=/usr/local/bin/udprelay
Restart=always
RestartSec=2
DynamicUser=yes
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes

[Install]
WantedBy=multi-user.target
EOF_UNIT
systemctl daemon-reload
systemctl enable %s
systemctl restart %s
echo "ретранслятор UDP запущен (127.0.0.1:47830)"
`, downloadURL(version), UDPRelayUnitName, UDPRelayUnitName, UDPRelayUnitName) + FirewallExemption
}
