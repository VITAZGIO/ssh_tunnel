# Update

Three things are updated: the network service `meshd` on each server, the plugin
host and the panel itself. The program on the devices is updated last.

## Order

Servers first, then devices:

1. **`meshd` on each server.** The **Update meshd** button in **Settings**. Version 5
   is needed for approval of new devices and for freezing in the network.
2. **Plugin host.** If the plugins are installed, press **Update** in the host card.
3. **The program on the devices.** Update it after the servers.

If there are several servers, update them one by one.

Why `meshd` comes first: approval of new devices and freezing in the network work
only with version 5 of the service. An old service does not understand them.

## The panel

**Settings** → block **Panel version** → **Check for updates**. If a new version is
out, the button **Update to …** appears.

The panel downloads its own file from the release on GitHub. It checks the file
against the checksum, replaces the file and restarts. It takes about a minute. The
page refreshes itself, and you do not need to log in again. Connected devices are
not affected.

If the server has no internet access, the update check does not work. The panel
writes that it cannot reach GitHub.

## Versions before 1.7.3

Old versions of the panel have no update button. They are updated once with a
command run as root. The command is written in the panel installation instructions
in the repository on GitHub. After that you can update from the settings.

## Reset the password

If the password is lost, reset the account. Delete the file `users.json` and restart
the service. More in the chapter "First login".

## Change the panel port

The panel port is set with the `-listen` flag. The default is `127.0.0.1:47823`.
To change the port, edit the `ExecStart` line in the service and restart it:

```bash
sudo sed -i 's|^ExecStart=.*|ExecStart=/usr/local/bin/ssh_tunnel_panel -listen 127.0.0.1:8443|' \
  /etc/systemd/system/ssh_tunnel_panel.service
sudo systemctl daemon-reload
sudo systemctl restart ssh_tunnel_panel
```

With the `-domain` flag the port does not matter: the panel listens on `:80` and
`:443`.

## Check after an update

Do this after each update:

1. **The plugin host is running.** If the plugins are installed, check the service
   with `systemctl status ssh_tunnel_node`. If the service does not start, send the
   output of `journalctl -u ssh_tunnel_node -n 50`.
2. **The plugin ports are open.** Open "What blocks the plugins" and press **Open the
   plugin ports** if it says something.
3. **The devices see each other.** Open **Networks** and check that each device is
   online.
4. **The programs on the devices are updated.** They are updated after the servers,
   see "Order" above.
