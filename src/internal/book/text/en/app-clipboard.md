# Shared clipboard

Copy on the PC — paste on the phone, and back. Text, images and files up to
**50 MB** (bigger — via Device files). It travels **only over the device
network**, encrypted like the network; turn on «Device network».

- **Taking part** — the switch on the app tile, separate on each device.
  Off — the device neither receives nor sends.
- **PC and Linux → others** — automatically: copy, and a second later the
  others have it. What arrives goes straight to the clipboard (a file — as a
  link to a copy in the temp folder).
- **Phone → others** — Share → «ssh_tunnel shared clipboard» (text,
  screenshot, file) or the **«Shared clipboard»** quick tile: it sends what
  is in the phone's clipboard now. Android does not let apps read the
  clipboard in the background.
- **To the phone** — automatically: it lands in the clipboard, just paste.
- **History** — the last 20 entries: copy text, download a file, delete.
  Files in history take up to 200 MB; older ones lose their content.
- No loops: what arrives is never sent on.

Linux: needs `wl-clipboard` (Wayland) or `xclip` (X11). Without them the
clipboard is not watched and entries only show up in history.
