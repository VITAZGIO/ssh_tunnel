# Docker

For each Docker container on this Linux machine, choose how it reaches the
internet. Works in the VPN version (`sudo ssh_tunnel_vpn_linux`).

## Three choices

- **Like the computer** — the default: the program leaves the container
  alone, it goes the same way as the machine itself.
- **Via the server** — only through the tunnel. Tunnel off, reconnecting or
  the program crashed — **the container has no internet**, it never goes
  around. DNS too: the container's queries to any DNS server (the home router
  included) go into the tunnel, where the program's DNS answers them; without
  the tunnel names do not resolve.
- **Direct** — past the tunnel, through the home network, even when the
  machine itself goes via the server.

Whatever you choose, these keep working: the home network, the machine
itself, neighbouring containers, the device network and ports published to
the home network (`-p`).

## Compose projects

A whole project can be switched at once — with the buttons in "Projects".
Choices of single services of that project are cleared. A choice is stored by
project and service (others — by container name), so it survives
`docker compose up --force-recreate`, a new image and a new address.

## Label

The container label `ssh_tunnel.route=tunnel` (or `direct`, `host`) wins over
the choice in the program — handy to keep right in `compose.yaml`:

```yaml
services:
  qbittorrent:
    labels:
      ssh_tunnel.route: tunnel
```

## What cannot be separated

- `network_mode: host` — the container is in the machine's own network and
  goes like it.
- `network_mode: "service:…"` / `container:…` — in another container's
  network, goes the same way as that one.

## Turning off

Turn the VPN off, stop the service or turn the app off — the rules are
removed and all containers go like the computer again. If the program
crashed, the rules stay (containers "via the server" have no internet) until
the next start: the VPN comes up — they are set again; it does not come up
within 2 minutes — they are removed.

From the command line: `ssh_tunnel_linux docker` — the list,
`ssh_tunnel_linux docker set container=web route=tunnel` — a choice.
