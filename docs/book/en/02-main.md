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
