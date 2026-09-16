package qemu

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestSerialLoginDetection(t *testing.T) {
	pr, pw := io.Pipe()
	c := newSerialConsole(io.Discard, pr)

	go func() {
		_, _ = io.WriteString(pw, "Welcome to Alpine\r\nboot messages\r\nlocalhost login:")
	}()

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	if err := c.awaitLogin(ctx); err != nil {
		t.Fatalf("login not detected: %v", err)
	}
}

func TestSerialRunScript(t *testing.T) {
	guestStdinR, guestStdinW := io.Pipe()   // host writes here, guest reads
	guestStdoutR, guestStdoutW := io.Pipe() // guest writes here, host reads
	c := newSerialConsole(guestStdinW, guestStdoutR)

	go fakeGuest(guestStdinR, guestStdoutW)

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()

	out, code, err := c.runScript(ctx, "echo hello")
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 {
		t.Errorf("exit code = %d", code)
	}
	if len(out) != 1 || out[0] != "hello" {
		t.Errorf("output = %q", out)
	}
}

// fakeGuest reads one wrapped script, extracts its fence token, and replies
// as a shell would: BEGIN marker, payload line, END marker with status 0.
func fakeGuest(in io.Reader, out io.Writer) {
	var acc strings.Builder
	buf := make([]byte, 512)
	for {
		n, err := in.Read(buf)
		if n > 0 {
			acc.WriteString(string(buf[:n]))
			if token, ok := tokenOf(acc.String()); ok {
				reply := token + "-BEGIN\n" + "hello\n" + token + "-END:0\n"
				_, _ = io.WriteString(out, reply)
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// tokenOf pulls the fence token out of a wrapper once its first line is seen.
func tokenOf(s string) (string, bool) {
	const prefix = "echo "
	const marker = "-BEGIN"
	i := strings.Index(s, prefix)
	j := strings.Index(s, marker)
	if i < 0 || j < 0 || j < i+len(prefix) {
		return "", false
	}

	return s[i+len(prefix) : j], true
}

func TestParseStatus(t *testing.T) {
	ok := []struct {
		in   string
		want int
	}{{"0", 0}, {"1", 1}, {"32", 32}, {" 5 ", 5}}
	for _, c := range ok {
		got, err := parseStatus(c.in)
		if err != nil || got != c.want {
			t.Errorf("parseStatus(%q) = %d, %v", c.in, got, err)
		}
	}
	for _, in := range []string{"", "x", "1a", "-1"} {
		if _, err := parseStatus(in); err == nil {
			t.Errorf("parseStatus(%q) accepted", in)
		}
	}
}

func TestParseHostKey(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	line := string(ssh.MarshalAuthorizedKey(signer.PublicKey()))

	key, err := parseHostKey(strings.TrimSpace(line))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(key.Marshal(), signer.PublicKey().Marshal()) {
		t.Error("round-tripped host key differs")
	}

	if _, err := parseHostKey("not a key"); err == nil {
		t.Error("garbage accepted as a host key")
	}
}
