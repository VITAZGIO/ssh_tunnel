# Commands and AI

Everything you can press in the window can be done with a command — in
PowerShell, in a Linux terminal, from your own script. Or you can hand the
buttons to an AI on this computer (Claude Desktop, Claude Code): it turns on
Bypass or adds a rule by itself.

Commands talk to the running app: the window (or the service) must be
running. A command finds it by the `api.json` file in the settings folder.

## Commands

In PowerShell (on Linux — `ssh_tunnel_linux` instead of `ssh_tunnel.exe`):

- `ssh_tunnel.exe state` — what is on: tunnel, device network, mode.
- `ssh_tunnel.exe tunnel off` — turn the tunnel off. The device network
  stays as it was. `tunnel on` — turn it on and wait for the connection.
- `ssh_tunnel.exe mesh on` or `mesh off` — the device network.
- `ssh_tunnel.exe rules` — server rules. `rules add direct youtube.com` —
  always direct, `rules add tunnel site.com` — always via the server,
  `rules add app steam.exe` — a program into the filter list,
  `rules remove …` — remove, `rules mode only` — only listed programs go
  through the tunnel. `rules fakeip on|off` — fake addresses in VPN-mode DNS,
  `rules add realip .deepseek.com` — the name always gets its real address.
- `ssh_tunnel.exe apps` — plugins: `apps add calendar`, `apps remove …`,
  `apps star …`, `apps on …`, `apps off …`.
- `ssh_tunnel.exe check` — self-check, `check site youtube.com` — check a
  site (needs the Site check plugin).
- `ssh_tunnel.exe help` — all commands. The list is built from the app
  itself: whatever a new version adds is in the commands right away.

The answer is text. For scripts add `--json`, for example
`$s = ssh_tunnel.exe state --json | ConvertFrom-Json`.

Exit code: 0 — done, 1 — error, 3 — the app is not running.

On Windows the app is a window app, so in PowerShell the answer may be
printed after the `PS>` prompt. To wait for it —
`ssh_tunnel.exe state | Out-String`.

## AI (MCP)

1. In the app settings tick **“Control by AI (MCP)”**. While it is off the
   AI can do nothing, even if connected. The checkbox is per device.
2. Connect the app to the AI.

**Claude Code** — one command in a terminal:
`claude mcp add ssh_tunnel -- "C:\path\to\ssh_tunnel.exe" mcp`

**Claude Desktop** — Settings → Developer → “Edit Config”. In
`claude_desktop_config.json` inside `mcpServers` add:
`"ssh_tunnel": {"command": "C:\\path\\to\\ssh_tunnel.exe", "args": ["mcp"]}`
and restart Claude Desktop.

Now you can ask: “turn on bypass”, “let youtube.com go direct”, “what is
with the device network”.

Nothing is opened to the network: the AI starts the app itself and talks to
it directly, nobody can connect to it over the network.

Dangerous actions — turning the device network off, removing a server or an
app, stopping another program, replacing settings — are marked “ask the
human” for the AI. A good AI asks you first.

## Panel password (Linux)

The Linux panel opened to the local network (`-web-lan`) can ask for a password: App settings → Advanced → «Password sign-in», then «Set password». Until a password is set, it does not ask.

Forgot it — on the server as root: `ssh_tunnel_vpn_linux -reset-password`, then `systemctl restart ssh_tunnel` (the sign-in screen shows the exact command).

## Updates

App settings → «Check for updates» → «Update»: the program downloads the new version, checks the checksum, installs it and restarts. Only on the button.
