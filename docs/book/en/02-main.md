# Main screen and modes

The main screen has **two independent buttons**. Each one changes only its
own thing.

- **The big round one** — bypass: websites and programs go through the
  server.
- **"Device network"** below it — your devices see each other by `.mesh`
  names. Shown only when the server has the device network enabled.

## Four modes

A mode is just a combination of the two buttons.

- **Off** — both buttons are off. The internet works as without the app.
- **Device network only** — websites go directly, your devices are reachable
  by `.mesh` (RDP, SSH, home server).
- **Bypass** — websites go through the server, no device network.
- **Bypass + device network** — both.

## Switching

- Pressing a button while "Connecting…" **cancels** the connection. The other
  button does not change.
- Switching needs no reboot: afterwards everything is as after a clean start
  in the new mode. Open browser tabs move to the new path by themselves.
- The app remembers what was on when it was closed and can restore it on
  start (App settings → Startup).

## Below the buttons

- **Server** — which one is selected; tap to change.
- **Speed, connections, channels** — what is going on right now.
- **Check** — tests the chain to the server step by step when something is
  wrong.

## Not everything goes through the server

In settings you can send some sites or programs **directly** (bypassing the
server) or, on the contrary, **always through the server**. The local network
(router, printer, NAS) goes directly by default.

## Exit through another server

With more than one server, each one's settings have "Exit through": you can
tell the app to connect to one server but go out to the internet only
through another (up to two in a row). Everything else stays the same — sites
and apps follow the same rules as without a chain.

The key point — **no fallback**. The last server in the chain is unreachable
— there is no internet at all until it comes back; traffic never takes a
detour through the server you connected to instead. That is deliberate: if
you picked a specific server to exit through, it is that one, not "whichever
works".

A line with the server path shows up under the main button. While everything
is fine it just shows the path; if the last server drops, it turns red and
says plainly that the internet is off because of the chain — so you are not
left looking for the cause somewhere else: the connection to the first
server and the device network keep working as usual.

## Site addresses (DNS)

In VPN mode the **server** resolves site names: your provider does not see
which sites you open and cannot slip in a block page. Programs get **real**
site addresses, so programs that check the address before connecting (AI
tools with `web_fetch`, NetBird, clients with address lists) work too.

Where a connection goes — through the server or directly — is decided by the
site name inside the connection itself, or, if it is not there, by recent DNS
answers.

The **"Fake addresses (fake-IP)"** checkbox (App settings) brings back the old
behaviour: every name gets a made-up `198.18.x.x` address. Names in the
**"Always real address"** list get the real address even then. Switching needs
no reboot: the app resets its tables, connections and the system DNS cache
itself.
