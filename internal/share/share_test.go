package share

import (
	"context"
	"io"
	"log/slog"
	"net/netip"
	"strings"
	"testing"

	"github.com/yousysadmin/lmnt/internal/guest"
	"github.com/yousysadmin/lmnt/internal/netx"
)

// recorder is a guest.Executor that records scripts and stdin and succeeds.
type recorder struct {
	scripts []string
	stdins  []string
}

func (r *recorder) Run(_ context.Context, script string) ([]byte, error) {
	r.scripts = append(r.scripts, script)
	r.stdins = append(r.stdins, "")

	return nil, nil
}

func (r *recorder) Start(_ context.Context, script string, s guest.Streams) (guest.Process, error) {
	var in []byte
	if s.Stdin != nil {
		in, _ = io.ReadAll(s.Stdin)
	}

	r.scripts = append(r.scripts, script)
	r.stdins = append(r.stdins, string(in))

	return okProcess{}, nil
}

func (r *recorder) Shell(context.Context, guest.Terminal) error { return nil }

type okProcess struct{}

func (okProcess) Wait() error { return nil }

func (r *recorder) stdinFor(t *testing.T, scriptPrefix string) string {
	t.Helper()

	for i, s := range r.scripts {
		if strings.HasPrefix(s, scriptPrefix) {
			return r.stdins[i]
		}
	}

	t.Fatalf("no script starting with %q in %q", scriptPrefix, r.scripts)

	return ""
}

func (r *recorder) has(t *testing.T, substr string) {
	t.Helper()

	for _, s := range r.scripts {
		if strings.Contains(s, substr) {
			return
		}
	}

	t.Errorf("no script contains %q, scripts: %q", substr, r.scripts)
}

func newGuest() (*guest.Guest, *recorder) {
	r := &recorder{}

	return guest.New(slog.New(slog.DiscardHandler), r), r
}

var creds = Credentials{User: User, Password: "Pa55word"}

func TestNames(t *testing.T) {
	for _, n := range Names() {
		b, ok := Lookup(n)
		if !ok || b.Name() != n || !Known(n) {
			t.Errorf("backend %q not consistent", n)
		}
	}

	if _, ok := Lookup("nfs"); ok || Known("nfs") {
		t.Error("unknown backend found")
	}

	// Lookup must hand out independent instances.
	a, _ := Lookup("smb")
	b, _ := Lookup("smb")
	if a == b {
		t.Error("Lookup returned a shared instance")
	}
}

func TestNewPassword(t *testing.T) {
	seen := map[string]bool{}

	for range 50 {
		p, err := NewPassword()
		if err != nil {
			t.Fatal(err)
		}

		if len(p) != PasswordLength {
			t.Errorf("length %d", len(p))
		}

		for _, r := range p {
			if !strings.ContainsRune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789", r) {
				t.Errorf("bad rune %q in %q", r, p)
			}
		}

		if seen[p] {
			t.Errorf("duplicate password %q", p)
		}

		seen[p] = true
	}
}

func TestSMBPlanAndStart(t *testing.T) {
	b := &SMB{}

	plan, err := b.Plan(Options{})
	if err != nil {
		t.Fatal(err)
	}

	if len(plan.Forwards) != 1 {
		t.Fatalf("plan %+v", plan)
	}

	fw := plan.Forwards[0]
	if fw.GuestPort != 445 || fw.Host.Addr() != netx.Loopback || fw.Host.Port() < 9000 {
		t.Errorf("forward %v", fw)
	}

	g, r := newGuest()

	info, err := b.Start(t.Context(), g, creds)
	if err != nil {
		t.Fatal(err)
	}

	wantURL := "smb://127.0.0.1:" + portString(fw.Host.Port()) + "/lmnt"
	if info.URL != wantURL || info.User != User || info.Password != creds.Password {
		t.Errorf("info %+v, want url %q", info, wantURL)
	}

	r.has(t, "adduser -D -H -h /mnt -s /sbin/nologin -u 1000 -G lmnt lmnt")
	r.has(t, "cat > /etc/samba/smb.conf")
	r.has(t, "rc-service samba start")
	r.has(t, "smbpasswd -a -s lmnt")

	conf := r.stdinFor(t, "umask 077 && cat > /etc/samba/smb.conf")
	for _, want := range []string{"[lmnt]", "path = /mnt", "force user = root", "server min protocol = SMB2", "read only = no"} {
		if !strings.Contains(conf, want) {
			t.Errorf("smb.conf lacks %q", want)
		}
	}

	if got := r.stdinFor(t, "smbpasswd"); got != "Pa55word\nPa55word\n" {
		t.Errorf("smbpasswd stdin %q", got)
	}
}

