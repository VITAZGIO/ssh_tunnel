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

## Connect a folder on another device

1. Add the app. The **«Folders»** tab shows «Server found».
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
and a reboot) or «conflict».

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
