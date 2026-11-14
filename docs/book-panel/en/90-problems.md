# If something does not work

Each entry: what you see → what to check. The source is in brackets.

## Panel

**The panel page does not open.** Without a domain the panel listens only on
`127.0.0.1:47823`. Connect through an SSH tunnel or put a proxy in front. Look at the
journal: `journalctl -u ssh_tunnel_panel -n 50`. (Installation, PANEL_SETUP.)

**You forgot the password.** Delete `/etc/ssh_tunnel_panel/users.json` and restart the
service. A new one-time password appears in the journal. (PANEL_SETUP.)

## Networks and devices

**You deleted a network, and it came back.** The network comes back as soon as any
device connects with its key. First remove this network's key in the program on
each device. (Decisions, wave 1; the delete confirmation in the panel.)

**A device is "waiting for approval".** Open **Devices** and press **Approve** or
**Reject**. Approval is switched on in **Networks** with a switch. (Panel, wave 5.)

**The network says "rules differ".** The access rules on the servers are different.
Set the same rules on all servers. (Panel, wave 4.)

**No shared server.** The program on the device says "no shared server: it is online
through …". Two devices have no shared live server. Check that each has a profile of
the same server. (The program's Book, device network.)

## Exit chain

**"Server profile needed".** The exit chain has a server that is not on the device. Add
its profile. Until the profile is there, there is no internet through the chain. (The
program's Book; manual checklist, wave 5.)

**No internet, though a chain is set.** One hop of the chain is unavailable. The tunnel
does not go around it, so there is no internet until the hop comes back or the chain is
changed. (Decisions, wave 3.)

**The server's exit rolled back by itself.** You did not confirm in the panel within 60
seconds. Press **Turn on** again and confirm at once. (PANEL_SETUP.)

**Server B is gone, and the server itself has no internet.** This is how the "No
internet (default)" mode works. If you need an exit with the server's own address,
choose **Direct exit**. (PANEL_SETUP.)

## Plugins

**"Server in the device network" is off.** Devices cannot reach the plugins. Switch the
device network on on this server. (Panel, wave 5.)

**"The plugin ports are not open".** Press **Open the plugin ports** in the card "What
blocks the plugins". Only the plugin ports are opened. (Panel.)

**"The block list did not update".** The server could not download the sources. Devices
get the previous list. Check the source addresses and the server's internet access.
(Panel, wave 5.)

**The shared clipboard keeps text in the open.** This is intended: entries are plain
text for 24 hours. Do not copy passwords into the clipboard. (Decisions, wave 3.)

**A deleted network left plugin data.** The card says "Network deleted, data left". The
data is not erased by itself. Delete it with the **Delete data** button. (Panel, wave 4.)

**A backup does not restore.** The program on the device restores, not the panel. Other
devices do not see the copies. (The program's Book, backups.)

## Other

**The journal is empty after a restart.** The journal is kept in the memory of the network
service. This is by design. (Decisions, wave 1.)

**A "through the server" Docker container does not answer on a port from the internet.**
A port forwarded on the router does not work through the tunnel. Choose "Direct". (The
program's Book, Docker.)

**Nginx Proxy Manager cannot see `.mesh` addresses.** There is no such path yet: only the
program on a device knows `.mesh` names. It needs separate work. (PANEL_SETUP, "Nginx
Proxy Manager and .mesh addresses".)

**The phone has one server.** Android still works with one server only. (Contract, wave 4.)
