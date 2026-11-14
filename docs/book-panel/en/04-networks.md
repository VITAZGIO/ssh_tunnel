# Networks

The **Networks** section shows the device networks that the server knows about,
and the servers of those networks.

## What a network and a network key are

A **device network** is a set of devices that see each other by permanent
addresses. The devices are joined by a **network key**. The key is created when
the device network is switched on on the first device. The other devices get it
by export from the first. The program describes this in its own Book, in the chapter
"Device network".

While two devices have the same key, they are in the same network. A different
key means a different network, even if the addresses match.

## Network name

A network can be named. The name is shown on the network map and in the server
list. It is handy to name it after a place: "Home", "Office".

1. Press **Rename network**.
2. Enter the **network name** and save.

If the name is cleared, the network is called by its number again ("Network N").

## Delete a network

The **Delete network** button erases the network on this server. All its devices
on this server are disconnected.

**Important: a deleted network can come back.** The network is taken from its
key. As soon as any device connects to the server again with its key, the network
appears — empty and without a name. To keep it from coming back, first remove
this network's key in the program on each device, or delete those devices in the
**Devices** section. Then the network stays deleted until nobody connects with
that key.

An empty network with nobody in it is removed by itself 7 days after it became
empty. If a device appears in it again, the count starts over.

## New devices — after approval

The **New devices — after approval** switch turns on the check. A new device that
connects for the first time waits in the **Devices** section until it is
approved. Devices that are already known are not affected by the check.

The switch works only on this server. More in the chapter "Devices".

## What the network card shows

Each network in the list shows:

- **Created** — when the network appeared on the server;
- how many of its devices are **online** now, out of the total;
- its **ID** — a service number of the network;
- **Removed automatically** — if nobody is in the network, here it says when it
  will be removed.

If the network has no devices, the panel says that it is empty. Before you delete
an empty network, make sure that its key is no longer needed by anyone.

## Plugin data of a deleted network

If a network was deleted but its plugin data (folders, clipboard, backups)
remains, the network card says "Network deleted, data left". The server never
erases this data by itself. You can erase it with the **Delete data** button. This
cannot be undone. More in the chapter "Server plugins".
