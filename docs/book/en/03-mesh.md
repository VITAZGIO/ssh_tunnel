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

## Several servers

A network can have several servers and none of them is the main one. To make a
server part of the network, add its profile on the device with the same network
key and the device network on — the device then stays connected to all such
servers at once (to the extra ones with a light connection, no internet
traffic through them). If one server goes down, connections go through
another server both devices share; direct connections notice nothing.

- A device on two servers is listed once, with the same address and name on
  both.
- If there is no shared live server, the app says so: "no common server: it is
  connected via …".
- Access rules, the journal and removal are per server. If the servers have
  different rules, the panel says so: make them the same.

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

### On the phone

The phone behaves the same way: at home without internet it turns on (after
a reboot too), the device network works, and the notification says "No
server, direct: 1" or "No connection, no neighbours in sight". The phone
looks for the server by itself: every few seconds at first, then about once
every half a minute, and right away when the network changes or you turn the
screen on. When the internet is back the notification says "online, through
the server" — nothing to touch.

You don't have to turn mobile data off: the phone looks for neighbours in
Wi-Fi even when Android says "Wi-Fi has no internet access" and goes online
over mobile data.

**Screen off.** While neighbours are connected directly, the clipboard and
files keep working with the screen off. If the link to the PC was lost (the
PC rebooted, the phone left home), the phone keeps looking for it with the
screen off too, but in deep sleep (after half an hour to an hour of lying
still) Android wakes apps only now and then — the neighbour is found at one
of those wake-ups or as soon as you turn the screen on. So that Android does
not put the app to sleep entirely, allow it:

- **Battery → Unrestricted** (Settings → Apps → ssh_tunnel → Battery).
  Otherwise the system may stop it after a while.
- **Autostart** — on Tecno/Infinix (HiOS), Xiaomi (MIUI/HyperOS) and similar
  phones this is a separate switch in the "Phone Master" or the app
  settings. Without it the app does not come up by itself after a reboot.
- **Always-on VPN** (Settings → Network → VPN → ssh_tunnel → "Always-on
  VPN") — then the app turns on by itself after a reboot, even if the server
  is unreachable.
