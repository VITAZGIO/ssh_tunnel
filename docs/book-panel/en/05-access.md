# Access

The **Access** section decides who may go where inside the device network. For
example, a laptop may open a website on the home server, and a phone may not.

## The main rule: rights belong to the SSH user, not the device

Rights belong to the **SSH user**, not to the device. A device logs in to the
server under its own user. The rules you set apply to that user. So the rights
cannot be faked by logging in from another device under someone else's user.

A simple rule follows from this: if two devices came in under the same user, they
have the same rights.

## Two modes

The section has two modes. The switch is **Turn on simple mode**.

### Simple mode

Simple mode shows a table with ticks. Each device is given a role:

- **My device** — goes to everyone except guests;
- **Server** — others go to it, it goes nowhere;
- **Guest** — internet only: the network does not see it, nobody goes to it.

Each role has a column **May connect to**: **To all** or **Nowhere**, and ticks
per device. Simple mode writes everything as rules by itself. It does not touch
the rules you set by hand in advanced mode: such ticks are shown as set by the
advanced mode rules.

### Advanced: groups and rules

In advanced mode there are two lists:

- **groups** — you put devices into groups (the **Rights** button on a device);
- **rules** — "from → to", with ports. Empty means all ports. You can write a
  list: `22,80,8000-8100`.

There are two special groups: **"All"** — all devices and servers, and
**"Servers"** — the network servers themselves. A rule "A → B" allows opening
connections from A to B. The other way round needs a rule in that direction too.

The limits are: no more than 64 groups and 256 rules. If saving would go over
them, the panel writes nothing and says how many are needed and how many are
allowed.

## How changes apply

New rules apply to new connections. A connection that is already running is not
cut by a rule. To end it, reconnect the device.

Rules are stored on each server separately. If the rules differ between two
servers, the panel writes "rules differ". Set the same rules on all servers.

## Without a server

Only the server checks the rules. Without a server nobody checks the rules: a
device is let in only from the last list that the server managed to pass on.
More in the chapter "No server".
