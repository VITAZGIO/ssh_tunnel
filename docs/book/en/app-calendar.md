# Calendar

Nextcloud calendars right in the program — instead of DAVx5. The program
syncs with Nextcloud every 15 minutes and right after your edit. Use the
Nextcloud address on the device network: it works at home and away, and the
VPN service on the phone does not get put to sleep like DAVx5.

## Connect

1. In Nextcloud: avatar → Settings → Security → "Create new app password".
   Your normal password is not needed.
2. "More apps" → "+" → "Calendar" → "Add", then open it.
3. Address — e.g. `http://ubuntu.mesh:8080` (the home server on the device
   network), login and app password → "Connect". The password is stored
   encrypted (on Windows — by Windows itself), never in the settings.
4. On the phone the program asks for calendar access: events appear in the
   system calendar and in any calendar app.

The "Contacts too" box copies Nextcloud contacts to the phone. Off by
default.

## What it does

- Views: month, week, upcoming list. The gear — connection and which
  calendars to show.
- "+" — new event; tap an event to edit or delete it (delete asks first). A
  recurring event is edited and deleted as a whole series.
- If someone changed the event elsewhere at the same time (Nextcloud, Home
  Assistant), the server rejects the edit — the program shows the server's
  version and says so at the top. Repeat the edit on the fresh version.
- In the phone's system calendar events are read-only: edit them in the
  program (the edit then goes straight to the server).

## Widget

- **Phone:** long-press the home screen → Widgets → ssh_tunnel →
  "ssh_tunnel calendar". Shows this week's upcoming events; tap to open.
- **PC:** gear → "Desktop widget". A small borderless window sits on the
  wallpaper below other windows, without a taskbar button. Drag it by the
  header; it remembers its place.

## If it does not sync

- The error is shown at the top. "Did not accept the login or password" —
  the app password was revoked or mistyped.
- "Device network is off" — `.mesh` addresses need the device network
  (the small button on the main screen).
- Phone: Settings → Accounts → "ssh_tunnel calendar" → sync must be on.
  Battery saving for ssh_tunnel — "unrestricted".
