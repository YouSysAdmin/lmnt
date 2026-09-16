# Troubleshooting

Start with the error message. lmnt errors are chains that read from the
outside in, and the last part carries the failing command's own output:

```
Mount: guest command mount -t ext4 /dev/vdb1 /mnt exited with status 32: mount: /mnt: wrong fs type, bad option, bad superblock on /dev/vdb1
```

Tools for digging deeper:

- `--log-level debug` prints every command sent to the guest.
- `--debug` keeps the QEMU display window open (boot problems) and passes
  QEMU's or Docker's own diagnostics through.
- `lmnt shell <target>` gives a root shell in the guest with the disk
  attached, `lmnt run ... --shell` does the same after the share is up.

## Common problems

**`the guest image is not built yet`** — run `lmnt build` (or
`lmnt build --provider docker`) once.

**The VM does not boot / `boot timeout`** — run with `--debug` to see the
console and check that `qemu-system-aarch64` (or `qemu-system-x86_64` on an
Intel Mac) is on `PATH`. A slow machine may need
`--boot-timeout 120s --setup-timeout 240s`.

**`appears to be mounted on the host`** — eject the disk first (`diskutil unmountDisk /dev/diskN`). lmnt refuses to
share a disk the host is
using, because writing to the same file system from two systems corrupts it.

**The guest was killed with `mounted on the host` while running** — macOS
auto-mounted a partition of the passed-through disk. lmnt kills the guest
immediately to limit damage. Run `fsck` on the affected file system from
`lmnt shell` before using it again, `diskutil unmountDisk` right before
starting lmnt is usually enough to keep macOS away from the disk.

**`Not enough available memory to open a keyslot`** or LUKS open times out —
the key derivation wants more RAM than the guest has. Use `--memory 4096`
(lmnt already raises the default to 2048 when LUKS is involved).

**`wrong fs type` / `unknown filesystem type`** — pass the type explicitly as
the third argument (`lmnt run img:x.img vdb1 xfs`). If the file system is
one lmnt's guest lacks a driver for, `lmnt shell --open-network` and
`apk search` may help, the image ships ext2/3/4, xfs, btrfs, f2fs, vfat,
exfat and ntfs3.

**Finder shows the SMB share read-only** — unless the mount was started with
`-r` (or "read-only" in the TUI), which is exactly what that does, check the
password (Finder caches old ones in Keychain). lmnt's SMB server writes as
root, so permissions on the disk are never the cause.

**`cannot mount read-only` / `write access will be enabled during recovery`
/ `norecovery` with `-r`** — the file system was not unmounted cleanly and
wants to replay its journal, which the read-only disk forbids. Mount it as it
is with `-o noload` (ext4) or `-o norecovery` (xfs, btrfs), unreplayed
changes stay invisible. Or drop `-r` to let the journal be replayed, which
writes to the disk.

**The disk looks corrupt in the guest (`bad superblock`, partitions with
absurd sizes)** — for physical disks lmnt passes the real logical sector size
to the guest. A disk that was written by an older tool assuming 512 bytes
needs `--sector-size 512`, a 4Kn disk written natively must not have it.

**`docker` provider: `is a device`** — the Docker provider can only attach
image files. Use `--provider qemu` for physical disks.

**Something was left behind** — `lmnt clean` deletes the data directory,
`docker ps -a --filter label=app=lmnt` lists leftover containers (there
should be none), and `docker image ls lmnt-guest` the images.

## Reporting a bug

Include the full command, the complete output with `--log-level debug`, the
macOS version, `lmnt version`, and `qemu-system-* --version` or
`docker version`.
