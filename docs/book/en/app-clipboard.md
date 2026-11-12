# Shared clipboard

Copy on the PC — paste on the phone, and back. Text, images and files up to
**50 MB** (bigger — via Device files). It travels **only over the device
network**, encrypted like the network; turn on «Device network».

- **Taking part** — the switch on the plugin tile, separate on each device.
  Off — the device neither receives nor sends.
- **PC and Linux → others** — automatically: copy, and a second later the
  others have it. What arrives goes straight to the clipboard (a file — as a
  link to a copy in the temp folder).
- **Phone → others** — Share → «ssh_tunnel shared clipboard» (text,
  screenshot, file) or the **«Shared clipboard»** quick tile: it sends what
  is in the phone's clipboard now. Android does not let apps read the
  clipboard in the background.
  Two more ways: the **“Send clipboard”** button in the app's persistent
  notification (shown while the plugin is on) and, optionally, an
  accessibility service — then the clipboard is sent by itself after you tap
  “Copy”. Turn it on from the plugin page (step-by-step instructions there,
  including “Allow restricted settings” on Android 13+). The service only
  sees the label of the tapped button and does not read the screen.
- **Photos**: JPEG, PNG and GIF from the phone paste on the PC both as a
  picture (Paint, Word, a messenger) and as a file (Explorer). HEIC, WebP and
  other formats are converted to JPEG on the phone first. Very large images
  (over 40 megapixels) stay file-only on the PC.
- **To the phone** — automatically: it lands in the clipboard, just paste.
- **History** — the last 20 entries: copy text, download a file, delete.
  Files in history take up to 200 MB; older ones lose their content.
- No loops: what arrives is never sent on.

Linux: needs `wl-clipboard` (Wayland) or `xclip` (X11). Without them the
clipboard is not watched and entries only show up in history.
