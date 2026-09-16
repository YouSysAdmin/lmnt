package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yousysadmin/lmnt/internal/guest"
)

func TestParseSize(t *testing.T) {
	good := map[string]int64{
		"10G":      10 << 30,
		"512M":     512 << 20,
		"1T":       1 << 40,
		"1TiB":     1 << 40,
		"64MiB":    64 << 20,
		"33554432": 32 << 20,
		"  128M  ": 128 << 20,
		"32m":      32 << 20,
	}

	for in, want := range good {
		got, err := parseSize(in)
		if err != nil {
			t.Errorf("%q: %v", in, err)
			continue
		}

		if got != want {
			t.Errorf("%q: %d, want %d", in, got, want)
		}
	}

	bad := []string{
		"",            // missing
		"abc",         // not a number
		"10Q",         // unknown suffix
		"0",           // not positive
		"-1G",         // not positive
		"1M",          // below the minimum
		"33554433",    // not a multiple of 512
		"9999999999T", // overflows
	}

	for _, in := range bad {
		if got, err := parseSize(in); err == nil {
			t.Errorf("%q: got %d, want an error", in, got)
		}
	}
}

func TestFormatSize(t *testing.T) {
	cases := map[int64]string{
		10 << 30: "10 GiB",
		1 << 40:  "1 TiB",
		32 << 20: "32 MiB",
		1536:     "1536 B",
	}

	for in, want := range cases {
		if got := formatSize(in); got != want {
			t.Errorf("%d: %q, want %q", in, got, want)
		}
	}
}

func TestConfirmedPrompt(t *testing.T) {
	answers := func(values ...string) guest.PasswordPrompt {
		i := 0

		return func(string) ([]byte, error) {
			v := values[i]
			i++

			return []byte(v), nil
		}
	}

	secret, err := confirmedPrompt(answers("hunter2", "hunter2"))("Passphrase: ")
	if err != nil {
		t.Fatal(err)
	}

	if string(secret) != "hunter2" {
		t.Errorf("secret: %q", secret)
	}

	if _, err := confirmedPrompt(answers("hunter2", "hunter3"))("Passphrase: "); err == nil {
		t.Error("mismatched passphrases: expected an error")
	}

	if _, err := confirmedPrompt(answers("", ""))("Passphrase: "); err == nil {
		t.Error("empty passphrase: expected an error")
	}

	boom := errors.New("terminal closed")
	failing := func(string) ([]byte, error) { return nil, boom }

	if _, err := confirmedPrompt(failing)("Passphrase: "); !errors.Is(err, boom) {
		t.Errorf("error: %v, want %v", err, boom)
	}
}

// Bad invocations must be rejected before anything is created on disk.
func TestCreateUsageErrors(t *testing.T) {
	dir := t.TempDir()
	img := filepath.Join(dir, "new.img")

	cases := map[string]struct {
		args []string
		code int
	}{
		"no size":    {[]string{"create", img}, exitUsage},
		"bad size":   {[]string{"create", img, "--size", "10Q"}, exitUsage},
		"tiny size":  {[]string{"create", img, "--size", "1M"}, exitUsage},
		"unknown fs": {[]string{"create", img, "--size", "64M", "--fs", "zfs"}, exitUsage},
		// Cobra checks the argument count itself, and Main reports that as a
		// plain failure rather than one of its own usage errors.
		"no path":   {[]string{"create"}, exitFailure},
		"two paths": {[]string{"create", img, img, "--size", "64M"}, exitFailure},
	}

	for name, tc := range cases {
		if code := Main(tc.args); code != tc.code {
			t.Errorf("%s: exit %d, want %d", name, code, tc.code)
		}

		if _, err := os.Stat(img); !os.IsNotExist(err) {
			t.Fatalf("%s: %s was created anyway", name, img)
		}
	}
}

