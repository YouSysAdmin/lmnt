package cli

import (
	"strings"
	"testing"
	"time"
)

func TestStopUsageErrors(t *testing.T) {
	cases := map[string][]string{
		"no ids":        {"stop"},
		"ids and --all": {"stop", "lmnt-12345678", "--all"},
		"unknown mount": {"stop", "lmnt-12345678"},
		"malformed id":  {"stop", "nonsense"},
	}

	for name, args := range cases {
		if code := Main(args); code != exitUsage {
			t.Errorf("%s: exit %d, want %d", name, code, exitUsage)
		}
	}
}

// With nothing running both commands must succeed and say so.
func TestMountsAndStopAllWhenIdle(t *testing.T) {
	dir := t.TempDir()

	for _, args := range [][]string{
		{"--data-dir", dir, "mounts"},
		{"--data-dir", dir, "stop", "--all"},
	} {
		if code := Main(args); code != exitOK {
			t.Errorf("%v: exit %d, want %d", args, code, exitOK)
		}
	}
}

func TestAge(t *testing.T) {
	cases := map[time.Duration]string{
		3 * time.Second:               "3s",
		90 * time.Second:              "1m",
		45 * time.Minute:              "45m",
		2*time.Hour + 5*time.Minute:   "2h5m",
		26*time.Hour + 30*time.Minute: "26h30m",
	}

	for d, want := range cases {
		if got := age(time.Now().Add(-d)); got != want {
			t.Errorf("%v: %q, want %q", d, got, want)
		}
	}
}

func TestDetachedID(t *testing.T) {
	t.Setenv(detachEnv, "")

	if got := detachedID(); got != "" {
		t.Errorf("unset: %q", got)
	}

	t.Setenv(detachEnv, "not-a-session-id")

	if got := detachedID(); got != "" {
		t.Errorf("malformed ids must be ignored, got %q", got)
	}

	t.Setenv(detachEnv, "lmnt-0a1b2c3d")

	if got := detachedID(); got != "lmnt-0a1b2c3d" {
		t.Errorf("valid id: %q", got)
	}
}

func TestDetachRejectsShell(t *testing.T) {
	img := newTestImage(t)

	// --shell needs a terminal the background process does not have.
	if code := Main([]string{"run", "img:" + img, "--detach", "--shell"}); code != exitUsage {
		t.Errorf("exit %d, want %d", code, exitUsage)
	}
}

func TestStdinPromptReadsLines(t *testing.T) {
	r, w, err := osPipe()
	if err != nil {
		t.Fatal(err)
	}

	defer r.Close()

	go func() {
		_, _ = w.WriteString("first\nsecond\n")
		_ = w.Close()
	}()

	restore := swapStdin(r)
	defer restore()

	prompt := stdinPrompt()

	for _, want := range []string{"first", "second"} {
		got, err := prompt("Passphrase: ")
		if err != nil {
			t.Fatal(err)
		}

		if string(got) != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}

	// Asking for more than was given must fail, not block.
	if _, err := prompt("Passphrase: "); err == nil {
		t.Error("expected an error once the pipe is drained")
	} else if !strings.Contains(err.Error(), "was not given") {
		t.Errorf("unhelpful error: %v", err)
	}
}

func TestMountArgs(t *testing.T) {
	opts := globalOptions{
		dataDir:      "/tmp/data",
		memoryMiB:    512,
		bootTimeout:  time.Minute,
		setupTimeout: 2 * time.Minute,
	}

	spec := mountSpec{provider: providerQEMU, shareName: "sftp"}
	spec.target.Kind = 1 // target.Image
	spec.target.Path = "/tmp/disk.img"

	args := mountArgs(opts, spec, mountChoice{Device: "vdb1", FSType: "ext4", Options: "ro"})
	joined := strings.Join(args, " ")

	for _, want := range []string{
		"run --detach img:/tmp/disk.img vdb1 ext4",
		"--share sftp", "--provider qemu", "--data-dir /tmp/data",
		"--boot-timeout 1m0s", "--setup-timeout 2m0s", "--mount-options ro",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("args %q lack %q", joined, want)
		}
	}

	// An unset --memory must not be passed on: naming it would switch off the
	// automatic rise that a LUKS mount needs.
	if strings.Contains(joined, "--memory") {
		t.Errorf("args name --memory although it was never set: %q", joined)
	}

	opts.memorySet = true
	opts.memoryMiB = 4096

	if got := strings.Join(mountArgs(opts, spec, mountChoice{Device: "vdb1"}), " "); !strings.Contains(got, "--memory 4096") {
		t.Errorf("an explicit --memory is not passed on: %q", got)
	}

	// No fstype means no third positional argument.
	got := mountArgs(opts, spec, mountChoice{Device: "vdb1"})
	if len(got) > 4 && got[4] != "--share" {
		t.Errorf("an empty fstype became a positional argument: %q", got)
	}

	// LUKS flags.
	luks := strings.Join(mountArgs(opts, spec, mountChoice{Device: "vdb", LUKS: true}), " ")
	if !strings.Contains(luks, "--luks") {
		t.Errorf("--luks missing: %q", luks)
	}

	// Read-only is a property of the spec, chosen before the guest booted.
	if strings.Contains(joined, "--read-only") {
		t.Errorf("--read-only passed although the spec is read-write: %q", joined)
	}

	roSpec := spec
	roSpec.readOnly = true
	if ro := strings.Join(mountArgs(opts, roSpec, mountChoice{Device: "vdb1"}), " "); !strings.Contains(ro, "--read-only") {
		t.Errorf("--read-only missing: %q", ro)
	}

	spec.luksEntire = true

	if got := strings.Join(mountArgs(opts, spec, mountChoice{Device: "vdb"}), " "); !strings.Contains(got, "--luks-container-entire-drive") {
		t.Errorf("the whole-drive container flag is missing: %q", got)
	}

	spec.luksEntire = false
	spec.luksContainer = "vdb3"

	if got := strings.Join(mountArgs(opts, spec, mountChoice{Device: "mapper/vg-root"}), " "); !strings.Contains(got, "--luks-container vdb3") {
		t.Errorf("the container flag is missing: %q", got)
	}

	// A chosen share password is announced, but never written on the command
	// line: the child reads it from stdin.
	spec.sharePassword = []byte("hunter2")

	withPass := strings.Join(mountArgs(opts, spec, mountChoice{Device: "vdb1"}), " ")
	if !strings.Contains(withPass, "--ask-share-password") {
		t.Errorf("--ask-share-password missing: %q", withPass)
	}

	if strings.Contains(withPass, "hunter2") {
		t.Errorf("the password is on the command line: %q", withPass)
	}
}
