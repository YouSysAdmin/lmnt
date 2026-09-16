package docker

import (
	"context"
	"errors"
	"net/netip"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/yousysadmin/lmnt/internal/guest"
	"github.com/yousysadmin/lmnt/internal/netx"
)

func TestImageRef(t *testing.T) {
	ref := ImageRef()
	if !regexp.MustCompile(`^lmnt-guest:[0-9a-f]{12}$`).MatchString(ref) {
		t.Errorf("unexpected image ref %q", ref)
	}
	if ref != ImageRef() {
		t.Error("ImageRef is not stable")
	}
}

func TestRunArgs(t *testing.T) {
	cfg := Config{
		ImagePath: "/tmp/disk.img",
		Forwards: []netx.Forward{
			{Host: netip.MustParseAddrPort("127.0.0.1:9000"), GuestPort: 445},
			{Host: netip.MustParseAddrPort("[::1]:9001"), GuestPort: 21},
		},
	}

	args := runArgs("lmnt-abc", cfg, "lmnt-guest:deadbeef0000")
	joined := strings.Join(args, " ")

	for _, want := range []string{
		"run --detach --rm --privileged",
		"--name lmnt-abc",
		"--label app=lmnt",
		"--mount type=bind,source=/tmp/disk.img,target=/lmnt/disk.img",
		"--publish 127.0.0.1:9000:445",
		"--publish ::1:9001:21",
		"lmnt-guest:deadbeef0000 sh -c",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("args lack %q:\n%s", want, joined)
		}
	}

	if !strings.Contains(args[len(args)-1], "softlevel") {
		t.Error("init script must create the OpenRC softlevel marker")
	}

	plain := runArgs("x", Config{}, "img")
	if slices.Contains(plain, "--mount") || slices.Contains(plain, "--publish") {
		t.Errorf("no image and no forwards should yield no mount/publish: %v", plain)
	}
}

func TestReadOnlyAttach(t *testing.T) {
	ro := strings.Join(runArgs("x", Config{ImagePath: "/tmp/disk.img", ReadOnly: true}, "img"), " ")
	if !strings.Contains(ro, "--mount type=bind,source=/tmp/disk.img,target=/lmnt/disk.img,readonly") {
		t.Errorf("a read-only image is not bound read-only: %s", ro)
	}

	if !strings.Contains(attachScript(true), "losetup --read-only --find --partscan --show /lmnt/disk.img") {
		t.Errorf("read-only attach script: %s", attachScript(true))
	}

	if strings.Contains(attachScript(false), "--read-only") {
		t.Errorf("writable attach script must not pass --read-only: %s", attachScript(false))
	}
}

func TestParseLoopName(t *testing.T) {
	good := map[string]string{"loop0\n": "loop0", "  loop12 \n": "loop12"}
	for in, want := range good {
		got, err := parseLoopName([]byte(in))
		if err != nil || got != want {
			t.Errorf("parseLoopName(%q) = %q, %v", in, got, err)
		}
	}

	for _, in := range []string{"", "/dev/loop0", "loop", "loop0; rm -rf /", "sda"} {
		if got, err := parseLoopName([]byte(in)); err == nil {
			t.Errorf("parseLoopName(%q) = %q, want error", in, got)
		}
	}
}

func TestTeardownScript(t *testing.T) {
	s := teardownScript("loop3")
	for _, want := range []string{"umount /mnt", "cryptsetup close cryptmnt", "vgchange -an", "cryptsetup close cryptcontainer", "losetup -d /dev/loop3"} {
		if !strings.Contains(s, want) {
			t.Errorf("script lacks %q", want)
		}
	}
	if !strings.HasSuffix(strings.TrimSpace(s), "exit 0") {
		t.Error("teardown must always succeed")
	}

	// Order: innermost first.
	if strings.Index(s, "umount") > strings.Index(s, "losetup") {
		t.Error("loop device must be detached last")
	}

	if strings.Contains(teardownScript(""), "losetup") {
		t.Error("no loop device, no detach")
	}
	if strings.Contains(teardownScript("loop0 && evil"), "evil") {
		t.Error("unvalidated loop name reached the script")
	}
}

func TestNewRejectsBadImages(t *testing.T) {
	if _, err := New(nil, Config{ImagePath: t.TempDir()}); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("directory accepted: %v", err)
	}
	if _, err := New(nil, Config{ImagePath: "/nonexistent/lmnt.img"}); err == nil {
		t.Error("missing file accepted")
	}

	c, err := New(nil, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(c.Name(), "lmnt-") || len(c.Name()) != len("lmnt-")+12 {
		t.Errorf("unexpected container name %q", c.Name())
	}
	if c.DiskDevice() != "" {
		t.Error("no image, no device")
	}
}

func TestStopBeforeStart(t *testing.T) {
	c, err := New(nil, Config{})
	if err != nil {
		t.Fatal(err)
	}

	if err := c.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := c.Stop(t.Context()); err != nil {
		t.Fatal("second Stop:", err)
	}

	select {
	case <-c.Exited():
	default:
		t.Fatal("Exited not closed after Stop")
	}
	if c.Err() != nil {
		t.Errorf("Err after a requested stop: %v", c.Err())
	}
}

func TestCommandError(t *testing.T) {
	exitErr := exec.Command("sh", "-c", "exit 3").Run()
	if exitErr == nil {
		t.Skip("no sh available")
	}

	err := commandError(t.Context(), "exit 3", exitErr, "boom\n")
	if guest.ExitCode(err) != 3 {
		t.Errorf("exit code %d, want 3", guest.ExitCode(err))
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("stderr missing from %q", err.Error())
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err = commandError(ctx, "sleep", exitErr, "")
	if !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled command error does not match ctx.Err(): %v", err)
	}
	if guest.ExitCode(err) != -1 {
		t.Errorf("exit code after cancellation should be -1, got %d", guest.ExitCode(err))
	}
}

func TestTail(t *testing.T) {
	tl := newTail(4)
	for _, chunk := range []string{"ab", "cd", "ef"} {
		if _, err := tl.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	if tl.String() != "cdef" {
		t.Errorf("got %q", tl.String())
	}
}
