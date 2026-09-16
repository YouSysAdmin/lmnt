package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/yousysadmin/lmnt/internal/datadir"
	"github.com/yousysadmin/lmnt/internal/guest"
	"github.com/yousysadmin/lmnt/internal/provider"
	"github.com/yousysadmin/lmnt/internal/share"
	"github.com/yousysadmin/lmnt/internal/target"
)

// Every mount the TUI starts runs in its own detached process, so quitting
// the TUI is a decision about those mounts rather than the end of them. What
// stays in this process is short-lived: one guest booted only to list the
// devices of a disk, so the wizard can offer a choice.

// tuiRuntime is the part of the TUI that outlives screens. The model holds a
// pointer to it and mutates it only from Update.
type tuiRuntime struct {
	app     *app
	dir     *datadir.Dir
	ctx     context.Context
	program *tea.Program
	appLogs *lineRing

	// spawn starts a background mount and returns its id. tui sets it to
	// startMount, tests replace it so no process is started.
	spawn func(spec mountSpec, choice mountChoice, container, volume []byte) (string, error)

	mu sync.Mutex
	// listing is the wizard's device-listing guest, if one is running.
	listing *listingSession
	// started records the mounts this TUI started, so quitting can offer to
	// stop those and leave everyone else's alone.
	started map[string]time.Time
}

// mountSpec is what the wizard collects before a guest boots.
type mountSpec struct {
	target        target.Target
	provider      string
	shareName     string
	luksContainer string
	luksEntire    bool
	// readOnly attaches the disk read-only, in the listing guest as much as
	// in the mount, so a disk the user wants left alone is never written to.
	readOnly bool
	// sharePassword is the password the share is protected with, empty means
	// the mount generates one. It travels to the background mount through
	// stdin, never on its command line.
	sharePassword []byte
}

// mountChoice is what the wizard collects once the device list is in.
type mountChoice struct {
	Device  string
	FSType  string
	Options string
	LUKS    bool
}

type promptReply struct {
	secret []byte
	err    error
}

var errPromptCancelled = errors.New("passphrase entry cancelled")

// listingSession is a guest booted for one purpose: to say what is on the
// disk. It ends as soon as the list is out, and the mount that follows gets
// a guest of its own in a process that survives this one.
type listingSession struct {
	id     string
	spec   mountSpec
	ctx    context.Context
	cancel context.CancelCauseFunc
	logs   *lineRing

	secret chan promptReply
	done   chan struct{}
	err    error // valid once done is closed

	// container holds the passphrase that opened the LUKS container, so the
	// background mount can reuse it instead of asking again.
	container []byte
}

// Messages from goroutines and timers to Update.
type (
	// devicesMsg carries the device list of a finished listing.
	devicesMsg struct {
		id      string
		devices []guest.BlockDevice
	}
	promptMsg      struct{ id, text string }
	listingDoneMsg struct {
		id  string
		err error
	}
	// mountUpMsg says a background mount published a running share.
	mountUpMsg struct {
		id     string
		record datadir.Session
	}
	mountFailedMsg struct {
		id  string
		err error
	}
	logsChangedMsg struct{ id string }
	registryMsg    struct {
		records []datadir.Session
		err     error
	}
	quitRequestMsg struct{}
	// signalQuitMsg is an outside request (SIGINT/SIGTERM/SIGHUP) to end the
	// program. Background mounts are left running: the TUI does not own them.
	signalQuitMsg struct{}
	clipboardMsg  struct {
		what string
		err  error
	}
	tickMsg         time.Time
	quitDeadlineMsg struct{}
)

