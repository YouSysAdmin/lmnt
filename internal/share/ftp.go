package share

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"

	"github.com/yousysadmin/lmnt/internal/guest"
	"github.com/yousysadmin/lmnt/internal/netx"
)

const (
	ftpGuestPort uint16 = 21
	// ftpPassivePorts is how many data ports are forwarded 1:1 for passive
	// mode transfers.
	ftpPassivePorts = 9
	ftpConfPath     = "/etc/vsftpd/vsftpd.conf"
)

// FTP serves the mount with vsftpd. vsftpd refuses to run sessions as root,
// so files are accessed as an unprivileged user and disks whose permissions
// exclude that user are read-only or invisible.
type FTP struct {
	planned  bool
	listen   netip.Addr
	public   netip.Addr
	hostPort uint16
	readOnly bool
	// passiveFrom..passiveFrom+ftpPassivePorts-1 are forwarded with equal
	// host and guest numbers so the server can advertise them unchanged.
	passiveFrom uint16
}

// Name implements Backend.
func (*FTP) Name() string { return "ftp" }

// Plan implements Backend: one control port plus a contiguous passive range.
func (f *FTP) Plan(opts Options) (Plan, error) {
	opts = opts.withDefaults()
	f.planned = true
	f.listen = opts.Listen
	f.public = opts.FTPPublicIP
	f.readOnly = opts.ReadOnly

	base, err := netx.FreePortRange(opts.Listen, firstPort, 1+ftpPassivePorts)
	if err != nil {
		return Plan{}, fmt.Errorf("ftp: %w", err)
	}

	f.hostPort = base
	f.passiveFrom = base + 1

	forwards := make([]netx.Forward, 0, 1+ftpPassivePorts)
	forwards = append(forwards, forward(opts.Listen, base, ftpGuestPort))

	for i := range uint16(ftpPassivePorts) {
		p := f.passiveFrom + i
		forwards = append(forwards, forward(opts.Listen, p, p))
	}

	return Plan{Forwards: forwards}, nil
}

// Start implements Backend.
func (f *FTP) Start(ctx context.Context, g *guest.Guest, creds Credentials) (Info, error) {
	if !f.planned {
		return Info{}, errors.New("ftp: Plan was not called")
	}

	if err := checkCredentials(creds); err != nil {
		return Info{}, fmt.Errorf("ftp: %w", err)
	}

	// A real login shell is required: vsftpd checks it against /etc/shells.
	if err := g.EnsureUser(ctx, guest.User{Name: creds.User, UID: 1000, Shell: "/bin/sh"}); err != nil {
		return Info{}, fmt.Errorf("ftp: %w", err)
	}

	if err := g.SetPassword(ctx, creds.User, creds.Password); err != nil {
		return Info{}, fmt.Errorf("ftp: %w", err)
	}

	if err := g.WriteFile(ctx, ftpConfPath, []byte(f.config()), 0o600); err != nil {
		return Info{}, fmt.Errorf("ftp: %w", err)
	}

	if err := g.EnableService(ctx, "vsftpd"); err != nil {
		return Info{}, fmt.Errorf("ftp: %w", err)
	}

	info := Info{
		URL:      "ftp://" + hostPort(f.public, f.hostPort),
		User:     creds.User,
		Password: creds.Password,
		ReadOnly: f.readOnly,
		Hints:    []string{"FTP sends the password in clear text, keep it on trusted networks."},
	}

	if f.public != f.listen && f.listen.IsLoopback() {
		info.Hints = append(info.Hints, "The advertised address differs from the loopback bind address, pass --listen to accept remote connections.")
	}

	return info, nil
}

func (f *FTP) config() string {
	last := f.passiveFrom + ftpPassivePorts - 1

	return `listen=YES
listen_ipv6=NO
anonymous_enable=NO
local_enable=YES
write_enable=` + strings.ToUpper(yesNo(!f.readOnly)) + `
local_umask=022
local_root=` + guest.MountPoint + `
chroot_local_user=YES
allow_writeable_chroot=YES
seccomp_sandbox=NO
pasv_enable=YES
pasv_addr_resolve=NO
pasv_address=` + f.public.String() + `
pasv_min_port=` + portString(f.passiveFrom) + `
pasv_max_port=` + portString(last) + `
`
}
