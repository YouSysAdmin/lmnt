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
	smbGuestPort uint16 = 445
	// smbShareName is the share name clients see.
	smbShareName = "lmnt"
	smbConfPath  = "/etc/samba/smb.conf"
)

// SMB serves the mount over SMB2/3 with Samba. Files are written as root so
// that foreign disks with arbitrary ownership stay writable.
type SMB struct {
	planned  bool
	listen   netip.Addr
	hostPort uint16
	readOnly bool
}

// Name implements Backend.
func (*SMB) Name() string { return "smb" }

// Plan implements Backend: port 445 is forwarded to a free host port.
func (s *SMB) Plan(opts Options) (Plan, error) {
	opts = opts.withDefaults()
	s.planned = true
	s.listen = opts.Listen
	s.readOnly = opts.ReadOnly

	port, err := netx.FreePortRange(opts.Listen, firstPort, 1)
	if err != nil {
		return Plan{}, fmt.Errorf("smb: %w", err)
	}

	s.hostPort = port

	return Plan{Forwards: []netx.Forward{forward(opts.Listen, port, smbGuestPort)}}, nil
}

// Start implements Backend.
func (s *SMB) Start(ctx context.Context, g *guest.Guest, creds Credentials) (Info, error) {
	if !s.planned {
		return Info{}, errors.New("smb: Plan was not called")
	}

	if err := checkCredentials(creds); err != nil {
		return Info{}, fmt.Errorf("smb: %w", err)
	}

	if err := g.EnsureUser(ctx, guest.User{Name: creds.User, UID: 1000, Shell: "/sbin/nologin"}); err != nil {
		return Info{}, fmt.Errorf("smb: %w", err)
	}

	if err := g.WriteFile(ctx, smbConfPath, []byte(smbConfig(s.readOnly)), 0o644); err != nil {
		return Info{}, fmt.Errorf("smb: %w", err)
	}

	if err := g.EnableService(ctx, "samba"); err != nil {
		return Info{}, fmt.Errorf("smb: %w", err)
	}

	if err := g.SetSambaPassword(ctx, creds.User, creds.Password); err != nil {
		return Info{}, fmt.Errorf("smb: %w", err)
	}

	return Info{
		URL:      smbURL(s.listen.String(), s.hostPort),
		User:     creds.User,
		Password: creds.Password,
		ReadOnly: s.readOnly,
	}, nil
}

func smbURL(host string, port uint16) string {
	return "smb://" + bracket(host) + ":" + portString(port) + "/" + smbShareName
}

func bracket(host string) string {
	if strings.Contains(host, ":") {
		return "[" + host + "]"
	}

	return host
}

func smbConfig(readOnly bool) string {
	return `[global]
server role = standalone server
server min protocol = SMB2
workgroup = WORKGROUP
server string = lmnt
map to guest = never
load printers = no
printing = bsd
printcap name = /dev/null
disable spoolss = yes
log level = 0

[` + smbShareName + `]
path = ` + guest.MountPoint + `
read only = ` + yesNo(readOnly) + `
browseable = yes
force user = root
force group = root
create mask = 0664
directory mask = 0775
`
}
