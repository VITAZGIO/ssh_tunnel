# Network scanner

Who is on your local network right now — Wi-Fi or wired.

- «Scan» (runs by itself when opened): the app takes the subnet of the
  active Wi-Fi/Ethernet (VPN and the device network do not count) and asks
  every address — a few in parallel, without flooding. A large network —
  only your own /24.
- The list shows **address, MAC, name, vendor** (by MAC, from a database
  inside the app: ESP — Espressif, Tuya, Xiaomi…) and **response time**.
- The name comes from the router (DNS), mDNS (ESPHome, Apple, printers) or
  NetBIOS (Windows). No name — the vendor is shown.
- «private MAC» — an address the device made up itself (a phone with a
  private Wi-Fi address, some routers): such a MAC has no vendor.
- Only ports **22, 80, 443** are checked: SSH open — an «SSH» button (the
  SSH plugin, address filled in), web open — «Open» in the browser. There is
  no full port scan.
- **Nothing is remembered** — a fresh list every time.

On a phone Android does not give apps the neighbours' MAC: you see «—» and
no vendor. On a PC the scanner opens in a separate window.
