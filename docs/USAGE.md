# Using lmnt

Every session follows the same three steps: pick the target, look at it with
`lmnt ls`, then `lmnt run` the device you want to share. Ctrl+C ends the
session and cleans up.

## 0. Or make a new disk

`lmnt create` writes a fresh image and puts a file system in it, so there is
nothing to find:

```sh
lmnt create vault.img --size 10G --encrypt --label vault
New passphrase for /dev/vdb:
Repeat passphrase:
```

```
Created /Users/you/vault.img (10 GiB, LUKS2, ext4).

  lmnt run -l img:/Users/you/vault.img
```

Without `--encrypt` the image is a plain file system and `lmnt run
img:vault.img` mounts it. `--size` takes `K`, `M`, `G` and `T` as powers of
1024 (a bare number is bytes), from 32 MiB up, `--fs` chooses among ext2,
ext3, ext4, xfs, btrfs, f2fs and vfat (default ext4), `--force` overwrites an
existing file.

The image is a sparse raw file: it reports its full size from the start but
only occupies disk space as it is written to. No partition table is written,
so the whole image is one volume: give `run` no device at all and it mounts
the attached disk itself, under either provider.

The passphrase is put through cryptsetup's own key derivation inside the
guest, which sizes its memory cost to the 2 GiB lmnt gives a LUKS guest. The
volume therefore opens again on the same setup without `--memory`.

## 1. Find the disk

**Disk image** (a `dd` dump, a `.img`): nothing to find, the target is
`img:/path/to/file.img`. No root needed, and both providers work.

**Physical disk**:

```sh
diskutil list
```

Pick the *whole disk* (`/dev/disk4`), not a slice (`/dev/disk4s1`). If macOS
mounted something from it, eject it first: `diskutil unmountDisk /dev/disk4`.
The target is `dev:/dev/disk4` and needs `sudo`.

## 2. Look inside

```sh
sudo lmnt ls dev:/dev/disk4
```

```
NAME               SIZE FSTYPE      LABEL
vdb                 1.8T
├─vdb1              512M vfat        EFI
├─vdb2                1G ext4        boot
└─vdb3              1.8T LVM2_member
  ├─vg0-root         50G ext4
  └─vg0-home        1.7T ext4
```

The names in the first column are what `run` takes: `vdb2` for a plain
partition, `mapper/vg0-home` for an LVM logical volume (lsblk shows it
without the `mapper/` prefix, `run` needs it).

With `--provider docker` the disk appears as `loop0`, `loop0p1`, ….

## 3. Mount and share

```sh
sudo lmnt run dev:/dev/disk4 mapper/vg0-home
```

```
SMB share is up. Press Ctrl+C to unmount and stop.

  URL:      smb://127.0.0.1:9000/lmnt
  User:     lmnt
  Password: 3fKq81zLpXw2Rm7N
```

Leave the terminal open. When finished, press Ctrl+C once and wait a few
seconds: lmnt unmounts, powers the guest off and removes the loop device or
VM.

Without a device argument the whole disk is mounted (`lmnt run img:part.img`
for an image that is a single file system). A third argument forces the file
system type: `lmnt run img:disk.img vdb1 ext4`. Mount options go through
`-o`: `-o noatime`, `-o subvol=@home,compress=zstd`.

### Read-only

```sh
sudo lmnt run -r dev:/dev/disk4 vdb1
```

`-r` (`--read-only`) promises that the disk is not changed. It is enforced at
every layer, not only by the mount: QEMU attaches the disk with `readonly=on`
(the Docker provider uses `losetup --read-only`), so the guest kernel gets a
read-only block device, LUKS mappings are opened with `cryptsetup
--readonly`, the file system is mounted with `-o ro`, and the share refuses
writes, so Finder shows a read-only volume rather than failing half-way
through a copy. The banner and `lmnt mounts` say `read-only` / `ro`.

A file system that was not unmounted cleanly asks to replay its journal
before it can be mounted, which a read-only device does not allow. Mount it
as it is with `-o noload` (ext4) or `-o norecovery` (xfs, btrfs):

```sh
lmnt run -r -o noload img:crashed.img vdb1
```

## The share password

By default every mount gets a fresh 16-character password, printed with the
URL and kept in the session record until the mount ends. To pick your own:

```sh
lmnt run img:disk.img vdb1 --ask-share-password   # typed, twice, not echoed
lmnt run img:disk.img vdb1 --share-password 'Str0ng!pass'
```

`--ask-share-password` is the one to prefer: a password on the command line is
visible to anyone who can list processes, and stays in the shell history.

The user stays `lmnt` either way. The password has to be a single line of
printable ASCII, at most 127 characters, without leading or trailing spaces -
the guest hands it to `smbpasswd`, `chpasswd` and vsftpd line by line, and a
password that cannot survive that is refused rather than silently mangled.

## Leaving a mount running

`lmnt run` holds the terminal for as long as the mount lives. `--detach` gives
it back:

```sh
lmnt run --detach img:disk.img vdb1
```

```
SFTP share is up in the background as lmnt-93a2e47a.

  URL:      sftp://lmnt@127.0.0.1:9000/
  User:     lmnt
  Password: 3fKq81zLpXw2Rm7N

  Log:      /Users/you/.lmnt/sessions/lmnt-93a2e47a.log
  Stop it:  lmnt stop lmnt-93a2e47a
```

The mount now belongs to a process of its own: closing the terminal, or
logging out of the shell, leaves it alone.

