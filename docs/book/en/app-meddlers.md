# Who touches traffic

Finds programs and settings that interfere with the VPN and helps check whether
they are the cause.

## What it looks for

- **Windows:** DPI bypasses that intercept packets (zapret, GoodbyeDPI —
  WinDivert), other VPNs (WireGuard, NetBird, OpenVPN, Radmin, Tailscale…),
  system proxy and PAC, manually set DNS, `hosts` entries, an antivirus that
  inspects HTTPS, foreign "whole internet" routes.
- **Linux:** other tunnels (wg, tun, tailscale…), DPI bypasses (nfqws, tpws),
  interception in iptables/nft, DNS in `resolv.conf`.

For each finding: what it is, how it interferes and what to do.

## How it checks

1. Under a finding: "Stop it for the check?" — **[Allow] [No]**. Without
   "Allow" the app touches nothing.
2. The app stops it and re-checks by itself: reconnects the tunnel and opens a
   test site (youtube.com by default, you can change it).
3. **Did not help** — it restores by itself: "It is not the cause, restored as
   it was".
4. **Helped** — it asks what next:
   - **Turn off for good** — only autostart is turned off, nothing is removed;
   - **Restore as it was**;
   - **Reconcile** (where possible) — e.g. add the VPN server address to
     zapret's exclusions so they do not get in each other's way.

Stopping a service or a DPI bypass needs administrator rights — Windows will
show its own prompt.

## What I turned off

Everything the app stopped or turned off is in the list at the bottom with a
**"Restore"** button. The list survives restarts and updates.

## If the VPN does not connect

Under the error on the main screen there is a **"Check who interferes"** button
— it opens this app.
