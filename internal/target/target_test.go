package target

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestParseImage(t *testing.T) {
	dir := t.TempDir()
	img := filepath.Join(dir, "disk.img")
	if err := os.WriteFile(img, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := Parse("img:" + img)
	if err != nil {
		t.Fatal(err)
	}

	if got.Kind != Image || got.Path != img {
		t.Errorf("got %+v", got)
	}

	if got.String() != "img:"+img {
		t.Errorf("String() = %q", got.String())
	}

	// Relative paths become absolute.
	wd, _ := os.Getwd()
	t.Chdir(dir)
	defer t.Chdir(wd)

	got, err = Parse("IMG:./disk.img")
	if err != nil {
		t.Fatal(err)
	}

	if got.Path != img {
		t.Errorf("relative path resolved to %q", got.Path)
	}

	if _, err := Parse("img:" + dir); err == nil {
		t.Error("directory accepted as image")
	}

	if _, err := Parse("img:" + filepath.Join(dir, "missing")); err == nil {
		t.Error("missing file accepted")
	}
}

func TestParseImageRejectsDevice(t *testing.T) {
	_, err := Parse("img:/dev/null")
	if err == nil {
		t.Fatal("device accepted as image")
	}
}

func TestParseDevice(t *testing.T) {
	got, err := Parse("dev:/dev//disk4/")
	if err != nil {
		t.Fatal(err)
	}

	if got.Kind != Device || got.Path != "/dev/disk4" {
		t.Errorf("got %+v", got)
	}

	if _, err := Parse("dev:disk4"); err == nil {
		t.Error("relative device path accepted")
	}
}

func TestParseUSB(t *testing.T) {
	for _, in := range []string{"usb:0781,5583", "usb:0x0781,0x5583", "USB: 781 , 5583"} {
		got, err := Parse(in)
		if err != nil {
			t.Errorf("Parse(%q): %v", in, err)
			continue
		}

		if got.Kind != USB || got.VendorID != 0x0781 || got.ProductID != 0x5583 {
			t.Errorf("Parse(%q) = %+v", in, got)
		}

		if got.String() != "usb:0781,5583" {
			t.Errorf("String() = %q", got.String())
		}
	}

	for _, in := range []string{"usb:0781", "usb:,", "usb:xyz,1", "usb:10000,1", "usb:1,2,3"} {
		if _, err := Parse(in); !errors.Is(err, ErrSyntax) {
			t.Errorf("Parse(%q) = %v, want ErrSyntax", in, err)
		}
	}
}

func TestParseSyntaxErrors(t *testing.T) {
	for _, in := range []string{"", "img", "img:", "disk.img", "cd:/x", ":x"} {
		if _, err := Parse(in); !errors.Is(err, ErrSyntax) {
			t.Errorf("Parse(%q) = %v, want ErrSyntax", in, err)
		}
	}
}
