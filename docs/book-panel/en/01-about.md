# What the panel is

The panel is a web page for managing your server. It is installed on the server
itself, not on your computer. The program `ssh_tunnel_panel` runs on the server
and solves one task: who may connect to this server.

Without the panel, every new device would have to be set up by hand: create a
user, put a key in place, check the login. The panel does it with a button. For
each device it creates a separate system user and a separate key. If a device is
lost, you delete it in one click, and its key stops working.

## How the panel differs from the program on a device

The ssh_tunnel program on your computer or phone manages only its own
connection. The panel manages all connections to the server. The program on a
computer knows nothing about other devices; the panel sees all of them.

The panel runs as root: without that it cannot create system users. That is why
login is protected by a password, and you should not leave the panel open to the
internet without a domain and a certificate. More in the chapter "First login".

## Sections

The panel has five main sections, plus the "Plugins" section:

- **Overview** — the main screen: the state of the server and the network journal.
- **Devices** — everyone who may connect to the server. An SSH client and a
  network device that belong to the same person are shown in one row.
- **Access** — who may go where inside the device network.
- **Networks** — device networks, servers of the network, the network map.
- **Settings** — password, autostart, server capacity, updates and exit through
  another server.
- **Plugins** — what runs on the server itself: mirrored folders, clipboard,
  backups and the rest.

Each section has its own chapter. For the first step, see "First login".

## If the words are unclear

- **Device** — a computer, a phone or a server that has been given access.
- **Device network** — devices that see each other by permanent addresses. More
  in the chapter "Networks".
- **Network server** — a server through which devices keep their connection. A
  network can have several servers. More in the chapter "Network servers".

## Where to start

If the panel is new to you, read the chapters in order:

1. "First login" — change the password and check that the page opens.
2. "Devices" — add your first device and hand it its settings.
3. "Networks" and "Access" — if the devices must see each other.
4. "Network servers" — if you have more than one server.
5. "Update" — when a new version is out.

The other chapters are for when you need them. For example, "Plugins" when you
want to keep folders or backups on the server, and "No server" when the home
internet goes down.

## Where to find details

Many sections have a **?** sign next to the heading. It opens a short hint. This
Book goes into more detail than the hints. For devices there is a separate Book,
inside the program itself.
