package datadir

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"time"
)

// ensure returns the path of a verified copy of a, downloading it if absent.
func (d *Dir) ensure(ctx context.Context, a asset) (string, error) {
	path, err := d.join(a.Name)
	if err != nil {
		return "", err
	}

	want, err := hex.DecodeString(a.SHA256)
	if err != nil || len(want) != sha256.Size {
		return "", fmt.Errorf("asset %s has a malformed pinned digest", a.Name)
	}

	_, err = os.Stat(path)
	switch {
	case err == nil:
		if err := verifyFile(path, want); err != nil {
			return "", fmt.Errorf("%s is present but corrupt (delete it to re-download): %w", path, err)
		}

		return path, nil
	case !errors.Is(err, fs.ErrNotExist):
		return "", fmt.Errorf("stat %s: %w", path, err)
	}

	if err := d.fetch(ctx, a, path, want); err != nil {
		return "", fmt.Errorf("download %s: %w", a.Name, err)
	}

	return path, nil
}

// fetch downloads a into dst. The body is streamed into dst+".part" while
// being hashed, only when the digest matches is the file renamed to dst.
func (d *Dir) fetch(ctx context.Context, a asset, dst string, want []byte) error {
	part := dst + ".part"

	// A partial file from an earlier attempt cannot be resumed (the hash
	// must cover the whole stream), so start over.
	if err := os.Remove(part); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("discard stale partial file: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}

	d.logger.Info("Downloading", "url", a.URL, "to", dst)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("get: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("get: server answered %s", resp.Status)
	}

	f, err := os.OpenFile(part, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // path is inside the data dir by construction
	if err != nil {
		return fmt.Errorf("create partial file: %w", err)
	}

	keep := false
	defer func() {
		_ = f.Close()
		if !keep {
			_ = os.Remove(part)
		}
	}()

	var body io.Reader = resp.Body
	// A decoded stream has no relation to Content-Length, so the percentage
	// is only known for raw downloads.
	total := resp.ContentLength
	if a.Decode != nil {
		body = a.Decode(resp.Body)
		total = -1
	}

	digest := sha256.New()
	meter := &progressMeter{
		every: time.Second,
		last:  time.Now(),
		report: func(n int64) {
			if total > 0 {
				d.logger.Info("Downloading", "file", a.Name, "done", HumanSize(n), "percent", n*100/total)
			} else {
				d.logger.Info("Downloading", "file", a.Name, "done", HumanSize(n))
			}
		},
	}

	n, err := io.Copy(io.MultiWriter(f, digest, meter), body)
	if err != nil {
		return fmt.Errorf("receive body: %w", err)
	}
	meter.report(n)

	if got := digest.Sum(nil); !bytes.Equal(got, want) {
		return fmt.Errorf("sha256 mismatch: got %x, want %x", got, want)
	}

	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync partial file: %w", err)
	}

	// Close before rename, so the file is complete when it appears under its name.
	if err := f.Close(); err != nil {
		return fmt.Errorf("close partial file: %w", err)
	}

	if err := os.Rename(part, dst); err != nil {
		return fmt.Errorf("move into place: %w", err)
	}

	keep = true
	d.logger.Info("Downloaded", "file", a.Name, "size", HumanSize(n))

	return nil
}

// verifyFile hashes the file at path and compares it to want.
func verifyFile(path string, want []byte) error {
	f, err := os.Open(path) //nolint:gosec // path is inside the data dir by construction
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer func() { _ = f.Close() }()

	digest := sha256.New()
	if _, err := io.Copy(digest, f); err != nil {
		return fmt.Errorf("read: %w", err)
	}

	if got := digest.Sum(nil); !bytes.Equal(got, want) {
		return fmt.Errorf("sha256 mismatch: got %x, want %x", got, want)
	}

	return nil
}

// progressMeter counts bytes written through it and calls report at most
// once per interval.
type progressMeter struct {
	n      int64
	every  time.Duration
	last   time.Time
	report func(int64)
}

func (m *progressMeter) Write(p []byte) (int, error) {
	m.n += int64(len(p))

	if now := time.Now(); now.Sub(m.last) >= m.every {
		m.last = now
		m.report(m.n)
	}

	return len(p), nil
}

// HumanSize formats a byte count with a binary unit, e.g. "12.3 MiB".
func HumanSize(n int64) string {
	const unit = 1024

	if n < unit {
		return fmt.Sprintf("%d B", n)
	}

	value := float64(n)
	suffixes := []string{"KiB", "MiB", "GiB", "TiB"}
	i := -1
	for value >= unit && i < len(suffixes)-1 {
		value /= unit
		i++
	}

	return fmt.Sprintf("%.1f %s", value, suffixes[i])
}
