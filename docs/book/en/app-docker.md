# Docker

For each Docker container on this Linux machine, choose how it reaches the
internet. Works in the VPN version (`sudo ssh_tunnel_vpn_linux`).

## Three choices

- **Like the computer** — the default: the program leaves the container
  alone, it goes the same way as the machine itself.
- **Via the server** — only through the tunnel, strictly. Tunnel off,
  reconnecting, the seconds after "Turn off" or the program crashed — **the
  container has no internet**, it never goes around. The "always direct"
  list and per-program rules do not apply to it: they are for the machine
  itself. DNS too: the container's queries to any DNS server (the home router
  included) go into the tunnel, where the program's DNS answers them — always
  via the server, the ISP never sees them; without the tunnel names do not
  resolve.
- **Direct** — past the tunnel, through the home network, even when the
  machine itself goes via the server.

Whatever you choose, these keep working: the home network, the machine
itself, neighbouring containers, the device network and ports published to
the home network (`-p`).

**A port forwarded on the router to the internet** does not work for a "via
the server" container: the reply would go into the tunnel, not the way the
request came. The app page says so next to such a container. Pick "Direct"
or "Like the computer" for it.

## No VPN — no internet

A checkbox on a "via the server" container. Without it, a VPN you turned off
yourself (both buttons off, service stopped) puts the container back to
"like the computer". With it, the container has no internet then too: with
the VPN off and when the program is not running at all. The home network and
neighbouring containers stay reachable.

It is cleared only by the same checkbox (turning the app off does not clear
it). A recreated container gets a new address; while the program is not
running, the new address is not closed — start the program and the rule is
set.

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
removed and all containers go like the computer again (except those marked
"no VPN — no internet"). If the program crashed, the rules stay (containers "via the server" have no internet) until
the next start: the VPN comes up — they are set again; it does not come up
within 2 minutes — they are removed.

## If it does not work

- Docker with `firewall-backend: nftables` (new, opt-in): the program cannot
  ask it not to rewrite the container's address, and "via the server" gets
  less strict — the "always direct" list applies to the container again. The
  app page shows the error in the "Kernel rules" line.
- In custom Docker networks (not `bridge`) names are resolved by Docker's
  embedded DNS. If the machine's own DNS is only `127.0.0.53`
  (systemd-resolved), Docker asks it on behalf of the machine, and such a
  query goes like the machine's. Safer: give the container a DNS explicitly
  (`dns: [1.1.1.1]` in `compose.yaml`) — then the query goes via the server.

From the command line: `ssh_tunnel_linux docker` — the list,
`ssh_tunnel_linux docker set container=web route=tunnel` — a choice,
`ssh_tunnel_linux docker set container=web strict=true` — "no VPN — no
internet".
