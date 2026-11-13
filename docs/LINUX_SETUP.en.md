# Installing on Linux

For servers and workstations, amd64 and arm64. The binary is built statically:
it needs no libraries and no particular glibc version — it runs the same on
Debian, Fedora, Arch, and even on Alpine with musl.

```bash
# download and make executable
curl -LO https://github.com/VITAZGIO/ssh_tunnel/releases/latest/download/ssh_tunnel_linux
chmod +x ssh_tunnel_linux

# set the server once
./ssh_tunnel_linux -host YOUR_SERVER -user tunnel -save

# run: tunnel only...
./ssh_tunnel_linux

# ...or with the web interface
./ssh_tunnel_linux -web

# ...or with the panel open to your home network
./ssh_tunnel_linux -web -web-lan
```

The ARM build and the checksums are on the
[release page](https://github.com/VITAZGIO/ssh_tunnel/releases/latest).

---

## Commands for your distribution

The program itself is identical everywhere — what differs is how you install
`curl`, how you open a port in the firewall, and how a service is set up. Open
your system:

<details>
<summary><b>Ubuntu · Debian · Linux Mint · Raspberry Pi OS</b></summary>

```bash
sudo apt update && sudo apt install -y curl

curl -LO https://github.com/VITAZGIO/ssh_tunnel/releases/latest/download/ssh_tunnel_linux
chmod +x ssh_tunnel_linux
sudo install -m 755 ssh_tunnel_linux /usr/local/bin/ssh_tunnel_linux

ssh_tunnel_linux -host YOUR_SERVER -user tunnel -save
ssh_tunnel_linux -web -web-lan
```

The panel opens at `http://MACHINE_ADDRESS:47821`. If ufw is on, let only your
home network in:

```bash
sudo ufw allow from 192.168.0.0/16 to any port 47821 proto tcp
```

Autostart — the "Start at system boot" checkbox in the panel itself.

On a Raspberry Pi and other ARM machines the file is named
`ssh_tunnel_linux_arm64`.
</details>

<details>
<summary><b>Proxmox VE</b></summary>

Proxmox is Debian, but you normally work there as `root` and with no user
session. So the service goes in as a **system** one rather than a user one, and
the autostart checkbox in the panel will not help here — it targets
`systemctl --user`.

```bash
apt update && apt install -y curl

curl -LO https://github.com/VITAZGIO/ssh_tunnel/releases/latest/download/ssh_tunnel_linux
chmod +x ssh_tunnel_linux
install -m 755 ssh_tunnel_linux /usr/local/bin/ssh_tunnel_linux

ssh_tunnel_linux -host YOUR_SERVER -user root -save
```

The system service (starts at host boot, before any login):

```bash
tee /etc/systemd/system/ssh_tunnel.service >/dev/null <<'EOF'
[Unit]
Description=ssh_tunnel
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/bin/ssh_tunnel_linux -web -web-lan
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable --now ssh_tunnel
```

Settings then live in `/root/.config/ssh_tunnel/`. If the Proxmox firewall is on
(Datacenter → Firewall), port `47821` has to be allowed there, in the Proxmox web
UI — `ufw` plays no part in it.
</details>

<details>
<summary><b>Fedora · RHEL · CentOS Stream · Rocky · AlmaLinux</b></summary>

```bash
sudo dnf install -y curl

curl -LO https://github.com/VITAZGIO/ssh_tunnel/releases/latest/download/ssh_tunnel_linux
chmod +x ssh_tunnel_linux
sudo install -m 755 ssh_tunnel_linux /usr/local/bin/ssh_tunnel_linux

ssh_tunnel_linux -host YOUR_SERVER -user tunnel -save
ssh_tunnel_linux -web -web-lan
```

The firewall here is `firewalld`, not `ufw`:

```bash
sudo firewall-cmd --permanent --add-port=47821/tcp
sudo firewall-cmd --reload
```

SELinux does not get in the way: the program touches neither system directories
nor other people's ports — everything of its own stays in the user's home.
</details>

<details>
<summary><b>Arch · Manjaro · EndeavourOS</b></summary>

```bash
sudo pacman -S --needed curl

curl -LO https://github.com/VITAZGIO/ssh_tunnel/releases/latest/download/ssh_tunnel_linux
chmod +x ssh_tunnel_linux
sudo install -m 755 ssh_tunnel_linux /usr/local/bin/ssh_tunnel_linux

ssh_tunnel_linux -host YOUR_SERVER -user tunnel -save
ssh_tunnel_linux -web -web-lan
```

There is no firewall by default at all — if you installed one, open port `47821`
there. Everything else is as on Ubuntu, same systemd.
</details>

<details>
<summary><b>openSUSE</b></summary>

```bash
sudo zypper install -y curl

curl -LO https://github.com/VITAZGIO/ssh_tunnel/releases/latest/download/ssh_tunnel_linux
chmod +x ssh_tunnel_linux
sudo install -m 755 ssh_tunnel_linux /usr/local/bin/ssh_tunnel_linux

ssh_tunnel_linux -host YOUR_SERVER -user tunnel -save
ssh_tunnel_linux -web -web-lan
```

The port is opened through `firewalld`, as on Fedora:

```bash
sudo firewall-cmd --permanent --add-port=47821/tcp && sudo firewall-cmd --reload
```
</details>

<details>
<summary><b>Alpine Linux</b></summary>

The one system on this list with **no systemd** — it runs OpenRC. The program
works fine (the binary is static, musl is no obstacle), but autostart is set up
differently, and the checkbox in the panel will not do it.

```sh
apk add curl

curl -LO https://github.com/VITAZGIO/ssh_tunnel/releases/latest/download/ssh_tunnel_linux
chmod +x ssh_tunnel_linux
install -m 755 ssh_tunnel_linux /usr/local/bin/ssh_tunnel_linux

ssh_tunnel_linux -host YOUR_SERVER -user tunnel -save
```

The OpenRC service:

```sh
cat > /etc/init.d/ssh_tunnel <<'EOF'
#!/sbin/openrc-run
command="/usr/local/bin/ssh_tunnel_linux"
command_args="-web -web-lan"
command_background=true
pidfile="/run/ssh_tunnel.pid"
depend() { need net; }
EOF

chmod +x /etc/init.d/ssh_tunnel
rc-update add ssh_tunnel default
rc-service ssh_tunnel start
```
</details>

---

## Starting at boot

At the bottom of the settings there are two checkboxes side by side:

- **"Start at system boot"** — the program writes the systemd unit itself,
  enables it, and allows it to run without the user logging in. That last part
  needs administrator rights: if the system asks for a password, a field for it
  appears right there — the password is used once and stored nowhere.
- **"Connect immediately on launch"** — the tunnel comes up on its own as soon
  as the program starts, no need to press "Connect" in the panel. On by
  default — that's the ordinary behavior; turn it off if you'd rather start the
  tunnel by hand.

Together they give you "turn the machine on, the tunnel is already up": the
first checkbox brings the program up at boot, the second connects it right
away.

If Docker is found on the machine, a third checkbox appears a little further
down — turn on autostart for Docker too, so that after a reboot the containers
and the tunnel come up together.

This works anywhere systemd is present. The exceptions are Proxmox under `root`
and Alpine: there the service is set up by hand, with the commands above.

---

## Docker: containers via the server or direct

In the VPN version the **"Docker"** app (Apps → Docker) lists the machine's
containers with three buttons each: **like the computer** (default), **via
the server**, **direct**. A compose project switches as a whole; the label
`ssh_tunnel.route=tunnel|direct|host` in `compose.yaml` wins over the buttons.

- "Via the server" is strict and never goes around: tunnel off,
  reconnecting, the seconds after "Turn off" or the program crashed — the
  container has no internet; "always direct" and per-program rules do not
  apply to it; its DNS also goes only into the tunnel and is resolved via the
  server.
- The **"no VPN — no internet"** checkbox on a "via the server" container:
  no internet with the VPN off and with the program not running (without it
  a VPN turned off puts the container back to "like the computer").
- The home network, the machine itself, neighbouring containers and ports
  published to the home network (`-p`) work with any choice. A port forwarded
  on the router to the internet does not work for a "via the server"
  container — replies would go into the tunnel; pick "direct" or "like the
  computer" for such a container.
- `network_mode: host` and containers in another container's network cannot
  be switched on their own.
- Needs access to the `/var/run/docker.sock` socket (the VPN version has it —
  it runs as root); the `docker` program is not needed.
- How it works: `ip rule from <container address>` rules into tables
  7094–7096, priorities 9400–9401 (9402–9403 with "no VPN — no internet";
  `ip rule`, `ip route show table 7094`). "Via the server" containers skip
  Docker's address rewriting (chain `SSH_TUNNEL_DOCKER` in
  `iptables -t nat`): the program recognises their connections by the
  address. Turn the VPN or the app off — they are removed (except those with
  the checkbox).