// startListing boots a guest that enumerates the devices of spec's target and
// then goes away. The wizard continues from devicesMsg.
func (rt *tuiRuntime) startListing(spec mountSpec) (*listingSession, error) {
	if _, ok := share.Lookup(spec.shareName); !ok {
		return nil, fmt.Errorf("unknown share protocol %q", spec.shareName)
	}

	id, err := datadir.NewSessionID()
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancelCause(rt.ctx)

	ls := &listingSession{
		id:     id,
		spec:   spec,
		ctx:    ctx,
		cancel: cancel,
		logs:   newLineRing(logTailLines),
		secret: make(chan promptReply, 1),
		done:   make(chan struct{}),
	}

	// Its own app copy: its own logger and provider choice, without touching
	// the TUI's options. Debug is off because both providers write outside
	// slog in debug mode.
	sa := *rt.app
	sa.logger = slog.New(slog.NewTextHandler(ls.logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	sa.opts.provider = spec.provider
	sa.opts.debug = false

	luks := luksOptions{container: spec.luksContainer, entireDrive: spec.luksEntire}
	p := rt.program

	s := &session{
		app:      &sa,
		target:   &spec.target,
		luks:     luks.requested(),
		readOnly: spec.readOnly,
		runner:   provider.RunContext,
		prompt: func(text string) ([]byte, error) {
			p.Send(promptMsg{id: id, text: text})

			select {
			case r := <-ls.secret:
				if r.err == nil {
					ls.container = bytesClone(r.secret)
				}

				return r.secret, r.err
			case <-ctx.Done():
				return nil, context.Cause(ctx)
			}
		},
	}

	task := func(ctx context.Context, e *env) error {
		container, err := luks.device(e.DiskDevice)
		if err != nil {
			return err
		}

		if container != "" {
			if err := e.CheckDevice(container); err != nil {
				return err
			}

			if err := e.Guest.OpenLUKSContainer(ctx, container); err != nil {
				return err
			}
		}

		var only []string
		if e.OnlyOwnDisk && e.DiskDevice != "" {
			only = []string{e.DiskDevice}
		}

		devices, err := e.Guest.BlockDevices(ctx, only...)
		if err != nil {
			return err
		}

		p.Send(devicesMsg{id: id, devices: devices})

		// The list is data, the guest has nothing left to do.
		return nil
	}

	rt.mu.Lock()
	rt.listing = ls
	rt.mu.Unlock()

	go func() {
		err := s.run(ctx, task)
		ls.err = err
		close(ls.done)
		p.Send(listingDoneMsg{id: id, err: err})
	}()

	// Forward log writes as messages so the log pane refreshes.
	go func() {
		for {
			select {
			case <-ls.logs.notify:
				p.Send(logsChangedMsg{id: id})
			case <-ls.done:
				return
			}
		}
	}()

	return ls, nil
}

// stop ends a listing guest.
func (ls *listingSession) stop() {
	ls.cancel(provider.ErrInterrupted)
}

// answerPrompt hands a passphrase (or a refusal) to the listing guest.
func (ls *listingSession) answerPrompt(r promptReply) {
	select {
	case ls.secret <- r:
	default:
	}
}

// wipe forgets the cached container passphrase.
func (ls *listingSession) wipe() {
	clear(ls.container)
	ls.container = nil
}

func bytesClone(b []byte) []byte {
	out := make([]byte, len(b))
	copy(out, b)

	return out
}

// startMount launches the mount as a detached process and returns its id. The
// secrets are the ones the wizard already collected, in the order the child
// reads them: the share password first, then the container and the volume
// guest.Mount asks for.
func (rt *tuiRuntime) startMount(spec mountSpec, choice mountChoice, container, volume []byte) (string, error) {
	id, err := datadir.NewSessionID()
	if err != nil {
		return "", err
	}

	var secrets [][]byte
	if len(spec.sharePassword) > 0 {
		secrets = append(secrets, spec.sharePassword)
	}

	if spec.luksContainer != "" || spec.luksEntire {
		secrets = append(secrets, container)
	}

	if choice.LUKS {
		secrets = append(secrets, volume)
	}

	args := mountArgs(rt.app.opts, spec, choice)

	if err := rt.app.spawnDetached(rt.dir, id, args, secrets); err != nil {
		return "", err
	}

	rt.mu.Lock()
	rt.started[id] = time.Now()
	rt.mu.Unlock()

	return id, nil
}

// awaitMount watches a background mount until its share is up or it gives up.
func (rt *tuiRuntime) awaitMount(id string) tea.Cmd {
	return func() tea.Msg {
		// The child bounds its own boot, this only has to outlast it.
		deadline := time.After(rt.app.opts.setupTimeout + rt.app.opts.bootTimeout + time.Minute)

		ticker := time.NewTicker(detachPollInterval)
		defer ticker.Stop()

		// A record appears as soon as the child publishes it, which is before
		// its guest boots, only its absence after that means it died.
		seen := false

		for {
			record, ok, err := findSession(rt.dir, id)
			switch {
			case err != nil:
				return mountFailedMsg{id: id, err: err}
			case ok && record.Mounted:
				return mountUpMsg{id: id, record: record}
			case ok:
				seen = true
			case seen:
				return mountFailedMsg{id: id, err: errors.New("the mount stopped before its share was up")}
			}

			select {
			case <-deadline:
				return mountFailedMsg{id: id, err: errors.New("the mount did not come up in time")}
			case <-rt.ctx.Done():
				return mountFailedMsg{id: id, err: rt.ctx.Err()}
			case <-ticker.C:
			}
		}
	}
}

// startedHere lists the ids of mounts this TUI started that are still
// running.
func (rt *tuiRuntime) startedHere(records []datadir.Session) []string {
	rt.mu.Lock()
	defer rt.mu.Unlock()

	var ids []string
	for _, rec := range records {
		if _, ok := rt.started[rec.ID]; ok {
			ids = append(ids, rec.ID)
		}
	}

	return ids
}

// shutdownListing ends the wizard's listing guest, if one is running. It runs
// after the Bubble Tea program exited, background mounts are untouched.
func (rt *tuiRuntime) shutdownListing(timeout time.Duration) {
	rt.mu.Lock()
	ls := rt.listing
	rt.mu.Unlock()

	if ls == nil {
		return
	}

	ls.stop()

	select {
	case <-ls.done:
	case <-time.After(timeout):
	}
}

// mountLog returns the last lines a background mount wrote.
func (rt *tuiRuntime) mountLog(id string, n int) []string {
	path, err := rt.dir.SessionLogPath(id)
	if err != nil {
		return nil
	}

	raw, err := os.ReadFile(path) //nolint:gosec // path comes from datadir, inside lmnt's own directory
	if err != nil {
		return nil
	}

	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}

	return lines
}

// copyToClipboard puts text on the system clipboard with pbcopy. Best effort.
func copyToClipboard(text string) error {
	candidates := [][]string{{"pbcopy"}}

	for _, c := range candidates {
		if _, err := exec.LookPath(c[0]); err != nil {
			continue
		}

		cmd := exec.Command(c[0], c[1:]...)
		cmd.Stdin = strings.NewReader(text)
		cmd.Stdout = nil
		cmd.Stderr = nil

		if err := cmd.Run(); err != nil {
			return fmt.Errorf("%s: %w", c[0], err)
		}

		return nil
	}

	return errors.New("no clipboard tool found")
}
