package guest

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestFormatPlain(t *testing.T) {
	g, f := newTestGuest(t)

	err := g.Format(t.Context(), FormatRequest{Device: "vdb", FSType: "ext4", Label: "data"})
	if err != nil {
		t.Fatal(err)
	}

	want := []string{"mkfs.ext4 -q -F -L data /dev/vdb", "sync"}
	if !slices.Equal(f.scripts, want) {
		t.Errorf("scripts: %q, want %q", f.scripts, want)
	}
}

func TestFormatLabelFlagPerFS(t *testing.T) {
	cases := map[string]string{
		"xfs":   "mkfs.xfs -q -f -L disk /dev/vdb",
		"btrfs": "mkfs.btrfs -q -f -L disk /dev/vdb",
		"f2fs":  "mkfs.f2fs -q -f -l disk /dev/vdb",
		"vfat":  "mkfs.vfat -n disk /dev/vdb",
	}

	for fs, want := range cases {
		g, f := newTestGuest(t)

		if err := g.Format(t.Context(), FormatRequest{Device: "vdb", FSType: fs, Label: "disk"}); err != nil {
			t.Fatalf("%s: %v", fs, err)
		}

		if got := f.scripts[0]; got != want {
			t.Errorf("%s: %q, want %q", fs, got, want)
		}
	}
}

func TestFormatWithoutLabel(t *testing.T) {
	g, f := newTestGuest(t)

	if err := g.Format(t.Context(), FormatRequest{Device: "loop0", FSType: "ext4"}); err != nil {
		t.Fatal(err)
	}

	if got := f.scripts[0]; got != "mkfs.ext4 -q -F /dev/loop0" {
		t.Errorf("script: %q", got)
	}
}

func TestFormatLUKS(t *testing.T) {
	g, f := newTestGuest(t)

	asked := 0
	g.Prompt = func(string) ([]byte, error) {
		asked++
		return []byte("s3cret"), nil
	}

	err := g.Format(t.Context(), FormatRequest{Device: "vdb", FSType: "ext4", LUKS: true})
	if err != nil {
		t.Fatal(err)
	}

	if asked != 1 {
		t.Errorf("passphrase asked %d times, want 1", asked)
	}

	want := []string{
		"cryptsetup luksFormat --type luks2 --batch-mode /dev/vdb",
		"cryptsetup open --type luks /dev/vdb cryptmnt",
		"mkfs.ext4 -q -F /dev/mapper/cryptmnt",
		"sync",
		"cryptsetup close cryptmnt",
	}
	if !slices.Equal(f.scripts, want) {
		t.Errorf("scripts: %q, want %q", f.scripts, want)
	}

	// Both cryptsetup calls get the same passphrase on stdin.
	for i := range 2 {
		if f.stdins[i] != "s3cret\n" {
			t.Errorf("stdin %d: %q", i, f.stdins[i])
		}
	}
}

func TestFormatLUKSClosesAfterMkfsFailure(t *testing.T) {
	g, f := newTestGuest(t)
	g.Prompt = func(string) ([]byte, error) { return []byte("s3cret"), nil }
	f.respond("mkfs.ext4", fakeResponse{exit: 1, stderr: "no space left on device"})

	err := g.Format(t.Context(), FormatRequest{Device: "vdb", FSType: "ext4", LUKS: true})
	if err == nil {
		t.Fatal("expected an error")
	}

	if got := lastScript(t, f); got != "cryptsetup close cryptmnt" {
		t.Errorf("last script: %q, want the mapping to be closed", got)
	}
}

func TestFormatRejects(t *testing.T) {
	cases := map[string]FormatRequest{
		"bad device": {Device: "../etc", FSType: "ext4"},
		"bad fstype": {Device: "vdb", FSType: "ext 4"},
		"no mkfs":    {Device: "vdb", FSType: "zfs"},
		"bad label":  {Device: "vdb", FSType: "ext4", Label: "a;rm -rf /"},
		"empty fs":   {Device: "vdb"},
	}

	for name, req := range cases {
		g, f := newTestGuest(t)

		if err := g.Format(t.Context(), req); err == nil {
			t.Errorf("%s: expected an error", name)
		}

		if len(f.scripts) != 0 {
			t.Errorf("%s: ran %q despite bad input", name, f.scripts)
		}
	}
}

func TestFormatLUKSNeedsPrompt(t *testing.T) {
	g, _ := newTestGuest(t)

	err := g.Format(t.Context(), FormatRequest{Device: "vdb", FSType: "ext4", LUKS: true})
	if !errors.Is(err, ErrNoPrompt) {
		t.Errorf("error: %v, want ErrNoPrompt", err)
	}
}

func TestFormatLUKSLowMemoryHint(t *testing.T) {
	g, f := newTestGuest(t)
	g.Prompt = func(string) ([]byte, error) { return []byte("s3cret"), nil }
	f.respond("luksFormat", fakeResponse{exit: 1, stderr: "Not enough available memory to open a keyslot."})

	err := g.Format(t.Context(), FormatRequest{Device: "vdb", FSType: "ext4", LUKS: true})
	if err == nil || !strings.Contains(err.Error(), "--memory") {
		t.Errorf("error: %v, want a hint about --memory", err)
	}
}

func TestFSTypes(t *testing.T) {
	types := FSTypes()

	if !slices.IsSorted(types) {
		t.Errorf("FSTypes is not sorted: %q", types)
	}

	if !slices.Contains(types, "ext4") {
		t.Errorf("FSTypes lacks ext4: %q", types)
	}
}

func TestCloseLUKSRejectsBadMapping(t *testing.T) {
	g, f := newTestGuest(t)

	if err := g.CloseLUKS(t.Context(), "crypt mnt"); err == nil {
		t.Error("expected an error")
	}

	if len(f.scripts) != 0 {
		t.Errorf("ran %q despite a bad mapping name", f.scripts)
	}
}
