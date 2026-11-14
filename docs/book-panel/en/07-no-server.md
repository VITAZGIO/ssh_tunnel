# No server

The device network can work even when there is no server. For example, the home
internet is down, or the server is off. But not everything works. This chapter
explains what happens.

## What keeps working

Devices that have already connected directly keep working with each other. The
shared clipboard, mirrored folders and files go without a server. In the program
such a link is marked as "without server, direct".

This also works when there has never been a server: the computer rebooted and
there is no internet at home. The device network switches on, and the status line
says "without server, direct: 2" (how many devices are visible).

A program restarted without a link to the server finds its neighbours on the same
local network in about half a minute. The network finds them by itself.

## What does not work

- **Access rules are not checked by anyone.** There is no server, so nobody
  decides "let in or not". Devices let each other in from the last known rules.
- **A removed device is not forgotten at once.** If you removed a device in the
  panel while there was no server, it stops being let in only when the device
  next contacts the server.
- **A device without a direct path cannot be reached.** A connection to a device
  that has no direct path is possible only through a server. The program says so
  right away.
- **There is no way around blocks.** The round button of the program turns off
  with an error when there is no server. Internet does not go through a server.

## When the internet comes back

The device network connects to the server by itself as soon as the server is
reachable. Switch the round button of the program on again if it did not come on
by itself. Autostart may do this for you.

## On a phone

A phone behaves the same way. At home without internet the device network works,
and the notification says "Without server, direct: 1". The phone looks for the
server by itself. You can leave mobile data on: the phone looks for neighbours
over Wi-Fi.

If the screen is off and Android has put the program to sleep, it will find a
neighbour at its next wake-up, or when you turn the screen on. For battery and
autostart in detail, see the Book of the program.

## What to do in advance

A network without a server works only for the devices that have already connected
directly. So prepare it before the internet goes down:

1. Make sure the devices see each other while the server is there. Without a
   server such a neighbour is marked in the list as "without server, direct".
2. Turn autostart on in the program, so the network starts after a reboot even
   without a server.
3. Do not delete devices in the panel while you are away without a server. The
   deletion reaches the device only at its next contact with the server.

If there is no such prepared path, a device without a server cannot be found, and
the program will say so directly.
