# Docker containers

The server panel does not manage Docker. The choice of how a container goes to
the internet is made in the program on the Linux machine where Docker runs. This
chapter explains what it means, so that you understand it when such a machine is
on your server.

The detailed instruction is in the Book of the program, chapter "Docker". This
text only explains the limits.

## Three choices

Each container has three positions:

- **Like the computer** — the default. The program does not touch the container:
  it goes the same way as the machine itself.
- **Through the server** — only through the tunnel, strictly. If the tunnel is off,
  reconnecting, or the program has crashed, the container has **no internet**. It
  cannot go around the tunnel.
- **Direct** — around the tunnel, through the home network, even when the machine
  itself goes through the server.

Neighbouring containers, the home network and the device network keep working as
before with any choice.

## Strict "through the server" mode

In the "through the server" mode the list "always direct" does not apply to the
container. DNS requests of the container go into the tunnel, and the program's DNS
answers them. So the provider does not see them. Without the tunnel, names are not
resolved.

## Without VPN — no internet

The tick **without VPN — no internet** is available on a "through the server"
container. Without it, a turned-off VPN returns the container to the "like the
computer" position. With it, the container stays without internet, even when the
VPN is off or the program is not running at all. The home network and neighbouring
containers remain reachable.

The tick can be removed only with the same tick. Turning off the plugin does not
remove it.

## A published port does not work

A port forwarded on the router to the internet does **not work** for a "through
the server" container. The reply would go through the tunnel instead of the way the
request came. For such a container choose "Direct" or "Like the computer".

## Docker 28 and newer

Docker 28 and newer drops the container's packets before the program can handle
them. So the strict mode on such a Docker needed a separate chain of rules. It is
in the program, and a test on the bench confirmed it. On a live machine with a new
Docker it is a manual check.

## Limits

- **Start window.** Until the program has set a rule for a new container, there is
  up to one second when the container has no rule of its own. This is accepted as
  a limit.
- **nftables firewall backend.** If Docker uses nftables (it is switched on by
  hand), the program cannot stop Docker from rewriting the container's address. The
  strictness of "through the server" then weakens, and the "always direct" list
  applies to the container again. The plugin page shows an error in the line "Rules
  in the kernel".
- **Strict mode on a real kernel** is checked on a test bench, but on a live Linux
  machine with Docker it is not confirmed yet.
