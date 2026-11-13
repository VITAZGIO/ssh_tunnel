# Backups

A folder of this device goes to the **network server** (your rented server)
on a schedule — with versions. From any version you can bring back one file
or the whole folder. Backups go **over the device network only**, not
through the internet tunnel.

## What the server needs

In the server panel: «Plugins» tab → the plugin host is installed → tick
**«Backups»** → «Apply». If there is a «What blocks the plugins» card, fix
what it says first (usually one «Open the plugin ports» button).

## A new backup

1. Add the «Backups» app.
2. **«My backups»** tab → the full path to the folder (or «Browse») and a name.
3. **When:**
   - **daily** or **weekly** — counted from the last successful backup;
   - **on change** — when something changed in the folder and then it was
     quiet for the given number of minutes (every new edit postpones it).
4. **Versions to keep** — the server removes older ones by itself.
5. **Server** — usually «any with backups».
6. **Allowed on a mobile connection** — on a phone: without it the backup
   waits for Wi-Fi.
7. «Add» — the first backup starts right away.

«Back up now» runs one out of turn. «Stop» interrupts it; next time it
resumes where it stopped, nothing already sent goes again.

## How it works

- Every version is the full list of the folder's files. The same content is
  stored on the server **once**: an unchanged file takes no space in a new
  version and is not sent again.
- A file that changes while being read (an open database, a log) is read
  again. If it keeps changing, it is **skipped** with a note and goes into
  the next backup. It is never stored half-written: the server checks every
  content against its hash.
- If the connection drops halfway, the backup resumes when the server is
  back (after 10 minutes or by the button).

## Restore

The **«Restore»** tab: backups of this device and your other devices (same
user). Choose a backup, a version, what to bring back — the whole folder, a
subfolder or one file — and **where** (a full path).

Restoring **never overwrites silently**:

- the same file already in place is left alone;
- a **different** file in place — your choice: put the restored one next to
  it as «name (из копии 13.11.2026).txt» (the default), skip it, or replace
  it (only if you pick that explicitly).

## Who can read backups

Only **this device and devices of the same SSH user** (one person). Other
devices of the network — even with access rules open — see neither the
backups nor their file lists and cannot even test whether some file is on
the server. A backup may hold everything from a home folder, so this is
strict.

## Space

Backups count against the shared plugin quota of the server. When space runs
out, the server first removes the oldest version of this backup (the one
that would drop out after the new one anyway). If that is not enough, the
backup is not made and you see a clear «the server ran out of space…» error.
Free some space or raise the quota in the server panel.

The server panel («Plugins» → network → «Backups») shows the size, the number
of versions and the time of the last backup of each device; a backup can be
deleted there too.
