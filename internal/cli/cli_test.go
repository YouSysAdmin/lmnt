package cli

import (
	"errors"
	"os"
	"slices"
	"testing"

	"github.com/yousysadmin/lmnt/internal/guest"
	"github.com/yousysadmin/lmnt/internal/share"
)

func TestMainVersionAndUsage(t *testing.T) {
	if code := Main([]string{"version"}); code != exitOK {
		t.Errorf("version: exit %d", code)
	}

	if code := Main([]string{"--provider", "vbox", "version"}); code != exitUsage {
		t.Errorf("bad provider: exit %d, want %d", code, exitUsage)
	}

	if code := Main([]string{"--sector-size", "513", "version"}); code != exitUsage {
		t.Errorf("bad sector size: exit %d, want %d", code, exitUsage)
	}

	if code := Main([]string{"--boot-timeout", "2m", "--setup-timeout", "1m", "version"}); code != exitUsage {
		t.Errorf("timeouts: exit %d, want %d", code, exitUsage)
	}

	if code := Main([]string{"no-such-command"}); code != exitFailure {
		t.Errorf("unknown command: exit %d", code)
	}
}

func TestLsTargetErrors(t *testing.T) {
	// A missing image is a usage problem, reported before any guest starts.
	if code := Main([]string{"ls", "img:/definitely/not/here.img"}); code != exitUsage {
		t.Errorf("missing image: exit %d, want %d", code, exitUsage)
	}

	if code := Main([]string{"ls", "img:x", "--luks-container", "vdb1", "-c"}); code != exitUsage {
		t.Errorf("conflicting luks flags: exit %d, want %d", code, exitUsage)
	}
}

func TestRunRequiresDeviceWithContainer(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "disk-*.img")
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	if code := Main([]string{"run", "img:" + f.Name(), "-c"}); code != exitUsage {
		t.Errorf("-c without device: exit %d, want %d", code, exitUsage)
	}

	if code := Main([]string{"run", "img:" + f.Name(), "vdb1", "--share", "nfs"}); code != exitUsage {
		t.Errorf("unknown share: exit %d, want %d", code, exitUsage)
	}
}

func TestLuksOptionsDevice(t *testing.T) {
	o := luksOptions{entireDrive: true}
	if _, err := o.device(""); !isUsageError(err) {
		t.Errorf("-c without a disk should be a usage error, got %v", err)
	}

	dev, err := o.device("loop0")
	if err != nil || dev != "loop0" {
		t.Errorf("got %q, %v", dev, err)
	}

	o = luksOptions{container: "vdb3"}
	dev, err = o.device("vdb")
	if err != nil || dev != "vdb3" {
		t.Errorf("got %q, %v", dev, err)
	}

	if dev, err := (&luksOptions{}).device("vdb"); err != nil || dev != "" {
		t.Errorf("no container requested: got %q, %v", dev, err)
	}
}

func TestSplitList(t *testing.T) {
	got := splitList(" a, b,,c ,")
	if !slices.Equal(got, []string{"a", "b", "c"}) {
		t.Errorf("got %q", got)
	}

	if got := splitList(""); got != nil {
		t.Errorf("empty input gave %q", got)
	}
}

func TestUsageErrorWrapping(t *testing.T) {
	inner := errors.New("boom")
	err := usagef("context: %w", inner)

	if !isUsageError(err) || !errors.Is(err, inner) {
		t.Errorf("usage error lost its identity: %v", err)
	}
}

func TestSharePassword(t *testing.T) {
	// Nothing asked for: a fresh random password.
	first, err := sharePassword(&runOptions{}, nil)
	if err != nil {
		t.Fatal(err)
	}

	second, err := sharePassword(&runOptions{}, nil)
	if err != nil {
		t.Fatal(err)
	}

	if first == second || len(first) != share.PasswordLength {
		t.Errorf("generated passwords %q and %q", first, second)
	}

	// An explicit one is taken as is.
	got, err := sharePassword(&runOptions{sharePassword: "hunter2"}, nil)
	if err != nil {
		t.Fatal(err)
	}

	if got != "hunter2" {
		t.Errorf("password %q, want hunter2", got)
	}

	// --ask-share-password in a detached child reads the line its parent
	// handed over, and checks it.
	handed := func(values ...string) guest.PasswordPrompt {
		i := 0

		return func(string) ([]byte, error) {
			v := values[i]
			i++

			return []byte(v), nil
		}
	}

	got, err = sharePassword(&runOptions{askPassword: true}, handed("s3cret"))
	if err != nil {
		t.Fatal(err)
	}

	if got != "s3cret" {
		t.Errorf("password %q, want s3cret", got)
	}

	if _, err := sharePassword(&runOptions{askPassword: true}, handed("")); err == nil {
		t.Error("an empty handed-over password was accepted")
	}
}

func TestRunRejectsBadSharePasswords(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "disk-*.img")
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	img := "img:" + f.Name()

	if code := Main([]string{"run", img, "vdb1", "--share-password", "two\nlines"}); code != exitUsage {
		t.Errorf("password with a newline: exit %d, want %d", code, exitUsage)
	}

	if code := Main([]string{"run", img, "vdb1", "--share-password", "x", "--ask-share-password"}); code != exitUsage {
		t.Errorf("both password flags: exit %d, want %d", code, exitUsage)
	}
}
