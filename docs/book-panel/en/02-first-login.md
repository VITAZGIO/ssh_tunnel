# First login

When it starts for the first time, the panel creates the account `admin` and a
one-time password. The password is kept nowhere except the service journal. You
can see it with this command:

```bash
journalctl -u ssh_tunnel_panel -n 50
```

Right after you log in, the panel asks you to change the password. Make it long
and unique. The panel slows down password guessing by itself: after failed
logins the delay before the answer grows, and after several failures login is
blocked. This is not a replacement for a proper password.

## Change the password

1. Open **Settings** → the **Account** block.
2. Press **Change password**.
3. Enter the **Current password**, then the **New password**, and repeat it.
4. Press **Set password**.

If the one-time password is lost and you can no longer log in, reset the
account. Delete the file `/etc/ssh_tunnel_panel/users.json` and restart the
service:

```bash
sudo rm /etc/ssh_tunnel_panel/users.json
sudo systemctl restart ssh_tunnel_panel
```

The panel creates a new one-time account, as on the first start. The list of
devices is not touched: it is stored separately.

## Domain and certificate

The panel itself has no field for a domain or a certificate. They are set during
installation with flags. There are two options, switched by flags without a
reinstall:

1. **Behind your own reverse proxy** (nginx or Caddy). The panel listens only on
   `127.0.0.1:47823`, and the proxy provides the certificate. The proxy must pass
   the `X-Forwarded-Proto` header: the panel uses it to set the login cookie with
   the `Secure` flag.
2. **Built-in certificate.** Install with the flag `--domain=panel.example.com`.
   The panel gets a Let's Encrypt certificate by itself and listens on ports 443
   and 80. Before the start, the domain must point to the server with an A
   record. Otherwise the certificate will not be issued.

Do not keep the panel on plain HTTP on a port open to the internet.

## If the page does not open

Check in this order:

1. **Is the service running?** Look at the journal with the command from the
   start of this chapter. Start-up errors are written there.
2. **Are you opening the right address?** Without `--domain` the panel listens
   only on `127.0.0.1:47823`. It is not visible from outside. Connect through an
   SSH tunnel or put a proxy in front of it.
3. **Is the port open?** A firewall in front of the panel (ufw and similar) must
   let the panel's port through. With `--domain` these are 80 and 443.
4. **Does the domain point to the server?** If it does not, the certificate
   cannot be obtained, and the HTTPS page will not open.

## Autostart

**Settings** → **Autostart** → tick **Start on system boot**. If you untick it,
the panel will not start by itself after the server reboots. It does not stop the
panel that is already running.
