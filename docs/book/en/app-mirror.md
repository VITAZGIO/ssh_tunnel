# Mirrored folders

The same folder on all your devices — for example Obsidian notes on a PC,
a laptop and a phone. An edit on any device goes to the folder's server
and from there to all the others. Works over the device network, **even
when the internet tunnel is off**.

## Folder server

Each folder has **one server** — the device where it is the "main" copy.
Best an always-on one (a home server, a computer that stays on).

1. On the server add the «Mirrored folders» app.
2. **«Sharing»** tab → full path to the folder → «Share».

Nested folders cannot be shared: one file — one server.

The "server" can also be your rented VPS (the same one the tunnel runs
through): in its panel, «Plugins» tab → «Mirrored folders» → «Add folder».
That way the folder stays in sync even when no two devices are ever online
together — the VPS is always reachable.

## Who sees a folder

Like Syncthing: **a new device of the network sees no folders until you
tick them for it.**

1. On the folder server — the **«Access»** tab: every device of the network,
   servers first, then the rest, alphabetically. Offline devices are listed
   too (greyed), so access can be given in advance. On the right — how many
   folders each one sees.
2. Tap a device → tick folders. A tick is saved at once; «All» and «None»
   do it in one go.
3. A new device is not listed yet — «Refresh».

On the «Sharing» tab each folder says which devices see it.

Untick — syncing of that folder stops on the device with «the owner closed
access to the folder», **its files stay where they are**. Tick again —
syncing continues from the same point; what is already there is not
downloaded again.

Access belongs to the device, not to its address or name: renamed or
readdressed — access stays. A device that was removed in the panel and
comes back with a new id is a new device.

**Upgrading cuts nobody off:** folders shared before this version are first
visible to everyone, as before. As soon as the network starts naming
devices (a new `meshd` on the network server), every device known at that
moment gets those folders; after that, only by ticks. A new folder is
visible to nobody until you tick it.

Folders on the rented server («Plugins» → «Mirrored folders») work the same:
in the panel under the folders, «Who sees the folders» → «Folders…» at a
device.

## Connect a folder on another device

1. Add the app. The **«Folders»** tab shows «Server found» — once the owner
   has ticked at least one folder for this device.
2. Pick **where to keep** the folder on this device and the mode.
3. «Connect».

Modes:

- **send and receive** (normal) — like Obsidian on every device;
- **receive only** — a copy of the server; your local edits are not sent
  and not overwritten (if the server changes the file, your edit stays next
  to it as a copy);
- **send only** — a backup, e.g. phone photos: deleting on the phone does
  **not** delete the file on the server.

You can connect a folder that already has files: identical ones are not
transferred, different ones become conflict copies — nothing is overwritten.

«Disconnect» stops syncing; the files on the device stay.

## Status

Each folder shows «in sync», «N files waiting» (e.g. the server is
unreachable — edits queue up and go out by themselves, even after a day
and a reboot), «conflict» or «the owner closed access to the folder».

## Conflicts

If a file was changed on two devices before they exchanged edits, **both
versions are kept**: the second one is placed next to it as
«file (конфликт, phone, 03.11.2026)». You get a notification, and the
**«Conflicts»** tab shows both versions side by side with buttons:

- **keep this** — the main file stays, the copy goes to the trash;
- **keep that** — the copy becomes the main file, the old one goes to versions;
- **keep both** — both stay, the conflict leaves the list.

Delete vs. edit: **the edit wins** — the file comes back.

## Trash and versions

Everything replaced or deleted is kept on the server for **30 days**.
«Trash & versions» tab → «Restore». A restore can be undone too: the
current version goes to versions itself.

## What is not synced

Always excluded: the Obsidian window layout (`.obsidian/workspace*.json`),
editor temp files, Syncthing conflicts (`*.sync-conflict-*`), `.DS_Store`,
`Thumbs.db`. Own exclusions — the settings button of a folder on the
«Sharing» tab (one pattern per line, e.g. `*.log`).

Empty folders and links are not transferred. Names that differ only in
letter case do not coexist on Windows and phones — such a file shows up as
«waiting».

## Phone

- To keep a folder in phone storage (e.g. an Obsidian vault), Android must
  give the app **«All files access»** — the «Allow» button in the app.
- There is no separate permanent notification: the VPN service runs it in
  the background.

## Moving from Syncthing

First **two weeks on an unimportant folder**. Do not remove Syncthing: move
to mirrored folders only a folder already taken off Syncthing — two programs
on one folder get in each other's way.
