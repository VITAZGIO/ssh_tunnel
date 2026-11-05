# Remote desktop

A Windows desktop over RDP from a PC or a phone, without port forwarding.

## RDP relay — install once

RDP itself is handled by a **relay** — guacd (Apache Guacamole) on a computer
in the **same local network as the machines**:

    docker run -d --name guacd --restart unless-stopped -p 4822:4822 guacamole/guacd

Enter its address at the top of the plugin: name.mesh:4822 via the device
network (works from anywhere) or 192.168.1.x:4822 locally.

## Machines and session

Add a machine by its home network address. The login can be remembered on
this device. In a session: on-screen keyboard (button toggles it), LMB hold / RMB, 🖼 paste a phone photo into the PC (needs Phone link there), touchpad/touch
modes, sticky Ctrl/Alt/Shift/Win, Ctrl+Alt+Del, Alt+Tab, quality, full screen.
The resolution follows the window; a dropped connection reconnects itself.