```sh
lmnt mounts              # everything running, wherever it was started
lmnt mounts -v           # with URL, user and password
lmnt stop lmnt-93a2e47a  # unmount and power the guest off
lmnt stop --all
```

`lmnt stop` waits for the mount to be really gone before it returns, because
the point is to unmount the file system and flush the disk rather than to kill
a process with a disk attached. That takes a few seconds.

A LUKS passphrase, and a share password asked for with
`--ask-share-password`, are collected before the mount goes to the background,
since the background process has no terminal to ask from. Whatever it would have
printed goes to its log file, which the command names when something fails.

One thing a background mount cannot do is `--shell`: there is no terminal to
open the shell on. Use `lmnt shell` on a disk that is not mounted, or run the
mount in the foreground.

## Connecting

### SMB

Finder → Go → Connect to Server (⌘K), paste the URL including the port, log
in as `lmnt` with the printed password. From a terminal:
`mount_smbfs //lmnt:PASSWORD@127.0.0.1:9000/lmnt /tmp/disk`.

The SMB server writes as root, so file ownership on the disk never gets in
the way.

### SFTP

```sh
lmnt run img:disk.img vdb1 --share sftp
```

```
  URL:      sftp://lmnt@127.0.0.1:9000/
```

Any SFTP client works: `sftp -P 9000 lmnt@127.0.0.1`, Cyberduck, Mountain
Duck, FileZilla, or `sshfs`. SFTP is the easiest protocol when the
share must be reached from another machine (`--listen 0.0.0.0`) and, like
SMB, writes as root. The server is a separate sshd instance inside the guest
that accepts only this user and only SFTP.

### FTP

```sh
lmnt run img:disk.img vdb1 --share ftp
```

FTP is the fallback when nothing else is available. It runs as an unprivileged
user in the guest, so files owned by root on the disk are read-only. For
remote clients set `--ftp-public-ip` to the address they see, since passive
FTP announces its own IP.

## LUKS

To make a new encrypted volume, see `lmnt create` above. To open one that
already exists — a LUKS partition (`FSTYPE crypto_LUKS` in `lmnt ls`):

```sh
lmnt run -l img:disk.img vdb2
Enter passphrase for /dev/vdb2:
```

A LUKS container with LVM inside (Ubuntu, Fedora and Debian full-disk
encryption look like this): open the container first so the volumes become
visible, then pick one.

```sh
lmnt ls  img:disk.img --luks-container vdb3
lmnt run img:disk.img --luks-container vdb3 mapper/vgubuntu-root
```

When the entire disk is one LUKS container (no partition table), `-c`
stands for `--luks-container <the disk>`.

Opening LUKS2 volumes needs memory for the key derivation. lmnt raises the
guest to 2 GiB automatically when `-l`, `--luks-container` or `-c` is used,
add `--memory 4096` if cryptsetup reports it cannot allocate enough.

## Shell access

```sh
lmnt shell img:disk.img          # root shell, disk attached as /dev/vdb
lmnt shell --open-network        # with internet, e.g. for apk add
lmnt run img:disk.img vdb1 --shell   # share first, then a shell
```

The guest is Alpine Linux with `apk`. It has `lsblk`, `mount`, `fdisk`,
`cryptsetup`, the LVM tools, `mkfs`/`fsck` for ext4, xfs, btrfs, f2fs, vfat
and NTFS. Everything else: `apk add <package>` (needs `--open-network`).
Changes to the guest itself are discarded when the session ends, changes to
the attached disk are real.

## Terminal UI

```sh
lmnt tui
```

The sidebar lists every mount: those of this TUI and those other terminals
started with `lmnt run` (marked with ⇡ and their PID). The panel on the right
shows the selected mount: target, device, status, and once the share is up
its URL, user and password. `enter` opens the full details with recent log
lines, `c` and `p` copy the URL or the password, `s` unmounts the selected
mount, `q` quits.

Mounts started in the TUI run in their own processes, so quitting the TUI
does not end them: `q` asks whether to leave them running or to unmount them
first. Left running, they show up in `lmnt mounts`, can be stopped with
`lmnt stop`, and are listed again by the next `lmnt tui`.

`n` opens the new-mount flow. Type the target (`img:…`, `dev:…`, `usb:…`),
choose between read-write and read-only access, pick the provider and the
share protocol with `↑`/`↓` and `enter`, set a share password or leave the
field empty for a random one, say whether a LUKS container must be opened
first, and the guest boots. With read-only chosen, the listing guest attaches
the disk read-only too, so the disk is never opened for writing. Its
device list appears as soon as it is up: LVM volumes show under their
physical volume, LUKS containers under their disk. Choose the device, set a
file system type or mount options if needed (`l` marks the device as a LUKS
volume, a `crypto_LUKS` device is marked automatically), and press `enter`.
A passphrase prompt, when one is needed, appears in the same screen. When
the share is up you are back on the mounts list with the new one selected.

Global flags such as `--provider docker` or `--memory 4096` set the defaults
of the flow.

A guest boots twice in this flow: once, briefly, to show what is on the disk,
and once for the mount itself, in the process that will own it. The first
guest is gone before the second starts, because QEMU keeps a write lock on the
disk image while its guest lives.

## Docker provider

```sh
lmnt ls  --provider docker img:disk.img
lmnt run --provider docker img:disk.img loop0p1 --share sftp
```

Same commands, different device names, one second to start. Physical disks
and USB are not possible: Docker Desktop and OrbStack run containers inside
their own Linux VM which has no access to the host's disks.
