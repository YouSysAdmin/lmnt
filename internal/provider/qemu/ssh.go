package qemu

import (
	"cmp"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yousysadmin/lmnt/internal/guest"
	"golang.org/x/crypto/ssh"
)

// sessionKey is the throwaway ed25519 key pair lmnt authenticates the guest
// session with. It lives only as long as the VM, so there is nothing to gain
// from a heavier algorithm.
type sessionKey struct {
	signer     ssh.Signer
	authorized string // the public half in authorized_keys format
}

func newSessionKey() (sessionKey, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return sessionKey{}, fmt.Errorf("generate session key: %w", err)
	}

	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		return sessionKey{}, fmt.Errorf("wrap session key: %w", err)
	}

	return sessionKey{
		signer:     signer,
		authorized: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))),
	}, nil
}

// newToken returns a random hex string used to fence serial output.
func newToken() string {
	var b [8]byte
	_, _ = rand.Read(b[:])

	return "LMNT" + hex.EncodeToString(b[:])
}

// parseHostKey reads a single public key in authorized_keys form, as printed
// by "cat /etc/ssh/ssh_host_ed25519_key.pub".
func parseHostKey(line string) (ssh.PublicKey, error) {
	key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(line))
	if err != nil {
		return nil, fmt.Errorf("parse guest host key: %w", err)
	}

	return key, nil
}

// sshClientConfig authenticates as root with the session key and pins the
// guest's host key.
func sshClientConfig(key sessionKey, hostKey ssh.PublicKey) *ssh.ClientConfig {
	return &ssh.ClientConfig{
		User: "root",
		Auth: []ssh.AuthMethod{ssh.PublicKeys(key.signer)},
		// Pin the guest's ed25519 host key and insist the server present that
		// same algorithm, so the fixed-key check compares like with like.
		HostKeyCallback:   ssh.FixedHostKey(hostKey),
		HostKeyAlgorithms: []string{ssh.KeyAlgoED25519},
		Timeout:           10 * time.Second,
	}
}

// dialSSH connects to the guest's forwarded sshd, retrying until ctx is done
// because sshd may still be coming up.
func dialSSH(ctx context.Context, port uint16, cfg *ssh.ClientConfig) (*ssh.Client, error) {
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port)))

	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()

	var lastErr error
	for {
		client, err := ssh.Dial("tcp", addr, cfg)
		if err == nil {
			return client, nil
		}
		lastErr = err

		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("dial guest ssh: %w (last attempt: %w)", ctx.Err(), lastErr)
		case <-ticker.C:
		}
	}
}

// sshExecutor implements guest.Executor over the VM's SSH server. Each call
// dials a fresh connection so that cancelling a context can simply close it.
type sshExecutor struct {
	port uint16
	cfg  *ssh.ClientConfig
}

func (e *sshExecutor) dial(ctx context.Context) (*ssh.Client, error) {
	// A short connect budget so a wedged guest does not hang a command
	// forever, the caller's ctx still bounds the command itself.
	dialCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	return dialSSH(dialCtx, e.port, e.cfg)
}

// Run executes script and returns its stdout.
func (e *sshExecutor) Run(ctx context.Context, script string) ([]byte, error) {
	client, err := e.dial(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = client.Close() }()

	sess, err := client.NewSession()
	if err != nil {
		return nil, &guest.CommandError{Script: script, ExitCode: -1, Cause: err}
	}
	defer func() { _ = sess.Close() }()

	var stdout, stderr strings.Builder
	sess.Stdout = &stdout
	sess.Stderr = &stderr

	done := make(chan error, 1)
	go func() { done <- sess.Run(script) }()

	select {
	case <-ctx.Done():
		_ = client.Close()
		<-done
		return nil, &guest.CommandError{Script: script, ExitCode: -1, Stderr: stderr.String(), Cause: ctx.Err()}
	case err := <-done:
		if err != nil {
			return nil, commandError(script, stderr.String(), err)
		}

		return []byte(stdout.String()), nil
	}
}

// Start launches script with the given streams and returns without waiting.
func (e *sshExecutor) Start(ctx context.Context, script string, streams guest.Streams) (guest.Process, error) {
	client, err := e.dial(ctx)
	if err != nil {
		return nil, err
	}

	sess, err := client.NewSession()
	if err != nil {
		_ = client.Close()
		return nil, &guest.CommandError{Script: script, ExitCode: -1, Cause: err}
	}

	sess.Stdin = streams.Stdin
	sess.Stdout = streams.Stdout
	sess.Stderr = streams.Stderr

	if err := sess.Start(script); err != nil {
		_ = sess.Close()
		_ = client.Close()
		return nil, commandError(script, "", err)
	}

	p := &sshProcess{client: client, sess: sess, script: script, done: make(chan struct{})}

	// Cancelling ctx closes the connection, which unblocks Wait.
	stop := context.AfterFunc(ctx, func() { _ = client.Close() })
	p.cancelWatch = stop

	return p, nil
}

// Shell attaches an interactive login shell to the terminal.
func (e *sshExecutor) Shell(ctx context.Context, tty guest.Terminal) error {
	client, err := e.dial(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()

	sess, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("open shell session: %w", err)
	}
	defer func() { _ = sess.Close() }()

	sess.Stdin = tty.Stdin
	sess.Stdout = tty.Stdout
	sess.Stderr = tty.Stderr

	modes := ssh.TerminalModes{ssh.ECHO: 1, ssh.TTY_OP_ISPEED: 14400, ssh.TTY_OP_OSPEED: 14400}
	term := cmp.Or(tty.Term, "xterm-256color")
	if err := sess.RequestPty(term, max(tty.Height, 24), max(tty.Width, 80), modes); err != nil {
		return fmt.Errorf("request pty: %w", err)
	}

	if err := sess.Shell(); err != nil {
		return fmt.Errorf("start shell: %w", err)
	}

	done := make(chan error, 1)
	go func() { done <- sess.Wait() }()

	select {
	case <-ctx.Done():
		return nil
	case err := <-done:
		// The user's shell exiting, with any status, is not our error.
		var exitErr *ssh.ExitError
		if err != nil && !errors.As(err, &exitErr) {
			var missing *ssh.ExitMissingError
			if errors.As(err, &missing) {
				return nil
			}

			return fmt.Errorf("wait for shell: %w", err)
		}

		return nil
	}
}

type sshProcess struct {
	client      *ssh.Client
	sess        *ssh.Session
	script      string
	done        chan struct{}
	doneOnce    sync.Once
	cancelWatch func() bool
}

// Wait blocks until the command exits.
func (p *sshProcess) Wait() error {
	err := p.sess.Wait()

	p.doneOnce.Do(func() { close(p.done) })
	if p.cancelWatch != nil {
		p.cancelWatch()
	}
	_ = p.sess.Close()
	_ = p.client.Close()

	if err != nil {
		return commandError(p.script, "", err)
	}

	return nil
}

// commandError converts an ssh.Run/Wait error into a *guest.CommandError,
// pulling the exit status out of *ssh.ExitError when present.
func commandError(script, stderr string, err error) error {
	var exitErr *ssh.ExitError
	if errors.As(err, &exitErr) {
		return &guest.CommandError{Script: script, ExitCode: exitErr.ExitStatus(), Stderr: stderr}
	}

	return &guest.CommandError{Script: script, ExitCode: -1, Stderr: stderr, Cause: err}
}