func TestCreateRefusesExistingFile(t *testing.T) {
	img := filepath.Join(t.TempDir(), "taken.img")

	if err := os.WriteFile(img, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}

	if code := Main([]string{"create", img, "--size", "64M"}); code != exitUsage {
		t.Errorf("exit %d, want %d", code, exitUsage)
	}

	data, err := os.ReadFile(img)
	if err != nil {
		t.Fatal(err)
	}

	if string(data) != "keep me" {
		t.Errorf("the existing file was touched: %q", data)
	}
}

func TestClaimPath(t *testing.T) {
	a := &app{stdout: os.Stdout, stderr: os.Stderr}

	// rootCommand applies the persistent flag defaults; setup builds a logger.
	if err := a.setup(a.rootCommand()); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()

	// A free path is claimed without doing anything.
	if err := a.claimPath(filepath.Join(dir, "free.img"), false); err != nil {
		t.Errorf("free path: %v", err)
	}

	img := filepath.Join(dir, "taken.img")
	if err := os.WriteFile(img, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := a.claimPath(img, false); err == nil {
		t.Error("existing file without --force: expected an error")
	}

	if err := a.claimPath(img, true); err != nil {
		t.Errorf("--force: %v", err)
	}

	if _, err := os.Stat(img); !os.IsNotExist(err) {
		t.Errorf("--force did not remove %s", img)
	}

	// A directory is never a disk image, --force or not.
	if err := a.claimPath(dir, true); err == nil {
		t.Error("directory: expected an error")
	}
}

func TestCreateSparseFile(t *testing.T) {
	img := filepath.Join(t.TempDir(), "sparse.img")

	if err := createSparseFile(img, 64<<20); err != nil {
		t.Fatal(err)
	}

	st, err := os.Stat(img)
	if err != nil {
		t.Fatal(err)
	}

	if st.Size() != 64<<20 {
		t.Errorf("size %d, want %d", st.Size(), 64<<20)
	}

	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode %o, want 600", perm)
	}

	// A second call must not clobber what is already there.
	if err := createSparseFile(img, 64<<20); err == nil {
		t.Error("expected an error for an existing file")
	}
}

func TestEnvCheckDevice(t *testing.T) {
	docker := &env{DiskDevice: "loop0"}
	qemu := &env{DiskDevice: "vdb"}

	ok := []struct {
		e    *env
		name string
	}{
		{docker, "loop0"},
		{docker, "loop0p1"},
		{docker, "mapper/vg-root"},
		{docker, ""},
		{qemu, "vdb"},
		{qemu, "vdb2"},
		{qemu, "mapper/cryptmnt"},
		// An unfamiliar name may still be a real device: dm-0, sr0, …
		{qemu, "dm-0"},
		{&env{}, "vdb"},
	}

	for _, c := range ok {
		if err := c.e.CheckDevice(c.name); err != nil {
			t.Errorf("%s on %s: %v", c.name, c.e.DiskDevice, err)
		}
	}

	// The other provider's scheme is the one case worth refusing.
	bad := []struct {
		e    *env
		name string
	}{
		{docker, "vdb"},
		{docker, "vdb1"},
		{qemu, "loop0"},
		{qemu, "loop0p1"},
	}

	for _, c := range bad {
		err := c.e.CheckDevice(c.name)
		if err == nil {
			t.Errorf("%s on %s: expected an error", c.name, c.e.DiskDevice)
			continue
		}

		if !isUsageError(err) {
			t.Errorf("%s on %s: %v is not a usage error", c.name, c.e.DiskDevice, err)
		}

		if !strings.Contains(err.Error(), c.e.DiskDevice) {
			t.Errorf("%s on %s: %v does not name the right device", c.name, c.e.DiskDevice, err)
		}
	}
}

// newTestImage makes an empty file that passes target.Parse.
func newTestImage(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "disk.img")

	if err := os.WriteFile(path, []byte{}, 0o600); err != nil {
		t.Fatal(err)
	}

	return path
}

func osPipe() (*os.File, *os.File, error) { return os.Pipe() }

// swapStdin points os.Stdin at r until the returned function is called.
func swapStdin(r *os.File) func() {
	saved := os.Stdin
	os.Stdin = r

	return func() { os.Stdin = saved }
}
