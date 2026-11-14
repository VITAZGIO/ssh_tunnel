# Machines

Readings of your servers and PCs on one page: CPU (with a 30-minute graph),
memory, disk, temperature, load, uptime.

- **SSH** (Linux): address, user, password — or the app key from the SSH plugin.
- **netdata**: http://server:19999. **Home Assistant**: URL, token, sensors.
- Tick **«Show my machines to all my devices»** on a home server — your phone
  sees them under «From the network».
- Optional: a **MAC** gives «Wake» (sent from here and from all your devices in
  the network), **«Shut down as»** gives «Shut down» (the machine runs
  ssh_tunnel with this plugin and allows it).
- **History on the server**: if the network server has the **«Machines:
  history»** plugin on, readings go there every 30 s too and the CPU graph
  shows a day — blue from the server, green the last minutes from this device.
  Only readings and machine names are sent. Other devices see it only with
  «Show my machines to all my devices»; untick **«Keep history on the network
  server»** to send nothing.