func TestReadOnlyShares(t *testing.T) {
	cases := []struct {
		backend Backend
		conf    string
		want    []string
		reject  []string
	}{
		{&SMB{}, "umask 077 && cat > /etc/samba/smb.conf", []string{"read only = yes"}, []string{"read only = no"}},
		{&FTP{}, "umask 077 && cat > /etc/vsftpd/vsftpd.conf", []string{"write_enable=NO"}, []string{"write_enable=YES"}},
		{&SFTP{}, "umask 077 && cat > /etc/ssh/sshd_share_config", []string{"Subsystem sftp internal-sftp -R", "ForceCommand internal-sftp -R"}, nil},
	}

	for _, c := range cases {
		if _, err := c.backend.Plan(Options{ReadOnly: true}); err != nil {
			t.Fatal(err)
		}

		g, r := newGuest()

		info, err := c.backend.Start(t.Context(), g, creds)
		if err != nil {
			t.Fatalf("%s: %v", c.backend.Name(), err)
		}

		if !info.ReadOnly {
			t.Errorf("%s: info does not say read-only", c.backend.Name())
		}

		conf := r.stdinFor(t, c.conf)
		for _, want := range c.want {
			if !strings.Contains(conf, want) {
				t.Errorf("%s config lacks %q:\n%s", c.backend.Name(), want, conf)
			}
		}

		for _, bad := range c.reject {
			if strings.Contains(conf, bad) {
				t.Errorf("%s config still contains %q", c.backend.Name(), bad)
			}
		}
	}
}

func TestSMBStartWithoutPlan(t *testing.T) {
	g, _ := newGuest()

	if _, err := (&SMB{}).Start(t.Context(), g, creds); err == nil {
		t.Error("Start without Plan accepted")
	}

	b := &SMB{}
	if _, err := b.Plan(Options{}); err != nil {
		t.Fatal(err)
	}

	if _, err := b.Start(t.Context(), g, Credentials{}); err == nil {
		t.Error("empty credentials accepted")
	}
}

func TestFTPPlanAndStart(t *testing.T) {
	b := &FTP{}
	listen := netx.Loopback

	plan, err := b.Plan(Options{Listen: listen, FTPPublicIP: netip.MustParseAddr("192.168.1.5")})
	if err != nil {
		t.Fatal(err)
	}

	if len(plan.Forwards) != 1+ftpPassivePorts {
		t.Fatalf("%d forwards", len(plan.Forwards))
	}

	control := plan.Forwards[0]
	if control.GuestPort != 21 || control.Host.Port() < 9000 {
		t.Errorf("control forward %v", control)
	}

	for i, fw := range plan.Forwards[1:] {
		want := control.Host.Port() + 1 + uint16(i)
		if fw.Host.Port() != want || fw.GuestPort != want || fw.Host.Addr() != listen {
			t.Errorf("passive forward %d = %v, want port %d", i, fw, want)
		}
	}

	g, r := newGuest()

	info, err := b.Start(t.Context(), g, creds)
	if err != nil {
		t.Fatal(err)
	}

	if info.URL != "ftp://192.168.1.5:"+portString(control.Host.Port()) {
		t.Errorf("url %q", info.URL)
	}

	r.has(t, "adduser -D -H -h /mnt -s /bin/sh -u 1000 -G lmnt lmnt")
	r.has(t, "rc-service vsftpd start")

	if got := r.stdinFor(t, "chpasswd"); got != "lmnt:Pa55word\n" {
		t.Errorf("chpasswd stdin %q", got)
	}

	conf := r.stdinFor(t, "umask 077 && cat > /etc/vsftpd/vsftpd.conf")
	first := control.Host.Port() + 1
	last := first + ftpPassivePorts - 1

	for _, want := range []string{
		"pasv_address=192.168.1.5",
		"pasv_min_port=" + portString(first),
		"pasv_max_port=" + portString(last),
		"chroot_local_user=YES",
		"anonymous_enable=NO",
		"local_root=/mnt",
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("vsftpd.conf lacks %q:\n%s", want, conf)
		}
	}

}

