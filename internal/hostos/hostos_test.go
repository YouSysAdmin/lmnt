package hostos

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestIsRoot(t *testing.T) {
	root, err := IsRoot()
	if err != nil {
		t.Fatal(err)
	}
	if root && os.Getenv("LMNT_TEST_EXPECT_ROOT") == "" {
		t.Skip("running as root, nothing to assert")
	}
}

func TestValidateDevicePathRejectsRegularFile(t *testing.T) {
	f := filepath.Join(t.TempDir(), "plain")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := ValidateDevicePath(f); err == nil {
		t.Error("a regular file passed as a device")
	}
	if err := ValidateDevicePath(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("a missing path passed as a device")
	}
}

func TestProcessAlive(t *testing.T) {
	alive, err := ProcessAlive(os.Getpid())
	if err != nil || !alive {
		t.Errorf("own pid: alive=%v err=%v", alive, err)
	}

	if _, err := ProcessAlive(0); err == nil {
		t.Error("pid 0 accepted")
	}

	// A process that has exited and been reaped is gone.
	cmd := exec.Command(os.Args[0], "-test.run=TestNothingHere")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	_ = cmd.Wait()

	alive, err = ProcessAlive(pid)
	if err != nil {
		t.Fatal(err)
	}
	if alive {
		t.Errorf("reaped pid %d reported alive", pid)
	}
}

func TestMountTableMentions(t *testing.T) {
	table := []byte(`/dev/disk3s1s1 on / (apfs, sealed, local, read-only, journaled)
devfs on /dev (devfs, local, nobrowse)
/dev/disk3s5 on /System/Volumes/Data (apfs, local, journaled, nobrowse)
/dev/disk4s2 on /Volumes/USB (msdos, local, nodev, nosuid, noowners)
/dev/disk6s1s1 on /Volumes/Sealed (apfs, local, read-only)
`)

	cases := []struct {
		dev  string
		want bool
	}{
		{"/dev/disk4", true},
		{"/dev/disk4s2", true},
		{"/dev/disk3", true},
		{"/dev/disk3s1", true},
		{"/dev/disk6", true},
		{"/dev/disk40", false},
		{"/dev/disk5", false},
		{"/dev/disk4s", false},
	}

	for _, c := range cases {
		if got := mountTableMentions(table, []string{c.dev}); got != c.want {
			t.Errorf("%s: got %v, want %v", c.dev, got, c.want)
		}
	}
}

func TestDeviceMountedOnRootDisk(t *testing.T) {
	// A bogus device is certainly not mounted.
	mounted, err := DeviceMounted("/dev/definitely-not-a-disk")
	if err != nil {
		t.Fatal(err)
	}
	if mounted {
		t.Error("nonexistent device reported mounted")
	}
}

func TestLockFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")

	locked, err := FileLocked(path)
	if err != nil || locked {
		t.Fatalf("missing file: locked=%v err=%v", locked, err)
	}

	release, err := LockFile(path)
	if err != nil {
		t.Fatal(err)
	}

	locked, err = FileLocked(path)
	if err != nil || !locked {
		t.Errorf("held lock: locked=%v err=%v", locked, err)
	}

	if _, err := LockFile(path); err == nil {
		t.Error("second exclusive lock succeeded")
	}

	release()

	if _, err := os.Stat(path); err == nil {
		t.Error("lock file survived release")
	}
}
