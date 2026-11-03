# Data usage

How much went out and came in through the app — by day and by program.
Runs in the background while the switch on its tile is on.

- **Three colours** on the chart: blue — via VPN (the server), green —
  direct, yellow — the device network. Period — day, week or month.
- **Apps** — who used how much; the bar under the name is split by the same
  colours. The sum over apps equals the sum over paths.
- On the phone you see apps (icons and names) and can pick the network:
  all, Wi-Fi or mobile.
- Only what goes through ssh_tunnel is counted. Apps excluded on the phone
  bypass it and are not counted; other devices' traffic is not shown.
- Daily totals are kept for **3 months**, older ones are deleted
  automatically.

## Limit

The «Warn about usage» box (off by default): how many gigabytes per month
or per day and what to count — all traffic, VPN only or (on the phone)
mobile data only. At **80%** and **100%** you get a warning — in the log on
a PC, as a notification on the phone. The VPN is **not** turned off.

On a PC the app opens in a separate window.
