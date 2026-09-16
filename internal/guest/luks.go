package guest

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

// PasswordPrompt asks the user for a secret and returns it. The caller
// wipes the returned bytes when done.
type PasswordPrompt func(prompt string) ([]byte, error)

// TerminalPrompt reads a password from the controlling terminal without
// echo, writing the prompt to stderr.
func TerminalPrompt(prompt string) ([]byte, error) {
	fmt.Fprint(os.Stderr, prompt)
	defer fmt.Fprintln(os.Stderr)

	secret, err := term.ReadPassword(int(os.Stdin.Fd()))
	if err != nil {
		return nil, fmt.Errorf("read password from terminal: %w", err)
	}

	return secret, nil
}

// ErrNoPrompt is returned when a LUKS device must be opened but no
// PasswordPrompt was supplied.
var ErrNoPrompt = errors.New("a passphrase is required but no prompt is available")

// OpenLUKS unlocks /dev/<device> as /dev/mapper/<mapping>, asking prompt
// (or g.Prompt when prompt is nil) for the passphrase. The time budget starts
// only once the passphrase has been entered, so a slow typist is never timed
// out.
func (g *Guest) OpenLUKS(ctx context.Context, device, mapping string, prompt PasswordPrompt) error {
	return g.openLUKS(ctx, device, mapping, prompt, false)
}

// openLUKS is OpenLUKS with a read-only switch: cryptsetup --readonly makes
// the mapping refuse writes even when the device underneath would take them.
func (g *Guest) openLUKS(ctx context.Context, device, mapping string, prompt PasswordPrompt, readOnly bool) error {
	dev, err := devicePath(device)
	if err != nil {
		return fmt.Errorf("open luks: %w", err)
	}

	if err := checkMappingName(mapping); err != nil {
		return fmt.Errorf("open luks: %w", err)
	}

	if prompt == nil {
		prompt = g.Prompt
	}

	if prompt == nil {
		return fmt.Errorf("open luks %s: %w", dev, ErrNoPrompt)
	}

	g.logger.Info("Unlocking LUKS device", "device", dev, "mapping", mapping, "read-only", readOnly)

	secret, err := prompt(fmt.Sprintf("Passphrase for %s: ", dev))
	if err != nil {
		return fmt.Errorf("open luks %s: %w", dev, err)
	}
	defer scrub(secret)

	ctx, cancel := context.WithTimeout(ctx, g.LUKSTimeout)
	defer cancel()

	args := []string{"cryptsetup", "open", "--type", "luks"}
	if readOnly {
		args = append(args, "--readonly")
	}

	script := QuoteAll(append(args, dev, mapping)...)
	stdin := io.MultiReader(bytes.NewReader(secret), strings.NewReader("\n"))

	err = feed(ctx, g.ex, script, stdin)
	if err == nil {

		g.logger.Info("LUKS device unlocked", "mapping", "/dev/mapper/"+mapping)

		return nil
	}

	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		err = fmt.Errorf("%w (key derivation did not finish in %v, a memory-hard KDF may need more guest memory, see --memory)", err, g.LUKSTimeout)
	case mentionsLowMemory(err):
		err = fmt.Errorf("%w (the guest has too little memory for this key slot, raise it with --memory)", err)
	}

	return fmt.Errorf("open luks %s: %w", dev, err)
}

// formatLUKS writes a fresh LUKS2 header onto dev, keyed by secret. The
// device's previous contents become unreachable.
//
// --batch-mode drops both the "YES" confirmation and the verification
// prompt, so cryptsetup reads the passphrase from stdin exactly once, the
// way `cryptsetup open` does.
func (g *Guest) formatLUKS(ctx context.Context, dev string, secret []byte) error {
	g.logger.Info("Creating a LUKS2 container", "device", dev)

	// The budget starts here, not at the caller: key derivation is the slow
	// part and the passphrase is already in hand.
	ctx, cancel := context.WithTimeout(ctx, g.LUKSTimeout)
	defer cancel()

	script := QuoteAll("cryptsetup", "luksFormat", "--type", "luks2", "--batch-mode", dev)
	stdin := io.MultiReader(bytes.NewReader(secret), strings.NewReader("\n"))

	err := feed(ctx, g.ex, script, stdin)
	if err == nil {
		return nil
	}

	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		err = fmt.Errorf("%w (key derivation did not finish in %v, a memory-hard KDF may need more guest memory, see --memory)", err, g.LUKSTimeout)
	case mentionsLowMemory(err):
		err = fmt.Errorf("%w (the guest has too little memory to derive a key, raise it with --memory)", err)
	}

	return fmt.Errorf("format luks %s: %w", dev, err)
}

// CloseLUKS removes a mapping created by OpenLUKS, flushing the volume's
// writes to the underlying device.
func (g *Guest) CloseLUKS(ctx context.Context, mapping string) error {
	if err := checkMappingName(mapping); err != nil {
		return fmt.Errorf("close luks: %w", err)
	}

	if _, err := g.Run(ctx, QuoteAll("cryptsetup", "close", mapping)); err != nil {
		return fmt.Errorf("close luks %s: %w", mapping, err)
	}

	return nil
}

func mentionsLowMemory(err error) bool {
	ce, ok := errors.AsType[*CommandError](err)

	return ok && strings.Contains(strings.ToLower(ce.Stderr), "not enough available memory")
}

// scrub overwrites secret material with random bytes and then zeros.
func scrub(b []byte) {
	_, _ = rand.Read(b)
	clear(b)
}
