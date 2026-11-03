# SSH

Your own list of servers on this device, with key sign-in.

## Add a server

«Add server» → name, address, login, port (22). The address is picked from
your device network (e.g. your home server, even when you are away) or
«Enter manually» — a name or IP.

## Key

The app creates an ed25519 key on first use. The private part is stored
**only encrypted**: on Windows with your account key (DPAPI), on Android
with an Android Keystore key. On Linux — a file with mode 600 in the
settings folder.

**«Install key»** — the app asks for the server password once, appends its
key to `~/.ssh/authorized_keys` and checks that the key works. The password
is not stored anywhere. The «Key» button shows the public part — you can
also add it on the server yourself. Installing works for Linux/Unix servers.

## Open

- **PC (Windows):** Windows Terminal (if installed) or PowerShell opens with
  the sign-in already started. The key is never written to disk in the
  clear: the terminal gets it from the app's own SSH agent, which lives for
  two minutes and closes.
- **Linux:** a desktop terminal; no desktop — the built-in one.
- **Phone:** the built-in terminal with a key row: Esc, Tab, Ctrl (tap it —
  the next letter goes with Ctrl), arrows, ^C, ^D, `|`, `~`.

## Server key

On the first sign-in the app remembers the server's key. If it changes
later, sign-in is refused: the server was reinstalled or someone is
impersonating it. If you know it was reinstalled — «Forget server key» in
the error message.
