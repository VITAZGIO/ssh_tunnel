# Device network

Your devices — desktop, laptop, phone, home server — see each other by
permanent addresses and names, like in NetBird or Tailscale. The middleman is
not someone else's cloud service but your own server, the same one the tunnel
goes through.

Example: a home server runs a website on port 8080. From a laptop in a cafe you
open `http://home-server.mesh:8080` and get there. No public IP or port
forwarding on the router is needed.

## How to enable

1. **On the server** — the device network service. The server setup wizard
   installs it (the "Also install the device network" checkbox), or the panel
   on the server.
2. **On the first device**: Settings → server settings → "Device network" →
   "Enable". The network key is created automatically. Any device name will
   do, it becomes the `.mesh` name.
3. **On the other devices** — export the server from the first device (the
   network key goes with it, to a phone — as a QR code).

## How to use

- The "Device network" button on the main screen turns only the device network
  on and off. The big button does not touch it: open RDP sessions stay up.
- The device list is on the main screen, scroll down: who is online, address,
  direct link or via the server.
- Devices connect **directly** when possible (faster), otherwise via the
  server.

## If the server is unreachable

Home internet is down or the server is off — devices that have already
connected directly keep working with each other: shared clipboard, mirrored
folders, files. The list shows it as "no server, direct". Restarted the app
without the server — devices in the same local network find each other on
their own within about half a minute. A device with no direct path cannot be
reached without the server, and the app says so right away.

This also works when there is no server from the very start: the PC
rebooted and the home internet is down. The "Device network" button turns
on, and the status line says "no server, direct: 2" (how many devices are
seen) or "no connection, no neighbours in sight". The round button turns
off with an error meanwhile: without the server there is no bypass. When
the internet is back, the device network connects to the server by itself;
turn the round button on again (or autostart will).

A device removed in the panel while you were without the server stops being
let in as soon as this device talks to the server again.
