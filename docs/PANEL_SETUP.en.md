# Web panel for a VPS (ssh_tunnel_panel)

A separate program. Installed **on the server itself** and adds clients with
a button — no console, no manual `useradd`, no editing `authorized_keys`.

## How it's different from the local panel

`ssh_tunnel_linux -web` is the program's window on the client's own
computer: it manages only its own connection, reachable by a token in the
address and only from that machine.

`ssh_tunnel_panel` is a separate program on the VPS itself. It doesn't
connect anywhere on its own — it manages **who is allowed to connect to this
server**: it adds and removes clients, each with its own unix user and its
own key, and shows traffic and who's currently online. It faces the
internet, so the login is a username and password, not a token in a link.

If you only need one computer you trust, and you're happy setting it up
yourself, the panel isn't necessary — [manual setup](SERVER_SETUP.md) is
enough. The panel earns its keep once there's more than one client (your own
several devices, or you're handing out access to other people) and adding
each one by hand over SSH gets old.

## The panel runs as root — and that's worth understanding

To add and remove system users, the panel needs root. That's not an
oversight — part of its job is simply impossible without it. The systemd
unit narrows everything else it can touch (`PrivateTmp`,
`ProtectSystem=true` — `/usr` and `/boot` read-only, `NoNewPrivileges`,
kernel-settings protection), but the fact that "a process facing the
internet runs as root" doesn't go away. Three practical consequences follow:

1. **Password** — change the one-time password to your own, long and
   unique, right after the first login. The panel throttles brute-forcing
   itself (a growing delay, a lockout after several failures), but that's
   not a substitute for a real password.
2. **Domain and certificate** — don't leave the panel as bare HTTP on a port
   open to the internet. Either your own domain with TLS through a reverse
   proxy, or the built-in auto-certificate (below) — never bare HTTP without
   a domain and certificate.
3. **Address restriction**, where possible — a firewall or reverse proxy
   that only lets the panel's port through from your own address or a VPN.
   The panel needs no special flags for this — it's handled by whatever
   sits in front of it (ufw, nginx, Tailscale/NetBird).

## One-command install

One block — paste it whole on a fresh server as root. Nothing to download
ahead of time: it fetches the right binary for the server's architecture
from GitHub itself, installs the service, and prints the panel's address
and password.

