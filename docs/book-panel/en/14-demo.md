# Demo mode

Demo mode lets you look at the panel without a real server. It is handy for seeing
the sections, buttons and texts without risking anything.

## How to start it

Start the panel program with the `-demo` flag and the address it should listen on:

```bash
ssh_tunnel_panel -demo -listen 127.0.0.1:8080
```

Then open the address `http://127.0.0.1:8080` in the browser.

Demo mode works only on `127.0.0.1`. The `-domain` flag does not start with it.

## What you see

A line **Demo mode** appears at the top of the page. It reminds you that the data is
made up. Devices, networks and the journal are made up too.

## What is not right

- **The data is made up and lives only in memory.** Nothing is written to disk or to
  a real server.
- **Answers may differ from real ones.** The demo repeats the page, but not all its
  answers match the real ones. Do not rely on the numbers from demo mode.
- **You cannot test how the network works.** Demo mode has no real devices,
  connections or rules.

Demo mode is there to let you see the panel. To set up a real server, use the real
panel, as described in the other chapters.

## If the page does not open

1. The address in the browser must match what you gave to the `-listen` flag.
2. If the port is taken by another program, choose another port and start again.
3. Do not open demo mode on a server reachable from the internet. It listens only on
   `127.0.0.1`, and that is how it should be.

## What to look at

A convenient order to walk through:

1. **Overview** — the state and the journal. In the demo the journal is full of made
   up events.
2. **Devices** — the list and a device card: the buttons "Freeze", "Disconnect",
   "Delete", "Approve", "Freeze in network".
3. **Access** — the simple mode ticks and the advanced mode with groups.
4. **Networks** — the names of the networks and the network servers.
5. **Plugins** — the host card and the plugins. The buttons in the demo do not change
   the real server.

Press the buttons to see what would happen. Nothing real happens. When you have
finished, stop the program.

## What the demo is for

The demo is for learning the layout: where a setting lives, what a button is
called, what a message looks like. It is not for testing a real setup. Before you
change a real server, read the chapter about that section in this Book and do the
step on a test server first.