- Tested on a real kernel: the `netns-lab` GitHub build sets up a lab of
  network namespaces with its own Docker and runs all of this with
  `rp_filter` 0, 1 and 2 (`test/netns/`).

From the command line:

```bash
ssh_tunnel_linux docker
ssh_tunnel_linux docker set container=qbittorrent route=tunnel
ssh_tunnel_linux docker set project=media route=direct
ssh_tunnel_linux docker set container=qbittorrent strict=true
```

---

## This server exiting through another ssh_tunnel server

If this machine is a panel server (`ssh_tunnel_panel`) and its own traffic
(not just clients') should exit through another ssh_tunnel server, that's a
separate panel feature, set up there: "Settings → Server egress → through
server B", see
[PANEL_SETUP.en.md](PANEL_SETUP.en.md#server-egress-through-another-server).
No need to install `ssh_tunnel_vpn_linux` by hand for this — the panel does
it itself (the same version as the panel, checksum verified) and makes sure
inbound access (SSH, the panel itself, devices) isn't affected. How the
client side works is in [VPN.md](VPN.md#выход-сервера-через-другой-сервер-режим-панели-сервера).

## Web interface

The very same one the Windows build shows in its window, served on the fixed
port `47821`. The port is deliberately unusual so it does not collide with
whatever is already running on a server.

By default the panel listens on `127.0.0.1` only and the address carries an
access token, printed at startup. Nothing reaches it from the network, and no
page that happens to be open in your browser can either.

The **`-web-lan`** flag turns it into a home service like Home Assistant: fixed
address, no token, opened straight at the machine's address —

```
http://192.168.1.203:47821
```

Only your own side gets in: local network addresses (`192.168.x.x`, `10.x.x.x`,
`172.16–31.x.x`) and mesh VPNs (`100.64.x.x`). A public address still needs the
token, and the `Origin` check keeps a foreign site in the next browser tab from
pressing anything on your behalf. Anyone on that network can control the tunnel —
usually the point at home, not something to leave on in someone else's network.

---

## Proxy in your shell

Wire the proxy into the current shell (curl, git, apt, docker, pip, npm):

```bash
source ~/.config/ssh_tunnel/proxy.env
```

As a systemd service — see [packaging/linux](../packaging/linux):

```bash
./packaging/linux/install.sh ./ssh_tunnel_linux --lan   # without --lan: local machine only
systemctl --user enable --now ssh_tunnel
```

---

Setting up a new server for the tunnel? See
[first-time server setup](SERVER_SETUP.md) (Russian only for now).
