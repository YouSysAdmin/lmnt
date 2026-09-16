# Installing lmnt

lmnt is a single binary. What it needs besides itself depends on which
provider runs the Linux guest.

| Provider         | Needs                                                                    | Handles                          |
|------------------|--------------------------------------------------------------------------|----------------------------------|
| `qemu` (default) | QEMU 8 or newer                                                          | physical disks, USB, disk images |
| `docker`         | any Docker-compatible runtime with a Linux VM (Docker Desktop, OrbStack) | disk images only                 |

You can install both and switch with `--provider`.

## QEMU provider

1. QEMU: `brew install qemu`. Apple silicon and Intel are both supported, the
   guest uses the Hypervisor framework (HVF).
2. lmnt:
   ```sh
   go install github.com/yousysadmin/lmnt/cmd/lmnt@latest      # needs Go 1.27+
   ```
   or download a release archive, unzip, and put `lmnt` somewhere on `PATH`.
3. Build the guest image once (downloads ~60 MB from dl-cdn.alpinelinux.org,
   plus the EDK2 firmware on Apple silicon):
   ```sh
   lmnt build
   ```

Physical disks (`dev:/dev/diskN`) need `sudo`. Disk images do not.

## Docker provider

No QEMU needed:

```sh
lmnt build --provider docker            # or let the first run build it
lmnt run --provider docker img:disk.img loop0p1
```

Docker Desktop and OrbStack work. The image is
`lmnt-guest:<hash>`, a new lmnt version with a changed guest definition
builds a new tag and leaves the old one untouched.

## Building from source

```sh
git clone https://github.com/yousysadmin/lmnt
cd lmnt
make build          # ./lmnt
make test
```

Go 1.27 or newer is required (`.go-version` is for goenv users).

## Where lmnt keeps things

| Path                        | Content                                                                    |
|-----------------------------|----------------------------------------------------------------------------|
| `~/.lmnt`                   | built guest image (`*.qcow2`), downloaded firmware, the registry of mounts |
| Docker image `lmnt-guest:*` | the container guest                                                        |

`lmnt clean` deletes the data directory, `docker image rm lmnt-guest:<tag>`
removes the container image. `--data-dir` moves the directory elsewhere.
