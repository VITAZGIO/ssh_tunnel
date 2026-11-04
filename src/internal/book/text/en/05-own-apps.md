# Your own plugin

Besides the catalog plugins you can install your own — made by you or someone
else. It is a plain web page (HTML, JS, CSS) and an icon packed into a zip.
It cannot contain programs: the program refuses an archive with exe, dll and
the like.

## How to install

1. “Plugins” → “+” → **“From file”**, pick the archive.
2. The program shows what the plugin asks for: “see the devices of your network”,
   “change the rules” and so on. If you agree — “Install”.
3. A tile appears marked **“not from catalog”**.

The plugin runs in its own sandbox: it does not see the program window or its key,
and it talks to the program with its own key that lets it only where you
allowed. Without the “see the devices” permission it does not get the
device list, however hard it tries.

To remove — “…” on the tile → “Remove”. The plugin is removed completely,
together with its data.

From the command line: `ssh_tunnel.exe apps install app.zip` shows the
permissions, and with `--yes` installs.

## How to make one

The archive has `manifest.json`, a page and an icon. A `manifest.json`
example:
`{"id": "hello", "name": {"ru": "Привет", "en": "Hello"}, "icon": "icon.svg", "page": "index.html", "permissions": ["state"]}`

Permissions:

- `state` — see whether the tunnel and the device network are on;
- `control` — turn them on and off;
- `devices` — see the devices of your network;
- `rules` — change the rules (what goes via the server, what goes direct);
- `files` — files of other devices;
- `traffic` — traffic usage;
- `clipboard` — the shared clipboard;
- `calendar` — calendar;
- `sitecheck` — check sites.

The plugin key is the third part of its page address:
`const KEY = location.pathname.split('/')[3]`, a request —
`fetch('/api/state', {headers: {Authorization: 'Bearer ' + KEY}})`.
The plugin keeps its own data in the program: `/api/app/data`.

A ready sample and the full format description are on GitHub, in
`docs/custom-app-sample` and `docs/CUSTOM_APPS.md`.
