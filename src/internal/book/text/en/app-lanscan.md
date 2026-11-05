# Network scanner

Three tabs: **Devices** (who is on your network), **Wi-Fi networks** (all
networks around and channel load) and **Signal** (your Wi-Fi level as you
walk around).

## Devices

- «Scan» (runs by itself when opened): takes the subnet of the active
  Wi-Fi/Ethernet and probes every address.
- The list shows **address, MAC, name, vendor** and **response time**.
- Known ports are checked (SSH, DNS, web, SMB, RDP, cameras, printers, MQTT,
  NAS, Proxmox, Home Assistant): they give the **device type**, and «Open»
  goes straight to its panel. SSH open — the «Terminal» button.
- **Nothing is remembered.**

On a phone Android does not give apps the MAC addresses of other devices
(since Android 10) — the scanner on a computer shows them.

## Wi-Fi networks

- The band (2.4 / 5 / 6 GHz) and a **channel graph**: each network is a hill
  as wide as its channel and as high as its signal.
- **«Least busy»** — the non-overlapping channel (1, 6, 11 for 2.4 GHz) with
  the fewest neighbours weighted by strength. Change it in the router.
- The list: level (dBm), channel, width, standard, security, BSSID.

On a phone precise location permission and location turned on are needed.
Android allows about 4 scans per 2 minutes. On Windows networks come from
netsh, on Linux from nmcli. Windows 11 lists networks only for apps with
location access — «Open settings» leads there.

## Signal

Your network level every second: dBm and a grade, a scale, **min / avg /
max** and a 3-minute graph. Walk around: stand in a room, type «kitchen» —
«Mark»; the 5-second average is saved. −50 and above is excellent, down to
−67 is fine for video and calls, below −75 expect drops.
