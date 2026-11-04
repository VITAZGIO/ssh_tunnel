# Common problems

## Does not connect

- Press **Check** on the main screen: it shows step by step where it breaks —
  network, server address, port, key.
- "The server did not accept the key" — the device may have been frozen or
  deleted in the server panel, or the wrong key is selected.
- "The server does not respond" — check the server address and port.

## Connected, but websites do not open

- Look at the log (the icon with lines at the top): it shows where each
  connection goes.
- Is the site in the "direct" list? Then it bypasses the server — and the ISP
  may block it.

## No internet after closing the app

The app removes the system proxy on exit. If it crashed, start it and quit via
the tray icon → "Quit".

## The window disappeared

The close button hides the window to the tray icon, the tunnel keeps working.
If the icon is not visible, press the "^" arrow next to the clock. To quit —
right-click the icon → "Quit". Separate windows (the Book, plugins) live on their
own and close with their own close button.

## A program says "non-public IP address"

Programs that check a site's address before connecting answer like this when
**"Fake addresses (fake-IP)"** is checked: a `198.18.x.x` address looks
internal to them. Uncheck it or add the site name to **"Always real address"**
(for example `.deepseek.com`). Excluding the program from the VPN does not
help: on Windows DNS is asked by the system, not by the program.

## The router, printer or NAS does not open

The local network goes directly by default. If "local network through the
tunnel" is on — turn it off.

## Still not working

Detailed help and where to report — on GitHub (link at the bottom of the
Book).
