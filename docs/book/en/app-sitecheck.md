# Check a site

Type an address and press "Check". The app goes two ways at once — **directly**
and **via VPN** — and shows the steps of each:

1. **DNS** — whether the name was found and which addresses.
2. **Connection** — whether the site's server answered.
3. **Secure connection** — whether the TLS handshake went through.
4. **Site reply** — what the site returned.

A green step passed, a red one is where it broke; time and details are next to
it.

## What the verdict means

- **"Your ISP blocks it, it opens via VPN"** — the direct way is blocked, the
  server way works.
- **"Your ISP spoofs DNS"** — the ISP answers the site name with its own stub
  address.
- **"Cuts the secure connection"** — this is how DPI works: the connection is
  cut at the handshake.
- **"Shows its block page"** — a block page instead of the site.
- **"The site does not let the server's address in"** — opens directly, not
  via VPN.
- **"It seems to be down itself"** — opens neither way.

## From the server

If the network server panel has the **"Check a site from the server"**
plugin on, a third column **"From the server «name»"** appears next to
"Direct" and "Via VPN": the same steps from the server network, with a
one-line verdict. The server checks only internet sites on port 443 — not
local addresses — and at most 5 times in a row per device.

## The rule button

After the verdict a **"Send via VPN"** or **"Send directly"** button appears —
it adds the site to "always through the server" or "always direct" in the
server settings right away. You can remove the rule there.

## Popular and history

- **"Check popular"** — YouTube, Telegram, Discord and others at once. Edit the
  list with "Edit list".
- At the bottom — the **last 10 checks**, tap one to see the details.
