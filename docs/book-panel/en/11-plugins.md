# Server plugins

The **Plugins** section holds the modules that run directly on the server. They
are the same as in the program on a device, but they live on the server. They are
served by a separate process, `ssh_tunnel_node`. It runs without root rights and
keeps its data in its own folder.

Plugins are switched on with ticks, after the **plugin host** is installed (the
**Install plugin host** button). The **Remove** button removes the host. The data
stays, unless the **Also delete plugin data (folders, clipboard)** tick is set.

The host works only when the device network is switched on on this server. While
there is no network, or its service is too old, the plugins admit nobody. The
host card says so.

## Plugins

**Mirrored folders.** A network folder that is the same on several devices. The
panel shows each folder, its quota, used space and conflicts. You can change the
quota right in the folder's row. Add a folder with **+ Add folder**. Who sees a
folder is set separately (see below).

**Files of the device (shared folder of the network).** A shared folder where
files arrive. Its space also counts in the common quota.

**Shared clipboard.** Entries that devices passed to each other through the
clipboard. The server keeps 20 entries for 24 hours. **Entries are stored as plain
text.** Do not copy passwords there. The **Clear clipboard** button erases the
clipboard history of the network. It cannot be undone.

**Backups.** Copies of device folders. More in the chapter "Backups".

**Machines: history.** Devices with the "Machines" plugin send the readings of
their machines every 30 seconds: processor, memory, disk, temperature. The server
does not read the machines itself, because it cannot call devices first. The
retention is set by the field **Keep machine history (days)**, 30 days by default.
The **Erase history** button erases it. Other devices see the history only if the
sending device allowed showing its machines to everyone.

**Ad blocking: shared list.** One list for the server: sources (links to hosts or
adblock lists, one per line), its own entries **Always block (own names)** and
**Never block (with subdomains)**. Sources are downloaded once a day, or by the
**Update now** button. If none was downloaded, the previous list is handed out. The
device still blocks by its own DNS. The server only keeps the list and hands it
out.

**Check a site from the server.** The same steps as the plugin on a device: DNS,
connection, secure connection, the site's answer. Only from the server's network.
Enter the address and press **Check**. This is not a proxy: only names on the
internet, on port 443, one request "/" without a body. Local and private addresses
are not checked. From one device no more than 5 checks in a row, then one every 10
seconds.

## Quotas

The quota is shared by all plugins of the network and by all networks together. If
space runs out, new data is refused with a clear error on the device. What is
already written is not damaged. Old backup versions may be removed, see the chapter
"Backups".

## What blocks the plugins

The card **What blocks the plugins** checks the settings by itself and writes the
reason. Common reasons:

- **"Server in the device network" is off.** Devices cannot reach the plugins.
- **The server's ports have no plugin ports.** Press **Open the plugin ports**. The
  panel opens only the plugin ports, nothing else.
- **The device network is not switched on on the server.** The plugins have nobody
  to serve.
- **No rule "to @servers".** If access rules are on, and some devices have no rule
  to the servers, the panel names them. The rule is set in **Access**. The panel
  does not change the rights itself.
- **This is a secondary server.** The plugins here cannot tell which device came.
  Put the plugins on the main server of the network.

Plugin ports: 47835 — files, 47836 — folders, 47837 — clipboard, 47838 — backups,
47844 — machines, 47845 — ad blocking, 47846 — site check.

## Who sees the folders

A new folder has an empty list: no device sees it until it is ticked. The **Who
sees the folders** section shows a tick for each device. A tick is saved at once.
The **All** and **None** buttons tick everyone or no one at once.

Folders created before this version are visible to all devices until the devices
of the network are known.

## Data of a deleted network

If a network was deleted but its data remains, the card says "Network deleted, data
left". The data is never erased by itself. The **Delete data** button erases
folders, files, the clipboard and the backups. This cannot be undone.