**Option 1 — no domain of your own.** The panel listens only on
`127.0.0.1:47823`; there's no need to open it to the outside — reach it
through an SSH tunnel, or add a domain and nginx/Caddy later (see "Domain
and TLS" below):

```bash
curl -fsSL https://raw.githubusercontent.com/VITAZGIO/ssh_tunnel/main/packaging/panel/install.sh \
  | sudo bash -s -- --lan --ssh-host=THIS_SERVERS_ADDRESS
```

**Option 2 — with your own domain.** The domain must already have an A
record pointing at this server — then the panel gets its own Let's Encrypt
certificate and is reachable over HTTPS right away:

```bash
curl -fsSL https://raw.githubusercontent.com/VITAZGIO/ssh_tunnel/main/packaging/panel/install.sh \
  | sudo bash -s -- --domain=panel.example.com
```

Either way, the command prints the panel's address and the one-time
password at the end — you can also look them up any time with:

```bash
journalctl -u ssh_tunnel_panel -n 50
```

Already have your own reverse proxy (nginx/Caddy) and want the panel behind
it without an open port — a third option, without `--lan` and without
`--domain` (this is the default mode, see "Domain and TLS" below):

```bash
curl -fsSL https://raw.githubusercontent.com/VITAZGIO/ssh_tunnel/main/packaging/panel/install.sh \
  | sudo bash -s -- --ssh-host=THIS_SERVERS_ADDRESS
```

Already downloaded the binary yourself (say, built it from source) — pass
its path as the first argument after `--`:

```bash
sudo bash install.sh ./ssh_tunnel_panel --lan --ssh-host=SERVER_ADDRESS
```

## First login

On first launch, if there are no users yet, the panel creates an `admin`
account with a random one-time password itself and prints it to the log. It
will require changing that password right after the first login — it isn't
stored anywhere else, and if it's lost, the only way out is to delete
`/etc/ssh_tunnel_panel/users.json` and restart the service
(`systemctl restart ssh_tunnel_panel`): the panel will create a fresh
one-time account, same as on first launch.

## Domain and TLS

Two paths, both supported without reinstalling — just different flags:

**Behind nginx/Caddy (default).** The panel listens only on
`127.0.0.1:47823`; the certificate and domain are the reverse proxy's job.
A typical nginx config:

```nginx
server {
    listen 443 ssl;
    server_name panel.example.com;
    ssl_certificate     /etc/letsencrypt/live/panel.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/panel.example.com/privkey.pem;

    location / {
        proxy_pass http://127.0.0.1:47823;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-Proto $scheme;
    }
}
```

The `X-Forwarded-Proto` header matters: it's how the panel knows the
connection arrived over HTTPS, and sets the session cookie with the
`Secure` flag.

**Built-in auto-certificate**, if there's no separate reverse proxy on the
machine:

```bash
sudo bash install.sh ./ssh_tunnel_panel --domain=panel.example.com
```

The panel obtains and renews its own Let's Encrypt certificate
(`golang.org/x/crypto/acme/autocert`), listening on `:443` for the panel and
`:80` only for the domain check Let's Encrypt requires. The domain must
already point at the server via an A record before you run this — otherwise
getting a certificate will fail.

## Changing the port

The port is set with the `-listen` flag (default `127.0.0.1:47823`). Edit
`ExecStart` in `/etc/systemd/system/ssh_tunnel_panel.service`:

```bash
sudo sed -i 's|^ExecStart=.*|ExecStart=/usr/local/bin/ssh_tunnel_panel -listen 127.0.0.1:8443|' \
  /etc/systemd/system/ssh_tunnel_panel.service
sudo systemctl daemon-reload
sudo systemctl restart ssh_tunnel_panel
```

With `--domain` the port doesn't matter — the panel always listens on `:80`
and `:443`.

## The address clients connect to

Separate from the panel's own address: clients connect over SSH to the
server, not to the web panel. With `--domain` the panel takes this address
from the same domain automatically. Without `--domain` (`--lan` or the
default reverse-proxy mode), set it explicitly at install time:

```bash
sudo bash install.sh ./ssh_tunnel_panel --lan --ssh-host=203.0.113.10
```

Without this address the panel will still create a client, but won't be
able to build a ready config for it — it will just report that no address
is set.

## Adding your first client

1. Open the panel, log in, change the one-time password.
2. On the main screen — "+ Add client": a device name ("Laptop", "Phone")
   and type (Windows, Linux, Android).
3. Once created, the panel shows a ready config right away: a QR code with a
   "Copy link" button under it (an `sshtunnel://` link, see
   `docs/IMPORT_LINK.md`), "Copy" and "Download file" — plus the address,
   port, user, and key as plain text. The QR code and link exist **only at
   this moment**; later the client's "Show settings" button gives the file
   and fields, but no QR.
4. On the new device — import it: on a phone scan the QR code (in the app or
   with the camera), on a computer paste the link with "Paste"; a file still
   opens with "Import".

From there, the client list shows traffic, the number of active sessions,
and the time of the last connection; buttons let you freeze (temporarily
deny login without removing the key from the panel's storage), disconnect
(kill current sessions without touching the key), and delete (erase the
user and key for good).

## Device network

"Settings" → "Network server": installs the panel's own `meshd`. A network can
have any number of servers and none is the main one: each runs its own
`meshd`, devices stay connected to every server they have a profile for (same
network key). "Networks" → "Servers of the network": this server, other
servers of the network as reported by devices (and whether their access rules
are the same), servers the panel suggests to devices. Access rules, the
journal, names and removal apply to this server only — the panel says so. The
old way — a "secondary" server forwarding to another one's `meshd` — still
works and is marked "old way"; to move off it: "Settings" → "Switch to an own
network service". The device card has "Approve" / "Reject" (when "New
devices — after approval" is on for the network in "Networks"), "Freeze in
network" and the exit chain the panel suggests to the device (the device may
change it — the panel shows its value). Approval and freezing also apply to
this server only; they need `meshd` version 5 ("Update meshd").

Panel API for this (session auth, errors — 400 `{"error"}`):
`POST /api/mesh/net/approval` `{"net","on"}`, `POST /api/mesh/approve`
`{"net","device"}`, `POST /api/mesh/freeze` `{"net","device","on"}`,
`POST /api/mesh/exit` `{"net","device","entry":"host:port","via":[…]}` →
`{"ok":true,"exit":{entry,via,t,by,byId}}`. `GET /api/mesh/state` gains
`approval` per network and `pending`, `frozen`, `profiles` (`[{k,n,a,v,t,b}]`:
key, name, active, chain, time, set by) and `exitSet` per device.
`POST /api/mesh/check` may answer `reason` `"pending"` or `"frozen"`. The
simple access mode is saved in one request, `POST /api/access/simple`
`{"enabled","groups","rules","clients":{"<client id>":[groups]}}`: everything
is checked before anything is written.

Details and the upgrade order for several servers — in [MESH.md](MESH.md)
(Russian).

## Server plugins

The "Plugins" tab: the server itself joins the device network — mirrored
folders, a shared folder ("Device files"), the shared clipboard and backups
run right on the server, through the same code that serves them on devices.

Plugins live in their own process, `ssh_tunnel_node`, without root: its own
system user (`DynamicUser` in systemd, like `meshd`), its own data folder
`/var/lib/ssh_tunnel_node`. The panel only installs, starts, stops and
configures it over a control socket
(`/run/ssh_tunnel_node/control.sock`) — root reaches it regardless of file
permissions, nobody else does.

Who called a plugin is told by `meshd` itself (as for any other port of the
server) — it cannot be faked from another machine or another system user.
Until the device network is installed, or its version is too old to answer
"who is calling", plugins let nobody in — the host card says so.

- "Install plugin host" downloads `ssh_tunnel_node` from this release's page
  and starts the service; once installed, the per-plugin switches, the total
  disk quota and the list of networks appear.
- For each network: its mirrored folders (name, quota, used space,
  conflicts), the shared folder's used space, and shared-clipboard items.
  The quota is shared by all plugins of all networks together — once full,
  new data is refused with a clear error on the device, and nothing already
  stored is damaged.
- "Remove" stops and removes the service; data (folders, clipboard) stays
  unless "Also delete plugin data" is checked — that cannot be undone.
- **"What blocks the plugins"** — the panel checks "Server in the device
  network" and the access rules against the ports of the enabled plugins
  (47835 files, 47836 folders, 47837 clipboard, 47838 backups). Closed server
  ports are opened with one button — only the plugin ports: each plugin
  identifies every caller itself. The panel never changes access rules on its
  own — it only names who has no rule to `@servers`. On a secondary server
  identification does not work, and the panel says so.
- Each network shows when devices last used each plugin; a mirrored
  folder's quota can be changed right in its row. An unnamed network is shown
  as "Network N".
- Device **backups**: size, versions, "keep", time of the last backup;
  "Delete" removes a backup with all its versions. The same content is stored
  once (within one user). A backup can be read only by its device and devices
  of the same SSH user.
- A network deleted in "Networks" whose plugin data is still here is marked
  "Network deleted, data left" with a "Delete data" button. It is never
  erased on its own; a live network cannot be erased this way.

## Server egress through another server

**Settings → "Server egress → through server B"**: this server's own
traffic (package updates, containers, tunnel users exiting here) leaves
through another ssh_tunnel server. Inbound access (SSH to this server, this
panel, devices, the device network) is not affected — it keeps answering
the way it came in.

To turn it on: create a regular client in server B's panel ("Devices →
Add") and paste its `sshtunnel://…` link here (shown once, right after
creation). The panel installs a Linux VPN client on this server, the same
version as itself (from the same GitHub release, checksum verified), checks
over SSH that server B isn't itself exiting through this server (a loop),
and applies the routes.

