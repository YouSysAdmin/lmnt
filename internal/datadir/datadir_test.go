package datadir

import (
	"bytes"
	"compress/bzip2"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func testDir(t *testing.T) *Dir {
	t.Helper()

	d, err := Open(slog.New(slog.NewTextHandler(io.Discard, nil)), filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}

	return d
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func serve(t *testing.T, body []byte) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	return srv
}

func TestDefaultPath(t *testing.T) {
	p := DefaultPath()
	if !filepath.IsAbs(p) {
		t.Fatalf("DefaultPath %q is not absolute", p)
	}

	base := filepath.Base(p)
	if base != ".lmnt" && base != "lmnt" {
		t.Errorf("unexpected base name %q", base)
	}
}

func TestOpenCreatesDirectory(t *testing.T) {
	d := testDir(t)

	info, err := os.Stat(d.Path())
	if err != nil || !info.IsDir() {
		t.Fatalf("data dir not created: %v", err)
	}

	if _, err := Open(slog.Default(), ""); err == nil {
		t.Error("Open(\"\") should fail")
	}
}

func TestVMImagePath(t *testing.T) {
	d := testDir(t)

	name := filepath.Base(d.VMImagePath())
	if !strings.HasPrefix(name, "alpine-"+AlpineVersion+"-") || !strings.HasSuffix(name, ".qcow2") || !strings.Contains(name, "-lmnt") {
		t.Errorf("unexpected image name %q", name)
	}

	ok, err := d.VMImageExists()
	if err != nil || ok {
		t.Fatalf("exists = %v, %v; want false", ok, err)
	}

	if err := os.WriteFile(d.VMImagePath(), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	ok, err = d.VMImageExists()
	if err != nil || !ok {
		t.Fatalf("exists = %v, %v, want true", ok, err)
	}
}

func TestEnsureDownloadsAndVerifies(t *testing.T) {
	d := testDir(t)
	body := bytes.Repeat([]byte("alpine"), 10_000)
	srv := serve(t, body)

	a := asset{Name: "thing.iso", URL: srv.URL + "/thing.iso", SHA256: digestOf(body)}

	path, err := d.ensure(t.Context(), a)
	if err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("stored file differs: %v", err)
	}

	if _, err := os.Stat(path + ".part"); !errors.Is(err, fs.ErrNotExist) {
		t.Error(".part file left behind")
	}

	// A second call must not touch the network: point the URL somewhere dead.
	a.URL = "http://127.0.0.1:1/nope"
	if _, err := d.ensure(t.Context(), a); err != nil {
		t.Fatalf("re-verify of an existing file: %v", err)
	}

	// Corrupt the file: ensure must refuse it rather than silently re-download.
	if err := os.WriteFile(path, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ensure(t.Context(), a); err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("corrupt file accepted: %v", err)
	}
}

func TestEnsureHashMismatchLeavesNothing(t *testing.T) {
	d := testDir(t)
	srv := serve(t, []byte("not what you expected"))

	a := asset{Name: "bad.iso", URL: srv.URL, SHA256: digestOf([]byte("expected"))}

	_, err := d.ensure(t.Context(), a)
	if err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("got %v, want mismatch error", err)
	}

	entries, _ := os.ReadDir(d.Path())
	if len(entries) != 0 {
		t.Errorf("directory not clean after failed download: %v", entries)
	}
}

func TestEnsureDiscardsStalePartial(t *testing.T) {
	d := testDir(t)
	body := []byte("fresh")
	srv := serve(t, body)

	stale := filepath.Join(d.Path(), "f.bin.part")
	if err := os.WriteFile(stale, []byte("half"), 0o600); err != nil {
		t.Fatal(err)
	}

	path, err := d.ensure(t.Context(), asset{Name: "f.bin", URL: srv.URL, SHA256: digestOf(body)})
	if err != nil {
		t.Fatal(err)
	}

	got, _ := os.ReadFile(path)
	if !bytes.Equal(got, body) {
		t.Errorf("got %q", got)
	}
}

