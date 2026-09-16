# Guest image for lmnt's container provider.
#
# It carries the same tool set as the QEMU guest (LVM, cryptsetup, the
# filesystem utilities and the share daemons) plus what a container needs to
# stand in for a booted system: OpenRC so that rc-service works, and an LVM
# configuration that does not wait for a udev daemon that is not running.
# Share users are created at run time by the share backend, not here.
FROM alpine:3.24

RUN apk add --no-cache \
        openrc \
        lvm2 device-mapper cryptsetup util-linux \
        e2fsprogs xfsprogs btrfs-progs f2fs-tools ntfs-3g dosfstools \
        samba vsftpd openssh \
    && printf '%s\n' \
        'devices { obtain_device_list_from_udev = 0 }' \
        'activation { udev_sync = 0 udev_rules = 0 }' \
        > /etc/lvm/lvmlocal.conf