After applying there is a 60-second window to confirm, with a button, that
the panel still answers (open it in a new tab or on your phone). Without
confirmation, a separate `systemd` timer — independent of the panel and the
network — reverts everything, including across a reboot or a crash mid-apply.

If server B goes away, the card picks the behaviour: "no internet" (default
— inbound still works, this server's own traffic goes nowhere) or "direct
exit". Details on the client side are in [VPN.md](VPN.md).

## Removing the panel

```bash
sudo systemctl disable --now ssh_tunnel_panel
sudo rm /etc/systemd/system/ssh_tunnel_panel.service
sudo systemctl daemon-reload
sudo rm /usr/local/bin/ssh_tunnel_panel
```

This stops the panel and removes it, but leaves already-created clients
alone — their unix users and keys in `authorized_keys` keep working, and the
sshd restrictions (`Match Group sshtunnel` in `/etc/ssh/sshd_config`,
between the `# BEGIN ssh_tunnel_panel` and `# END ssh_tunnel_panel` lines)
stay in effect. To remove everything:

```bash
# Delete all of the panel's clients (replace tun_* with real names from
# `getent passwd | grep tun_` if you only want to remove some):
for u in $(getent passwd | cut -d: -f1 | grep '^tun_'); do
  sudo userdel -r "$u"
done

# Remove the restriction block from sshd_config (it's between these two
# lines) and the group:
sudo sed -i '/# BEGIN ssh_tunnel_panel/,/# END ssh_tunnel_panel/d' /etc/ssh/sshd_config
sudo systemctl reload ssh
sudo groupdel sshtunnel

# The panel's own data (panel users, client list, certificate cache):
sudo rm -rf /etc/ssh_tunnel_panel
```

## Flags

| Flag | Default | What it does |
|---|---|---|
| `-listen` | `127.0.0.1:47823` | address the panel listens on without `-domain` |
| `-domain` | — | turns on HTTPS with a built-in auto-certificate for this domain |
| `-ssh-host` | taken from `-domain` | address clients use to connect to the server over SSH |
| `-ssh-port` | `22` | the server's SSH port — goes into the client config |
| `-public-url` | built from `-domain` | the panel's own address — goes into the client config |
| `-data-dir` | `/etc/ssh_tunnel_panel` | folder with accounts, clients, and the certificate cache |
