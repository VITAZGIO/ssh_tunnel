# Leak check

Checks that your real address is not visible while the VPN is on.

## What it shows

- **Which address sites see** — country and ISP. It first asks your own point
  on the server panel, if there is none — a backup external service.
- **Whose DNS answers** — if site names are asked from your ISP, bypassing the
  tunnel, that is a DNS leak.
- **IPv6** — whether traffic goes over IPv6 around the tunnel.

The verdict is one line. If there is a leak that a setting can close, a
**"Fix"** button appears (for example, turn on the system proxy).

## WebRTC

A browser may reveal the address via WebRTC. **"Open in browser"** opens a
small check page on your server panel — it stores nothing and sends nothing.
Available if the server was issued by a panel.

## Silently, on every connection

While the plugin is on (the switch on its tile), the app checks for leaks after
every connection and writes to the log **only when it finds a leak**.