func TestFTPPublicDefaultsToListen(t *testing.T) {
	b := &FTP{}
	if _, err := b.Plan(Options{}); err != nil {
		t.Fatal(err)
	}

	if b.public != netx.Loopback {
		t.Errorf("public ip %v", b.public)
	}
}

func TestSFTPPlanAndStart(t *testing.T) {
	b := &SFTP{}

	plan, err := b.Plan(Options{})
	if err != nil {
		t.Fatal(err)
	}

	if len(plan.Forwards) != 1 || plan.Forwards[0].GuestPort != sftpGuestPort {
		t.Fatalf("plan %+v", plan)
	}

	g, r := newGuest()

	info, err := b.Start(t.Context(), g, creds)
	if err != nil {
		t.Fatal(err)
	}

	if info.URL != "sftp://lmnt@127.0.0.1:"+portString(plan.Forwards[0].Host.Port())+"/" {
		t.Errorf("url %q", info.URL)
	}

	// The share user is an alias of root, written straight to passwd.
	r.has(t, "lmnt:x:0:0::/mnt:/bin/sh")
	r.has(t, "ssh-keygen -A")
	r.has(t, "/usr/sbin/sshd -f /etc/ssh/sshd_share_config")

	if got := r.stdinFor(t, "chpasswd"); got != "lmnt:Pa55word\n" {
		t.Errorf("chpasswd stdin %q", got)
	}

	conf := r.stdinFor(t, "umask 077 && cat > /etc/ssh/sshd_share_config")
	for _, want := range []string{
		"Port 2022",
		"PasswordAuthentication yes",
		"PubkeyAuthentication no",
		"PermitRootLogin yes",
		"AllowUsers lmnt",
		"ForceCommand internal-sftp",
		"AllowTcpForwarding no",
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("sshd config lacks %q", want)
		}
	}

	// Ordering: the account and password must exist before sshd starts.
	var userAt, sshdAt int
	for i, s := range r.scripts {
		if strings.Contains(s, ">> /etc/passwd") {
			userAt = i
		}

		if strings.HasPrefix(s, "/usr/sbin/sshd") {
			sshdAt = i
		}
	}

	if userAt >= sshdAt {
		t.Errorf("sshd started before the user existed: %q", r.scripts)
	}
}

func TestValidatePassword(t *testing.T) {
	good := []string{"hunter2", "a", strings.Repeat("x", MaxPasswordLength), "with spaces", `q!#$%^&*()"'\`}
	for _, p := range good {
		if err := ValidatePassword(p); err != nil {
			t.Errorf("%q rejected: %v", p, err)
		}
	}

	bad := map[string]string{
		"":                                       "empty",
		strings.Repeat("x", MaxPasswordLength+1): "too long",
		" leading":                               "leading space",
		"trailing ":                              "trailing space",
		"two\nlines":                             "newline",
		"tab\there":                              "tab",
		"pässwort":                               "non-ASCII",
	}

	for p, why := range bad {
		if err := ValidatePassword(p); err == nil {
			t.Errorf("%q (%s) accepted", p, why)
		}
	}

	// A generated password always passes.
	p, err := NewPassword()
	if err != nil {
		t.Fatal(err)
	}

	if err := ValidatePassword(p); err != nil {
		t.Errorf("generated password rejected: %v", err)
	}
}

func TestStartRejectsABadPassword(t *testing.T) {
	for _, b := range []Backend{&SMB{}, &FTP{}, &SFTP{}} {
		if _, err := b.Plan(Options{}); err != nil {
			t.Fatal(err)
		}

		g, _ := newGuest()

		_, err := b.Start(context.Background(), g, Credentials{User: User, Password: "two\nlines"})
		if err == nil {
			t.Errorf("%s: a password with a newline was accepted", b.Name())
		}
	}
}
