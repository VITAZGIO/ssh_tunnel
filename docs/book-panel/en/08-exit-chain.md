# Exit chain

Usually a device goes to the internet through one server. An **exit chain** lets
traffic pass through several servers in turn: first the first one, then the
second. The last server of the chain is what the internet sees as the exit.

For example: laptop → Frankfurt → London → internet. Websites see London's address.

A chain can include no more than two extra servers after the first one.

## Set the chain from the panel

The chain is set in the device card, in the block **Exit to the internet
(chain)**.

1. Open **Devices** and select the device.
2. Choose the servers of the chain in order: first the entry, then the others.
3. Press **Send to device**.

The panel saves the chain, and the device accepts it. In the program on the device,
under the chain, it says "set in the server panel …" and the time.

## What the device does

The device applies the chain at once, as if it had set it itself. The tunnel
reconnects through the chain. The external address becomes the address of the
last server.

The chain can also be changed on the device itself. Then **the newer edit wins**.
If you changed the chain on the device after the panel, the device keeps its own
value. The panel sees it and shows that the chain is set on the device.

## If a hop breaks

If one of the servers of the chain is unavailable, the tunnel does **not** go
around it. Instead there is no internet at all. This is deliberate: you set the
chain, and traffic must not quietly leave it for the entry server or for a direct
connection. Websites will not open until the hop comes back or the chain is
changed.

## Server profile needed

If the chain has a server whose profile is not on the device, the tunnel does not
start. The program writes "server profile needed …". Internet through this chain
will not go until you add that server's profile to the device.

## Check that the chain works

1. Open the exit card on the device. It says which chain is in use now ("On the
   device: …") and what the exit without a chain is.
2. Open a site that shows your external address, for example 2ip.ru.
3. Compare the address with the last server of the chain. If they match, the chain
   works.
4. To check a failure, stop SSH on the second server of the chain. Websites should
   stop opening. Then start SSH again — the internet comes back by itself.

## Limits

- If a device has no profile for some hop, the chain is kept as "profile needed",
  and the tunnel stays down until the profile is added.
- On Android the panel does not yet show the chain or its state.
