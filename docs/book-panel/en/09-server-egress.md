# Server's own exit

Usually the server goes to the internet with its own address: it updates
packages, pulls container images, carries the traffic of tunnel users. In the
**Settings** section you can send all of the server's own traffic through another
of your servers. In this chapter that other server is called **B**.

**Warning.** Start only on a test server, and make sure you have console access
from the hoster: VNC or the web console in the customer account. If something
goes wrong and SSH is lost, the console is the only way to bring the server back.
Do not turn this on for a server you cannot recover without SSH.

## What changes and what does not

Only the **outgoing** traffic of the server itself changes. Incoming connections —
SSH to the server, this panel, tunnel users and the device network — do not change.
They answer by the same path they came by. The panel adds separate routing rules
for this.

## How to turn it on

1. Open server B's panel. Create an ordinary client: **Devices** → **+ Add device**.
2. Copy its `sshtunnel://` link. It is shown once, right after creation.
3. In this panel open **Settings** → **Server's exit → through server B**.
4. Paste the link into **Client link from server B's panel**.
5. Choose what to do if B is unavailable (see below).
6. Press **Turn on**.

The panel checks everything step by step: it installs the Linux client of
ssh_tunnel of the same version as the panel itself, checks that B does not go to
the internet through this same server, and applies the routes. The phases are
shown in the card: "checking and installing the client…", "applied, waiting for
confirmation", "working through server B".

## Confirmation within a minute

After turning it on you have **60 seconds**. Within that time you must confirm that
the panel still answers. Open the panel in a new tab or on your phone and press
**Confirm: the panel is reachable**.

If there is no confirmation, a systemd timer brings everything back as it was.
This timer does not depend on the panel or on the network, so the rollback works
after a reboot too, and if the panel crashed in the middle of applying.

So you do not lose access even if the rule turned out to be wrong.

## If server B is gone

In the card you choose what to do while B is unavailable:

- **No internet (default)** — the default. Incoming connections work. The server's
  own traffic goes nowhere: it has no internet until B comes back.
- **Direct exit** — the server's own traffic goes with its own address, around B.
  Incoming connections keep working.

The panel checks the SSH port of B itself. The phase **server B is unavailable**
means that it does not see B. In "direct" mode the rule for B is removed while B
stays silent.

## Turn off

The **Turn off** button brings everything back as it was: the server's traffic
goes direct again. The panel asks for confirmation.

## Limits

- The function has been checked on a test bench with a real Linux kernel, with
  different `rp_filter` values. It has not been checked yet on a live VPS: that is
  a manual check. Do not consider it checked until you have done it yourself.
