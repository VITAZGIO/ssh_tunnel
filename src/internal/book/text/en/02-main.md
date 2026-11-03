# Main screen and modes

The main screen has **two independent buttons**. Each one changes only its
own thing.

- **The big round one** — bypass: websites and programs go through the
  server.
- **"Device network"** below it — your devices see each other by `.mesh`
  names. Shown only when the server has the device network enabled.

## Four modes

A mode is just a combination of the two buttons.

- **Off** — both buttons are off. The internet works as without the app.
- **Device network only** — websites go directly, your devices are reachable
  by `.mesh` (RDP, SSH, home server).
- **Bypass** — websites go through the server, no device network.
- **Bypass + device network** — both.

## Switching

- Pressing a button while "Connecting…" **cancels** the connection. The other
  button does not change.
- Switching needs no reboot: afterwards everything is as after a clean start
  in the new mode. Open browser tabs move to the new path by themselves.
- The app remembers what was on when it was closed and can restore it on
  start (App settings → Startup).

## Below the buttons

- **Server** — which one is selected; tap to change.
- **Speed, connections, channels** — what is going on right now.
- **Check** — tests the chain to the server step by step when something is
  wrong.

## Not everything goes through the server

In settings you can send some sites or programs **directly** (bypassing the
server) or, on the contrary, **always through the server**. The local network
(router, printer, NAS) goes directly by default.

## Site addresses (DNS)

In VPN mode the **server** resolves site names: your provider does not see
which sites you open and cannot slip in a block page. Programs get **real**
site addresses, so programs that check the address before connecting (AI
tools with `web_fetch`, NetBird, clients with address lists) work too.

Where a connection goes — through the server or directly — is decided by the
site name inside the connection itself, or, if it is not there, by recent DNS
answers.

The **"Fake addresses (fake-IP)"** checkbox (App settings) brings back the old
behaviour: every name gets a made-up `198.18.x.x` address. Names in the
**"Always real address"** list get the real address even then. Switching needs
no reboot: the app resets its tables, connections and the system DNS cache
itself.