func TestEnsureBadStatus(t *testing.T) {
	d := testDir(t)
	srv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)

	_, err := d.ensure(t.Context(), asset{Name: "x", URL: srv.URL, SHA256: digestOf(nil)})
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("got %v", err)
	}
}

func TestEnsureDecodes(t *testing.T) {
	d := testDir(t)

	// Go has no bzip2 writer, use a tiny pre-compressed fixture of "hello\n".
	compressed, err := hex.DecodeString("425a6839314159265359c1c080e2000001410000100244a00030cd00c3462997177245385090c1c080e2")
	if err != nil {
		t.Fatal(err)
	}

	plain, err := io.ReadAll(bzip2.NewReader(bytes.NewReader(compressed)))
	if err != nil {
		t.Fatalf("fixture does not decompress: %v", err)
	}
	if string(plain) != "hello\n" {
		t.Fatalf("fixture decodes to %q", plain)
	}

	srv := serve(t, compressed)

	path, err := d.ensure(t.Context(), asset{Name: "fw.fd", URL: srv.URL, SHA256: digestOf(plain), Decode: bzip2.NewReader})
	if err != nil {
		t.Fatal(err)
	}

	got, _ := os.ReadFile(path)
	if string(got) != "hello\n" {
		t.Errorf("stored %q, want decompressed content", got)
	}
}

func TestEnsureFirmwareOnlyOnArm64(t *testing.T) {
	d := testDir(t)

	// A cancelled context guarantees the test never reaches the network: on
	// arm64 the download must fail fast, elsewhere no download is attempted.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	path, err := d.EnsureFirmware(ctx)
	switch runtime.GOARCH {
	case "arm64":
		if err == nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("got %q, %v, want a cancellation error", path, err)
		}
	default:
		if err != nil || path != "" {
			t.Fatalf("got %q, %v, want no firmware", path, err)
		}
	}
}

func TestRemoveStaysInside(t *testing.T) {
	d := testDir(t)

	inside := filepath.Join(d.Path(), "a.txt")
	if err := os.WriteFile(inside, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	outside := filepath.Join(filepath.Dir(d.Path()), "victim.txt")
	if err := os.WriteFile(outside, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, bad := range []string{outside, "../victim.txt", "..", "", "."} {
		if err := d.Remove(bad); err == nil {
			t.Errorf("Remove(%q) succeeded", bad)
		}
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatal("file outside the data dir was removed")
	}

	if err := d.Remove("a.txt"); err != nil {
		t.Fatal(err)
	}
	if err := d.Remove(inside); err != nil {
		t.Errorf("removing a missing file by absolute path: %v", err)
	}

	if err := d.RemoveAll(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(d.Path()); !errors.Is(err, fs.ErrNotExist) {
		t.Error("RemoveAll left the directory")
	}
}

func TestHumanSize(t *testing.T) {
	cases := map[int64]string{
		0:              "0 B",
		512:            "512 B",
		1024:           "1.0 KiB",
		1536:           "1.5 KiB",
		12_897_484:     "12.3 MiB",
		3 << 30:        "3.0 GiB",
		5 << 40:        "5.0 TiB",
		1 << 50:        "1024.0 TiB",
		60_000_000_000: "55.9 GiB",
	}

	for n, want := range cases {
		if got := HumanSize(n); got != want {
			t.Errorf("HumanSize(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestAssetsAreWellFormed(t *testing.T) {
	for _, a := range []asset{alpineISO(), aarch64Firmware} {
		if _, err := hex.DecodeString(a.SHA256); err != nil || len(a.SHA256) != 64 {
			t.Errorf("%s: bad digest %q", a.Name, a.SHA256)
		}
		if !strings.HasPrefix(a.URL, "https://") {
			t.Errorf("%s: URL %q is not https", a.Name, a.URL)
		}
	}
}
