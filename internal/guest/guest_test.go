package guest

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func newTestGuest(t *testing.T) (*Guest, *fakeExecutor) {
	t.Helper()

	f := newFake()
	g := New(slog.New(slog.DiscardHandler), f)
	g.Prompt = nil

	return g, f
}

func lastScript(t *testing.T, f *fakeExecutor) string {
	t.Helper()

	if len(f.scripts) == 0 {
		t.Fatal("no script was run")
	}

	return f.scripts[len(f.scripts)-1]
}

func TestValidDeviceName(t *testing.T) {
	good := []string{"vdb", "vdb1", "loop0p2", "mapper/vg-lv", "mapper/cryptmnt", "sda_1"}
	bad := []string{"", "/dev/vdb", "../etc", "vdb 1", "mapper/", "mapper/a/b", "vdb;rm", "vd$b", "mapper/mapper/x"}

	for _, n := range good {
		if !ValidDeviceName(n) {
			t.Errorf("%q should be valid", n)
		}
	}

	for _, n := range bad {
		if ValidDeviceName(n) {
			t.Errorf("%q should be invalid", n)
		}
	}
}

func TestWriteFile(t *testing.T) {
	g, f := newTestGuest(t)

	err := g.WriteFile(t.Context(), "/etc/samba/smb.conf", []byte("[global]\n"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	if got := lastScript(t, f); got != "umask 077 && cat > /etc/samba/smb.conf && chmod 0600 /etc/samba/smb.conf" {
		t.Errorf("script: %q", got)
	}

	if f.stdins[0] != "[global]\n" {
		t.Errorf("stdin: %q", f.stdins[0])
	}

	if err := g.WriteFile(t.Context(), "relative", nil, 0o600); err == nil {
		t.Error("relative path accepted")
	}

	err = g.WriteFile(t.Context(), "/tmp/with space", nil, 0o400)
	if err != nil {
		t.Fatal(err)
	}

	if got := lastScript(t, f); !strings.Contains(got, "cat > '/tmp/with space' && chmod 0400 '/tmp/with space'") {
		t.Errorf("script: %q", got)
	}
}

func TestWriteFileFoldsStderrIntoError(t *testing.T) {
	g, f := newTestGuest(t)
	f.respond("cat >", fakeResponse{exit: 1, stderr: "sh: can't create /x/y: nonexistent directory\n"})

	err := g.WriteFile(t.Context(), "/x/y", []byte("a"), 0o600)
	if err == nil {
		t.Fatal("expected an error")
	}

	if ExitCode(err) != 1 {
		t.Errorf("exit code %d", ExitCode(err))
	}

	if !strings.Contains(err.Error(), "nonexistent directory") {
		t.Errorf("stderr not folded in: %v", err)
	}
}

func TestListBlockDevices(t *testing.T) {
	g, f := newTestGuest(t)
	f.respond("lsblk", fakeResponse{stdout: "NAME SIZE\nvdb 1G\n"})

	out, err := g.ListBlockDevices(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	if string(out) != "NAME SIZE\nvdb 1G\n" {
		t.Errorf("output %q", out)
	}

	if got := lastScript(t, f); got != "lsblk --output NAME,SIZE,FSTYPE,LABEL --exclude 2,7,11" {
		t.Errorf("script: %q", got)
	}

	if _, err := g.ListBlockDevices(t.Context(), "loop0", "mapper/vg-lv"); err != nil {
		t.Fatal(err)
	}

	if got := lastScript(t, f); got != "lsblk --output NAME,SIZE,FSTYPE,LABEL /dev/loop0 /dev/mapper/vg-lv" {
		t.Errorf("script: %q", got)
	}

	if _, err := g.ListBlockDevices(t.Context(), "../sda"); err == nil {
		t.Error("bad device name accepted")
	}
}

func TestActivateLVM(t *testing.T) {
	g, f := newTestGuest(t)

	if err := g.ActivateLVM(t.Context()); err != nil {
		t.Fatal(err)
	}

	got := lastScript(t, f)
	if !strings.HasPrefix(got, "vgchange -ay") || !strings.Contains(got, "dmsetup mknodes") {
		t.Errorf("script: %q", got)
	}
}

func TestEnableService(t *testing.T) {
	g, f := newTestGuest(t)

	if err := g.EnableService(t.Context(), "samba"); err != nil {
		t.Fatal(err)
	}

	if got := lastScript(t, f); got != "rc-service samba start" {
		t.Errorf("script: %q", got)
	}

	if err := g.EnableService(t.Context(), "samba; reboot"); err == nil {
		t.Error("bad service name accepted")
	}
}

func TestMountPlain(t *testing.T) {
	g, f := newTestGuest(t)

	err := g.Mount(t.Context(), MountRequest{Device: "vdb1"})
	if err != nil {
		t.Fatal(err)
	}

	if got := lastScript(t, f); got != "mount /dev/vdb1 /mnt" {
		t.Errorf("script: %q", got)
	}

	err = g.Mount(t.Context(), MountRequest{Device: "mapper/vg-home", FSType: "btrfs", Options: "subvol=@home,compress=zstd:3,ro"})
	if err != nil {
		t.Fatal(err)
	}

	if got := lastScript(t, f); got != "mount -t btrfs -o subvol=@home,compress=zstd:3,ro /dev/mapper/vg-home /mnt" {
		t.Errorf("script: %q", got)
	}
}

func TestMountReadOnly(t *testing.T) {
	g, f := newTestGuest(t)
	g.Prompt = func(string) ([]byte, error) { return []byte("pw"), nil }

	err := g.Mount(t.Context(), MountRequest{Device: "vdb1", ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}

	if got := lastScript(t, f); got != "mount -o ro /dev/vdb1 /mnt" {
		t.Errorf("script: %q", got)
	}

	// A user "rw" loses to the read-only request: mount takes the last word.
	err = g.Mount(t.Context(), MountRequest{Device: "vdb1", Options: "noatime,rw", ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}

	if got := lastScript(t, f); got != "mount -o noatime,rw,ro /dev/vdb1 /mnt" {
		t.Errorf("script: %q", got)
	}

	// Both LUKS layers are opened read-only too.
	f.scripts = nil
	err = g.Mount(t.Context(), MountRequest{Device: "vdb2", LUKS: true, LUKSContainer: "vdb", ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}

	want := []string{
		"cryptsetup open --type luks --readonly /dev/vdb cryptcontainer",
		"vgchange -ay",
		"cryptsetup open --type luks --readonly /dev/vdb2 cryptmnt",
		"mount -o ro /dev/mapper/cryptmnt /mnt",
	}

	if len(f.scripts) != len(want) {
		t.Fatalf("scripts: %q", f.scripts)
	}

	for i, w := range want {
		if !strings.HasPrefix(f.scripts[i], w) {
			t.Errorf("script %d = %q, want prefix %q", i, f.scripts[i], w)
		}
	}
}

func TestMountRejectsBadInput(t *testing.T) {
	g, _ := newTestGuest(t)

	cases := []MountRequest{
		{},
		{Device: "/dev/vdb1"},
		{Device: "vdb1", FSType: "ext4; reboot"},
		{Device: "vdb1", FSType: "NTFS"},
		{Device: "vdb1", Options: "ro,$(id)"},
		{Device: "vdb1", Options: "uid=1000 gid=1000"},
	}

	for _, c := range cases {
		if err := g.Mount(t.Context(), c); err == nil {
			t.Errorf("%+v accepted", c)
		}
	}
}

func TestMountLUKS(t *testing.T) {
	g, f := newTestGuest(t)

	var prompts []string
	g.Prompt = func(p string) ([]byte, error) {
		prompts = append(prompts, p)
		return []byte("s3cret"), nil
	}

	err := g.Mount(t.Context(), MountRequest{Device: "vdb2", LUKS: true})
	if err != nil {
		t.Fatal(err)
	}

	if len(f.scripts) != 2 {
		t.Fatalf("scripts: %q", f.scripts)
	}

	if f.scripts[0] != "cryptsetup open --type luks /dev/vdb2 cryptmnt" {
		t.Errorf("luks script: %q", f.scripts[0])
	}

	if f.stdins[0] != "s3cret\n" {
		t.Errorf("passphrase fed as %q", f.stdins[0])
	}

	if f.scripts[1] != "mount /dev/mapper/cryptmnt /mnt" {
		t.Errorf("mount script: %q", f.scripts[1])
	}

	if len(prompts) != 1 || !strings.Contains(prompts[0], "/dev/vdb2") {
		t.Errorf("prompts: %q", prompts)
	}
}

func TestMountLUKSContainerThenLVM(t *testing.T) {
	g, f := newTestGuest(t)
	g.Prompt = func(string) ([]byte, error) { return []byte("pw"), nil }

	err := g.Mount(t.Context(), MountRequest{Device: "mapper/vg-root", LUKSContainer: "vdb"})
	if err != nil {
		t.Fatal(err)
	}

	want := []string{
		"cryptsetup open --type luks /dev/vdb cryptcontainer",
		"vgchange -ay",
		"mount /dev/mapper/vg-root /mnt",
	}

	if len(f.scripts) != len(want) {
		t.Fatalf("scripts: %q", f.scripts)
	}

	for i, w := range want {
		if !strings.HasPrefix(f.scripts[i], w) {
			t.Errorf("script %d = %q, want prefix %q", i, f.scripts[i], w)
		}
	}
}

func TestOpenLUKSErrors(t *testing.T) {
	g, f := newTestGuest(t)

	err := g.OpenLUKS(t.Context(), "vdb", "cryptmnt", nil)
	if !errors.Is(err, ErrNoPrompt) {
		t.Errorf("want ErrNoPrompt, got %v", err)
	}

	promptErr := errors.New("tty gone")
	err = g.OpenLUKS(t.Context(), "vdb", "cryptmnt", func(string) ([]byte, error) { return nil, promptErr })
	if !errors.Is(err, promptErr) {
		t.Errorf("prompt error not propagated: %v", err)
	}

	f.respond("cryptsetup", fakeResponse{exit: 2, stderr: "Not enough available memory to open a keyslot.\n"})
	err = g.OpenLUKS(t.Context(), "vdb", "cryptmnt", func(string) ([]byte, error) { return []byte("x"), nil })
	if err == nil || !strings.Contains(err.Error(), "--memory") {
		t.Errorf("no memory hint in %v", err)
	}

	if err := g.OpenLUKS(t.Context(), "vdb", "bad name", func(string) ([]byte, error) { return []byte("x"), nil }); err == nil {
		t.Error("bad mapping name accepted")
	}
}

func TestOpenLUKSTimeoutStartsAfterPrompt(t *testing.T) {
	g, _ := newTestGuest(t)
	g.LUKSTimeout = time.Hour

	// A prompt that takes longer than the budget must not trip it.
	slow := func(string) ([]byte, error) {
		time.Sleep(20 * time.Millisecond)
		return []byte("x"), nil
	}

	g.LUKSTimeout = 5 * time.Millisecond

	if err := g.OpenLUKS(t.Context(), "vdb", "cryptmnt", slow); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestEnsureUser(t *testing.T) {
	g, f := newTestGuest(t)

	if err := g.EnsureUser(t.Context(), User{Name: "lmnt", UID: 1000}); err != nil {
		t.Fatal(err)
	}

	if got := lastScript(t, f); got != "id lmnt >/dev/null 2>&1 || { addgroup -g 1000 lmnt 2>/dev/null; adduser -D -H -h /mnt -s /bin/sh -u 1000 -G lmnt lmnt; }" {
		t.Errorf("script: %q", got)
	}

	if err := g.EnsureUser(t.Context(), User{Name: "lmnt", UID: 0, Shell: "/sbin/nologin"}); err != nil {
		t.Fatal(err)
	}

	got := lastScript(t, f)
	for _, want := range []string{"lmnt:x:0:0::/mnt:/sbin/nologin", ">> /etc/passwd", "lmnt:!::0:::::", ">> /etc/shadow"} {
		if !strings.Contains(got, want) {
			t.Errorf("script %q lacks %q", got, want)
		}
	}

	if strings.Contains(got, "adduser") {
		t.Errorf("uid 0 must not use adduser: %q", got)
	}

	bad := []User{
		{Name: "Root", UID: 1},
		{Name: "lmnt", UID: -1},
		{Name: "lmnt", UID: 1, Home: "relative"},
		{Name: "lmnt", UID: 1, Shell: "/bin/sh:evil"},
		{Name: "a b", UID: 1},
	}

	for _, u := range bad {
		if err := g.EnsureUser(t.Context(), u); err == nil {
			t.Errorf("%+v accepted", u)
		}
	}
}

func TestSetPassword(t *testing.T) {
	g, f := newTestGuest(t)

	if err := g.SetPassword(t.Context(), "lmnt", "p:a$s'w"); err != nil {
		t.Fatal(err)
	}

	if got := lastScript(t, f); got != "chpasswd" {
		t.Errorf("script: %q", got)
	}

	if f.stdins[0] != "lmnt:p:a$s'w\n" {
		t.Errorf("stdin: %q", f.stdins[0])
	}

	if err := g.SetPassword(t.Context(), "lmnt", ""); err == nil {
		t.Error("empty password accepted")
	}

	if err := g.SetPassword(t.Context(), "lmnt", "a\nb"); err == nil {
		t.Error("multi-line password accepted")
	}
}

func TestSetSambaPassword(t *testing.T) {
	g, f := newTestGuest(t)

	if err := g.SetSambaPassword(t.Context(), "lmnt", "pw"); err != nil {
		t.Fatal(err)
	}

	if got := lastScript(t, f); got != "smbpasswd -a -s lmnt" {
		t.Errorf("script: %q", got)
	}

	if f.stdins[0] != "pw\npw\n" {
		t.Errorf("stdin: %q", f.stdins[0])
	}

	f.respond("smbpasswd", fakeResponse{exit: 1, stderr: "Failed to add entry for user lmnt.\n"})

	err := g.SetSambaPassword(t.Context(), "lmnt", "pw")
	if err == nil || !strings.Contains(err.Error(), "Failed to add entry") {
		t.Errorf("stderr missing: %v", err)
	}
}

func TestRunAppliesTimeout(t *testing.T) {
	g, _ := newTestGuest(t)
	g.Timeout = -time.Second // already expired

	if _, err := g.Run(t.Context(), "true"); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("want deadline exceeded, got %v", err)
	}
}
