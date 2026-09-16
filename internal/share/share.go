// Package share exposes the file system mounted in the guest to the host as
// a network file share. Each Backend knows one protocol: how many ports it
// needs forwarded before the guest boots (Plan), and how to configure and
// start its daemon once the guest is up (Start).
package share

import (
	"context"
	"crypto/rand"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/yousysadmin/lmnt/internal/guest"
	"github.com/yousysadmin/lmnt/internal/netx"
)

// User is the account every backend authenticates against.
const User = "lmnt"

// firstPort is where the search for free host ports starts.
const firstPort uint16 = 9000

// Backend is one file share protocol.
type Backend interface {
	// Name is the identifier users pass on the command line.
	Name() string

	// Plan reserves the host ports the backend needs before the guest
	// starts. It must be called exactly once, before Start.
	Plan(opts Options) (Plan, error)

	// Start configures and launches the share daemon in the guest and says
	// how to connect to it.
	Start(ctx context.Context, g *guest.Guest, creds Credentials) (Info, error)
}

// Options is how the share is exposed: where it listens and whether clients
// may write.
type Options struct {
	// Listen is the host address forwarded ports bind to. Zero means
	// loopback.
	Listen netip.Addr
	// FTPPublicIP is the address the FTP server advertises for passive
	// connections. Zero means Listen.
	FTPPublicIP netip.Addr
	// ReadOnly makes the daemon refuse writes, so clients see a read-only
	// share instead of failing on a read-only file system underneath.
	ReadOnly bool
}

func (o Options) withDefaults() Options {
	if !o.Listen.IsValid() {
		o.Listen = netx.Loopback
	}

	if !o.FTPPublicIP.IsValid() {
		o.FTPPublicIP = o.Listen
	}

	return o
}

// Plan is what the provider must set up before the guest starts.
type Plan struct {
	Forwards []netx.Forward
}

// Credentials are the account the share is protected with.
type Credentials struct {
	User     string
	Password string
}

// Info tells the user how to connect.
type Info struct {
	URL      string
	User     string
	Password string
	// ReadOnly says the share refuses writes.
	ReadOnly bool
	// Hints are short notes worth printing next to the URL.
	Hints []string
}

// Names lists the available backends in display order.
func Names() []string {
	return []string{"smb", "ftp", "sftp"}
}

// Lookup returns a fresh backend for name.
func Lookup(name string) (Backend, bool) {
	switch name {
	case "smb":
		return &SMB{}, true
	case "ftp":
		return &FTP{}, true
	case "sftp":
		return &SFTP{}, true
	default:
		return nil, false
	}
}

// Known reports whether name is a backend.
func Known(name string) bool {
	return slices.Contains(Names(), name)
}

// PasswordLength is the length of generated share passwords.
const PasswordLength = 16

// MaxPasswordLength is the longest password the backends accept. Samba
// truncates beyond this, and a password that is silently cut in half is worse
// than one that is refused.
const MaxPasswordLength = 127

// ValidatePassword reports whether p can be used as the share password. The
// backends hand it to smbpasswd, chpasswd and vsftpd over stdin, one line at
// a time, so anything that is not a single line of printable ASCII is
// refused rather than mangled.
func ValidatePassword(p string) error {
	switch {
	case p == "":
		return fmt.Errorf("the password is empty")
	case len(p) > MaxPasswordLength:
		return fmt.Errorf("the password is longer than %d characters", MaxPasswordLength)
	case strings.TrimSpace(p) != p:
		return fmt.Errorf("the password starts or ends with a space")
	}

	for _, r := range p {
		if r < 0x20 || r > 0x7e {
			return fmt.Errorf("the password may only contain printable ASCII characters")
		}
	}

	return nil
}

// NewPassword returns a random alphanumeric password.
func NewPassword() (string, error) {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

	out := make([]byte, 0, PasswordLength)
	buf := make([]byte, 64)

	for len(out) < PasswordLength {
		if _, err := rand.Read(buf); err != nil {
			return "", fmt.Errorf("generate password: %w", err)
		}

		for _, b := range buf {
			// Rejection sampling keeps the distribution uniform: 248 is the
			// largest multiple of len(alphabet) that fits in a byte.
			if b >= 248 {
				continue
			}

			out = append(out, alphabet[int(b)%len(alphabet)])
			if len(out) == PasswordLength {
				break
			}
		}
	}

	return string(out), nil
}

// forward maps guestPort to hostPort on the listen address.
func forward(listen netip.Addr, hostPort, guestPort uint16) netx.Forward {
	return netx.Forward{Host: netip.AddrPortFrom(listen, hostPort), GuestPort: guestPort}
}

func hostPort(addr netip.Addr, port uint16) string {
	return netip.AddrPortFrom(addr, port).String()
}

// yesNo renders a flag the way samba and vsftpd spell booleans.
func yesNo(b bool) string {
	if b {
		return "yes"
	}

	return "no"
}

func portString(p uint16) string {
	return strconv.Itoa(int(p))
}

func checkCredentials(creds Credentials) error {
	if creds.User == "" {
		return fmt.Errorf("share credentials are incomplete")
	}

	if err := ValidatePassword(creds.Password); err != nil {
		return fmt.Errorf("share password: %w", err)
	}

	return nil
}
