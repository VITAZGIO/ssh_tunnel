# Journal

The journal shows what happened in the device network: who connected and
disconnected, who was refused by access rules, what was renamed and deleted. It is
on the main screen **Overview**, in the card **Journal**.

## Filters

Above the journal there are three buttons:

- **All** — all events;
- **Connections** — who connected and disconnected;
- **Refusals** — refusals only.

The hint about the journal is behind the **?** sign next to the heading.

## What the entries mean

The entries are short. The most common ones:

- **"… connected"** and **"… disconnected"** — a device joined the network or left it;
- **"New device: …"** — a new device came for the first time;
- **"Refused: A → B, port"** — device A was not allowed to reach B on this port.
  The reason is the access rules;
- **"Device renamed"** and **"… removed from the network"** — name changes and
  removals;
- **"Network service restarted — journal started over"** — the network service
  restarted.

## Why the journal is empty

The journal is kept in memory by the network service, not on disk. So after the
service restarts, the journal starts over, and the numbering begins from one. The
lines already shown in the panel stay on the screen.

The journal is for watching current network work. It is not an audit: it does not
keep history of the past.

## If the journal is empty

An empty journal happens in two cases:

- **No events yet.** The network has not worked with this server since the service
  started. Wait until a device connects.
- **No such events yet.** There are events, but not of the type chosen by the
  filter. Switch the filter to **All**.

## Each server has its own journal

Each server of the network keeps its own journal. If a device connected through one
server, the second server's journal will not show it. To see everything, open the
journal on each server.

## If you see "Refused"

A refusal means that an access rule forbade the connection. This is not a fault.

1. Check which groups and rules the devices have in the **Access** section.
2. If the connection should be allowed, add a rule or a group tick. For simple mode,
   tick the right cell of the table.
3. A new rule applies to new connections. Reconnect a connection that is already
   running.

More about rules in the chapter "Access".
