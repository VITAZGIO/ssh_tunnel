# Backups

Backups are kept on the server. The program on a device copies a folder on a
schedule, and the server keeps it. Copies travel only over the device network, not
over the internet.

## What you see in the panel

In the **Plugins** section there is a block **Backups**. For each copy you see:

- **versions** — how many versions are kept;
- **keep** — how many versions to keep;
- **last** — when the last successful copy was made;
- **on disk** — how much space is used. Identical content is stored once.

The **Delete** button removes the copy with all its versions. This cannot be
undone, and the panel asks for confirmation.

## The schedule and versions are set on the device

The schedule, the folder and the number of versions are set by the program on the
device. The panel only shows them. The program has:

- **once a day** or **once a week**;
- **after changes**, when the folder has been quiet for a set number of minutes;
- **how many versions to keep** — the server removes extra ones itself.

More in the Book of the program, chapter "Backups".

## What happens when space runs out

The quota is shared by all plugins of the network (see the chapter "Server plugins").
When space runs out, the server first removes **the oldest version of the same
copy** beyond "keep". Other people's copies are not touched.

If there is still not enough space, the new copy stops. The device gets the answer
"the server is out of space". What is already written is not damaged.

## Restore

The program on the device restores files, not the panel. From any version you can
restore one file or the whole folder.

Restore **never overwrites files silently**. If the file on the device differs from
the copy, the restored one is placed next to it. Its name contains the mark "(from
copy …)". The old file stays where it is.

## Example

A laptop copies its "Documents" folder to the server. A phone of the same SSH user
sees this copy and can restore a file from it. If the phone already has a file with
that name but different content, the restored file lands next to it with the mark
"(from copy …)". The old file is not touched.

A device of another SSH user does not see this copy, even on the same network.

## Who sees whose copies

Only the device that made a copy, and the devices of the same SSH user, can read
it. Other devices of the network do not see the copies. Even open access rights do
not change this.
