package share

import (
	"context"
	"errors"
	"fmt"
	"net/netip"

	"github.com/yousysadmin/lmnt/internal/guest"
	"github.com/yousysadmin/lmnt/internal/netx"
)

const (
	// sftpGuestPort is where the dedicated sshd listens. It is separate
	// from the guest's own sshd (port 22), which the QEMU provider uses as
	// its control channel with key authentication only.
	sftpGuestPort uint16 = 2022
	sftpConfPath         = "/etc/ssh/sshd_share_config"
	sftpHostKey          = "/etc/ssh/ssh_host_ed25519_key"
	sftpPidFile          = "/run/sshd_share.pid"
)

// SFTP serves the mount with a second OpenSSH daemon restricted to the SFTP
// subsystem. The share user is an alias of root so that writes succeed on
// disks with foreign ownership.
type SFTP struct {
	planned  bool
	listen   netip.Addr
	hostPort uint16
	readOnly bool
}

// Name implements Backend.
func (*SFTP) Name() string { return "sftp" }

// Plan implements Backend: one forwarded port.
func (s *SFTP) Plan(opts Options) (Plan, error) {
	opts = opts.withDefaults()
	s.planned = true
	s.listen = opts.Listen
	s.readOnly = opts.ReadOnly

	port, err := netx.FreePortRange(opts.Listen, firstPort, 1)
	if err != nil {
		return Plan{}, fmt.Errorf("sftp: %w", err)
	}

	s.hostPort = port

	return Plan{Forwards: []netx.Forward{forward(opts.Listen, port, sftpGuestPort)}}, nil
}

// Start implements Backend.
func (s *SFTP) Start(ctx context.Context, g *guest.Guest, creds Credentials) (Info, error) {
	if !s.planned {
		return Info{}, errors.New("sftp: Plan was not called")
	}

	if err := checkCredentials(creds); err != nil {
		return Info{}, fmt.Errorf("sftp: %w", err)
	}

	if err := g.EnsureUser(ctx, guest.User{Name: creds.User, UID: 0, Shell: "/bin/sh"}); err != nil {
		return Info{}, fmt.Errorf("sftp: %w", err)
	}

	if err := g.SetPassword(ctx, creds.User, creds.Password); err != nil {
		return Info{}, fmt.Errorf("sftp: %w", err)
	}

	if err := g.WriteFile(ctx, sftpConfPath, []byte(sftpConfig(creds.User, s.readOnly)), 0o600); err != nil {
		return Info{}, fmt.Errorf("sftp: %w", err)
	}

	// Host keys exist in the VM image (its own sshd made them) but not in a
	// fresh container; the privilege-separation directory likewise.
	prep := "[ -s " + guest.Quote(sftpHostKey) + " ] || ssh-keygen -A >/dev/null; mkdir -p /run/sshd /var/empty"
	if _, err := g.Run(ctx, prep); err != nil {
		return Info{}, fmt.Errorf("sftp: prepare host keys: %w", err)
	}

	launch := guest.QuoteAll("/usr/sbin/sshd", "-f", sftpConfPath)
	if _, err := g.Run(ctx, launch); err != nil {
		return Info{}, fmt.Errorf("sftp: start sshd: %w", err)
	}

	return Info{
		URL:      "sftp://" + creds.User + "@" + hostPort(s.listen, s.hostPort) + "/",
		User:     creds.User,
		Password: creds.Password,
		ReadOnly: s.readOnly,
		Hints:    []string{"Connect with sftp/scp, sshfs, or a client such as Cyberduck or FileZilla, Finder does not open sftp URLs."},
	}, nil
}

// sftpConfig is the configuration of the share daemon. PermitRootLogin is
// needed because the share user carries UID 0, everything but the SFTP
// subsystem is switched off. readOnly runs the SFTP server with -R, which
// rejects every request that would modify the file system.
func sftpConfig(user string, readOnly bool) string {
	sftp := "internal-sftp"
	if readOnly {
		sftp += " -R"
	}

	return `Port ` + portString(sftpGuestPort) + `
ListenAddress 0.0.0.0
ListenAddress ::
PidFile ` + sftpPidFile + `
HostKey ` + sftpHostKey + `
PasswordAuthentication yes
KbdInteractiveAuthentication no
PubkeyAuthentication no
PermitEmptyPasswords no
PermitRootLogin yes
AllowUsers ` + user + `
MaxAuthTries 6
Subsystem sftp ` + sftp + `
ForceCommand ` + sftp + `
AllowTcpForwarding no
AllowAgentForwarding no
X11Forwarding no
PermitTunnel no
PermitTTY no
PrintMotd no
`
}
