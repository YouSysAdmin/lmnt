package cli

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/yousysadmin/lmnt/internal/datadir"
	"github.com/yousysadmin/lmnt/internal/guest"
	"github.com/yousysadmin/lmnt/internal/provider"
)

func TestLineRing(t *testing.T) {
	r := newLineRing(3)
	for _, l := range []string{"a\n", "b\n", "c\n", "d\n"} {
		if _, err := r.Write([]byte(l)); err != nil {
			t.Fatal(err)
		}
	}

	if got := r.Snapshot(); !slices.Equal(got, []string{"b", "c", "d"}) {
		t.Errorf("Snapshot = %q", got)
	}
	if got := r.Tail(2); !slices.Equal(got, []string{"c", "d"}) {
		t.Errorf("Tail = %q", got)
	}

	select {
	case <-r.notify:
	default:
		t.Error("no notification after writes")
	}
}

func testRuntime(t *testing.T) *tuiRuntime {
	t.Helper()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	dir, err := datadir.Open(logger, filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}

	a := &app{logger: logger, opts: globalOptions{provider: providerQEMU, memoryMiB: defaultMemoryMiB}}

	rt := &tuiRuntime{app: a, dir: dir, ctx: t.Context(), appLogs: newLineRing(10), started: map[string]time.Time{}}

	// No test may start a real background process.
	rt.spawn = func(mountSpec, mountChoice, []byte, []byte) (string, error) {
		return "", errors.New("spawn not stubbed in this test")
	}

	return rt
}

// fakeMount registers a running mount as if a background process had
// published it, optionally marked as started by this TUI.
func fakeMount(rt *tuiRuntime, id string, here bool) datadir.Session {
	rec := datadir.Session{
		ID: id, PID: os.Getpid(), Started: time.Now(),
		Provider: providerQEMU, Target: "img:/x.img",
		Mounted: true, Device: "vdb", Share: "smb",
		URL: "smb://127.0.0.1:9000/lmnt", User: "lmnt", Password: "pw",
	}

	if here {
		rt.started[id] = time.Now()
	}

	return rec
}

// publishMount writes a mounted record and holds its lock, standing in for
// the background process a real spawn would start.
func publishMount(t *testing.T, rt *tuiRuntime, id string) string {
	t.Helper()

	release, err := rt.dir.LockSession(id)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(release)

	rec := fakeMount(rt, id, true)
	if err := rt.dir.SaveSession(rec); err != nil {
		t.Fatal(err)
	}

	return id
}

// stopRequested reports whether a stop file was created for id.
func stopRequested(t *testing.T, rt *tuiRuntime, id string) bool {
	t.Helper()

	requested, err := rt.dir.StopRequested(id)
	if err != nil {
		t.Fatal(err)
	}

	return requested
}

func press(m tea.Model, keys ...tea.Key) tea.Model {
	m, _ = pressCmd(m, keys...)

	return m
}

// pressCmd is press that also hands back the last command, for the steps
// whose work happens in one.
func pressCmd(m tea.Model, keys ...tea.Key) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	for _, k := range keys {
		m, cmd = m.Update(tea.KeyPressMsg(k))
	}

	return m, cmd
}

// run executes a command and feeds its message back into the model, the way
// the Bubble Tea runtime would.
func run(t *testing.T, m tea.Model, cmd tea.Cmd) {
	t.Helper()

	if cmd == nil {
		t.Fatal("expected a command")
	}

	if msg := cmd(); msg != nil {
		_, _ = m.Update(msg)
	}
}

func text(s string) tea.Key { return tea.Key{Code: rune(s[0]), Text: s} }

var (
	keyEnter = tea.Key{Code: tea.KeyEnter}
	keyEsc   = tea.Key{Code: tea.KeyEscape}
	keyDown  = tea.Key{Code: tea.KeyDown}
	keyCtrlC = tea.Key{Code: 'c', Mod: tea.ModCtrl}
)

func TestQuitWithoutOwnMountsQuitsAtOnce(t *testing.T) {
	m := newTUIModel(testRuntime(t))

	_, cmd := m.Update(tea.KeyPressMsg(text("q")))
	if cmd == nil {
		t.Fatal("expected a command")
	}

	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Errorf("got %T, want QuitMsg", cmd())
	}
}

// Mounts started elsewhere are none of this TUI's business on the way out.
func TestQuitIgnoresForeignMounts(t *testing.T) {
	rt := testRuntime(t)
	rec := fakeMount(rt, "lmnt-00000007", false)

	var m tea.Model = newTUIModel(rt)
	m, _ = m.Update(registryMsg{records: []datadir.Session{rec}})

	_, cmd := m.Update(tea.KeyPressMsg(text("q")))
	if cmd == nil {
		t.Fatal("expected a quit command")
	}

	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Errorf("got %T, want QuitMsg", cmd())
	}
}

func TestQuitOffersLeavingOrUnmounting(t *testing.T) {
	rt := testRuntime(t)
	rec := fakeMount(rt, "lmnt-00000001", true)

	var m tea.Model = newTUIModel(rt)
	m, _ = m.Update(tea.WindowSizeMsg{Width: 140, Height: 40})
	m, _ = m.Update(registryMsg{records: []datadir.Session{rec}})

	m, cmd := m.Update(tea.KeyPressMsg(keyCtrlC))
	if cmd != nil {
		t.Error("ctrl+c with mounts of this TUI must not quit immediately")
	}

	if m.(tuiModel).screen != screenConfirmQuit {
		t.Fatalf("screen = %v, want confirm", m.(tuiModel).screen)
	}

	view := m.View().Content
	for _, want := range []string{"leave running", "unmount all"} {
		if !strings.Contains(view, want) {
			t.Errorf("the quit prompt lacks %q", want)
		}
	}

	// esc backs out.
	m = press(m, keyEsc)
	if m.(tuiModel).screen != screenSessions {
		t.Fatal("esc did not cancel the quit")
	}

	// Leaving quits without asking anything to stop.
	m, _ = m.Update(tea.KeyPressMsg(text("q")))
	_, cmd = m.Update(tea.KeyPressMsg(text("l")))

	if cmd == nil {
		t.Fatal("expected a quit command")
	}

	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Errorf("got %T, want QuitMsg", cmd())
	}

	if stopRequested(t, rt, rec.ID) {
		t.Error("leaving the mounts running must not ask them to stop")
	}
}

func TestQuitUnmountingWaitsForTheMounts(t *testing.T) {
	rt := testRuntime(t)
	rec := fakeMount(rt, "lmnt-00000008", true)

	var m tea.Model = newTUIModel(rt)
	m, _ = m.Update(tea.WindowSizeMsg{Width: 140, Height: 40})
	m, _ = m.Update(registryMsg{records: []datadir.Session{rec}})
	m, _ = m.Update(tea.KeyPressMsg(text("q")))
	m, _ = m.Update(tea.KeyPressMsg(text("u")))

	if !m.(tuiModel).quitting {
		t.Fatal("u did not start quitting")
	}

	if !stopRequested(t, rt, rec.ID) {
		t.Fatal("the mount was not asked to stop")
	}

	// While it is still listed the program stays up.
	m, cmd := m.Update(registryMsg{records: []datadir.Session{rec}})
	if cmd != nil {
		if _, ok := cmd().(tea.QuitMsg); ok {
			t.Error("quit before the mount was gone")
		}
	}

	// Once it is gone, the program ends.
	_, cmd = m.Update(registryMsg{})
	if cmd == nil {
		t.Fatal("expected quit once the mounts were gone")
	}

	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Errorf("got %T, want QuitMsg", cmd())
	}
}

// A signal is not an answer to the unmount question: the mounts stay up.
func TestSignalQuitLeavesMountsRunning(t *testing.T) {
	rt := testRuntime(t)
	rec := fakeMount(rt, "lmnt-0000000a", true)

	var m tea.Model = newTUIModel(rt)
	m, _ = m.Update(registryMsg{records: []datadir.Session{rec}})

	_, cmd := m.Update(signalQuitMsg{})
	if cmd == nil {
		t.Fatal("expected a quit command")
	}

	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Errorf("got %T, want QuitMsg", cmd())
	}

	if stopRequested(t, rt, rec.ID) {
		t.Error("a signal must not unmount anything")
	}
}

func TestStopSelectedAsksTheMountToStop(t *testing.T) {
	rt := testRuntime(t)
	rec := fakeMount(rt, "lmnt-00000002", true)

	var m tea.Model = newTUIModel(rt)
	m, _ = m.Update(tea.WindowSizeMsg{Width: 140, Height: 40})
	m, _ = m.Update(registryMsg{records: []datadir.Session{rec}})
	_ = press(m, text("s"))

	if !stopRequested(t, rt, rec.ID) {
		t.Error("s did not ask the mount to stop")
	}
}

func TestRowsComeFromTheRegistry(t *testing.T) {
	rt := testRuntime(t)
	mine := fakeMount(rt, "lmnt-00000003", true)
	theirs := fakeMount(rt, "lmnt-aaaaaaaa", false)

	var m tea.Model = newTUIModel(rt)
	m, _ = m.Update(tea.WindowSizeMsg{Width: 140, Height: 40})
	m, _ = m.Update(registryMsg{records: []datadir.Session{mine, theirs}})

	rows := m.(tuiModel).rows
	if len(rows) != 2 {
		t.Fatalf("rows = %+v, want both mounts", rows)
	}

	if !rows[0].here || rows[1].here {
		t.Errorf("here flags wrong: %v %v", rows[0].here, rows[1].here)
	}

	m = press(m, keyEnter)
	if m.(tuiModel).screen != screenDetail {
		t.Fatal("enter did not open details")
	}

	view := m.View().Content
	for _, want := range []string{"smb://127.0.0.1:9000/lmnt", "pw"} {
		if !strings.Contains(view, want) {
			t.Errorf("detail view lacks %q", want)
		}
	}

	m = press(m, keyEsc)
	if m.(tuiModel).screen != screenSessions {
		t.Fatal("esc did not go back")
	}
}

func TestWizardRadioAndBack(t *testing.T) {
	rt := testRuntime(t)
	var m tea.Model = newTUIModel(rt)
	m, _ = m.Update(tea.WindowSizeMsg{Width: 140, Height: 40})
	m = press(m, text("n"))

	tm := m.(tuiModel)
	if tm.screen != screenWizard || tm.wizard.step != stepTarget {
		t.Fatalf("n did not open the wizard: screen=%v step=%v", tm.screen, tm.wizard.step)
	}

	// A bad target stays on the step with an error.
	for _, r := range "img:/nope.img" {
		m = press(m, tea.Key{Code: r, Text: string(r)})
	}
	m = press(m, keyEnter)
	tm = m.(tuiModel)
	if tm.wizard.step != stepTarget || tm.wizard.err == "" {
		t.Fatalf("missing image accepted: step=%v err=%q", tm.wizard.step, tm.wizard.err)
	}

	// Radio steps: reach them by faking the target step.
	tm.wizard.step = stepProvider
	m = press(tm, keyDown)
	if got := m.(tuiModel).wizard.providerIdx; got != 1 {
		t.Errorf("down did not move the provider cursor: %d", got)
	}
	m = press(m, text("k"))
	if got := m.(tuiModel).wizard.providerIdx; got != 0 {
		t.Errorf("k did not move the provider cursor back: %d", got)
	}

	m = press(m, keyEnter)
	if got := m.(tuiModel).wizard.step; got != stepShare {
		t.Errorf("enter did not advance to the share step: %v", got)
	}

	m = press(m, keyEsc)
	if got := m.(tuiModel).wizard.step; got != stepProvider {
		t.Errorf("esc did not go back to the provider step: %v", got)
	}

	// Escaping from the first step leaves the wizard.
	tm = m.(tuiModel)
	tm.wizard.step = stepTarget
	m = press(tm, keyEsc)
	if m.(tuiModel).screen != screenSessions {
		t.Error("esc on the first step did not leave the wizard")
	}
}

// The access step sits between the target and the provider, its choice ends
// up in the spec so both the listing guest and the mount attach read-only.
func TestWizardAccessStep(t *testing.T) {
	rt := testRuntime(t)
	var m tea.Model = newTUIModel(rt)
	m, _ = m.Update(tea.WindowSizeMsg{Width: 140, Height: 40})

	tm := m.(tuiModel)
	tm.screen = screenWizard
	tm.wizard.step = stepAccess
	m = press(tm, keyDown)

	if got := m.(tuiModel).wizard; !got.readOnly() || !strings.Contains(got.stepValue(stepAccess), "read-only") {
		t.Fatalf("down did not select read-only: idx=%d", got.accessIdx)
	}

	m = press(m, keyEnter)
	if got := m.(tuiModel).wizard.step; got != stepProvider {
		t.Errorf("enter did not advance to the provider step: %v", got)
	}

	m = press(m, keyEsc)
	if got := m.(tuiModel).wizard.step; got != stepAccess {
		t.Errorf("esc from the provider step went to %v, want access", got)
	}

	m = press(m, keyEsc)
	if got := m.(tuiModel).wizard.step; got != stepTarget {
		t.Errorf("esc from the access step went to %v, want target", got)
	}

	// The mount options screen reminds the user of the choice.
	tm = m.(tuiModel)
	tm.wizard.step = stepMountOptions
	tm.wizard.devs = []guest.BlockDevice{{Name: "vdb1", Path: "/dev/vdb1", Type: "part", FSType: "ext4"}}
	if view := tm.wizard.view(""); !strings.Contains(view, "read-only") {
		t.Errorf("mount options view does not mention read-only:\n%s", view)
	}
}

func TestWizardPicksDeviceAndHandsOver(t *testing.T) {
	rt := testRuntime(t)

	var got struct {
		spec   mountSpec
		choice mountChoice
	}

	rt.spawn = func(spec mountSpec, choice mountChoice, _, _ []byte) (string, error) {
		got.spec, got.choice = spec, choice

		return publishMount(t, rt, "lmnt-00000005"), nil
	}

	var m tea.Model = newTUIModel(rt)
	m, _ = m.Update(tea.WindowSizeMsg{Width: 140, Height: 40})
	tm := m.(tuiModel)
	tm.screen = screenWizard
	tm.wizard.step = stepBooting
	tm.wizard.spec = mountSpec{provider: providerQEMU, shareName: "smb"}
	m = tm

	devices := []guest.BlockDevice{
		{Name: "vdb", Path: "/dev/vdb", Type: "disk", FSType: "LVM2_member", Size: "96M"},
		{Name: "vgt-data", Path: "/dev/mapper/vgt-data", Type: "lvm", FSType: "ext4", Size: "92M", Depth: 1},
	}

	m, _ = m.Update(devicesMsg{id: "listing", devices: devices})
	tm = m.(tuiModel)

	if tm.wizard.step != stepDevice {
		t.Fatalf("step = %v, want device", tm.wizard.step)
	}

	if got := tm.wizard.devCursor; got != 1 {
		t.Errorf("cursor = %d, want the mountable LV preselected", got)
	}

	// Enter → mount options, Enter → the mount is handed to its own process.
	m = press(m, keyEnter)
	if step := m.(tuiModel).wizard.step; step != stepMountOptions {
		t.Fatalf("step = %v, want mount options", step)
	}

	m, cmd := pressCmd(m, keyEnter)

	if step := m.(tuiModel).wizard.step; step != stepMounting {
		t.Errorf("step = %v, want mounting", step)
	}

	// The hand-over happens in the command, not in Update.
	if got.choice.Device != "" {
		t.Error("the mount was spawned from Update rather than from a command")
	}

	run(t, m, cmd)

	if got.choice.Device != "mapper/vgt-data" || got.choice.LUKS {
		t.Errorf("choice = %+v", got.choice)
	}
}

// A LUKS device is asked for its passphrase before the hand-over, and the
// passphrase goes to the background process rather than to a guest here.
func TestWizardAsksVolumePassphraseBeforeHandover(t *testing.T) {
	rt := testRuntime(t)

	var volume []byte

	rt.spawn = func(_ mountSpec, _ mountChoice, _, v []byte) (string, error) {
		volume = v

		return publishMount(t, rt, "lmnt-00000006"), nil
	}

	var m tea.Model = newTUIModel(rt)
	m, _ = m.Update(tea.WindowSizeMsg{Width: 140, Height: 40})
	tm := m.(tuiModel)
	tm.screen = screenWizard
	tm.wizard.step = stepBooting
	m = tm

	m, _ = m.Update(devicesMsg{id: "listing", devices: []guest.BlockDevice{
		{Name: "vdb", Path: "/dev/vdb", Type: "disk", FSType: "crypto_LUKS", Size: "96M"},
	}})

	// Choosing a crypto_LUKS device marks it as a LUKS volume.
	m = press(m, keyEnter)
	if !m.(tuiModel).wizard.luksVolume {
		t.Fatal("a crypto_LUKS device was not marked as a LUKS volume")
	}

	m = press(m, keyEnter)
	if step := m.(tuiModel).wizard.step; step != stepPassphrase {
		t.Fatalf("step = %v, want passphrase", step)
	}

	if volume != nil {
		t.Fatal("the mount was handed over before the passphrase was given")
	}

	m, cmd := pressCmd(m, text("s"), text("3"), keyEnter)

	if step := m.(tuiModel).wizard.step; step != stepMounting {
		t.Errorf("step = %v, want mounting", step)
	}

	run(t, m, cmd)

	if string(volume) != "s3" {
		t.Errorf("passphrase = %q, want s3", volume)
	}
}

// The listing guest can need a passphrase of its own, to open a container
// before its contents are visible.
func TestWizardAnswersTheListingPrompt(t *testing.T) {
	rt := testRuntime(t)

	ls := &listingSession{
		id:     "lmnt-0000000b",
		secret: make(chan promptReply, 1),
		done:   make(chan struct{}),
		logs:   newLineRing(10),
	}
	ls.ctx, ls.cancel = context.WithCancelCause(rt.ctx)

	var m tea.Model = newTUIModel(rt)
	m, _ = m.Update(tea.WindowSizeMsg{Width: 140, Height: 40})
	tm := m.(tuiModel)
	tm.screen = screenWizard
	tm.wizard.ls = ls
	tm.wizard.step = stepBooting
	m = tm

	m, _ = m.Update(promptMsg{id: ls.id, text: "Passphrase for /dev/vdb3: "})
	if step := m.(tuiModel).wizard.step; step != stepPassphrase {
		t.Fatalf("step = %v, want passphrase", step)
	}

	m = press(m, text("s"), text("3"), keyEnter)

	select {
	case r := <-ls.secret:
		if string(r.secret) != "s3" || r.err != nil {
			t.Errorf("reply = %q, %v", r.secret, r.err)
		}
	default:
		t.Fatal("no passphrase reached the listing guest")
	}

	// Cancelling instead stops the listing guest and closes the wizard.
	m, _ = m.Update(promptMsg{id: ls.id, text: "again"})
	m = press(m, keyEsc)

	select {
	case r := <-ls.secret:
		if !errors.Is(r.err, errPromptCancelled) {
			t.Errorf("reply err = %v", r.err)
		}
	default:
		t.Fatal("the cancel did not reach the listing guest")
	}

	if !errors.Is(context.Cause(ls.ctx), provider.ErrInterrupted) {
		t.Error("cancelling did not stop the listing guest")
	}

	if m.(tuiModel).screen != screenSessions {
		t.Error("the wizard did not close after the cancel")
	}
}

// A listing guest that fails to boot must say so instead of closing the
// wizard in silence.
func TestWizardReportsAFailedListing(t *testing.T) {
	rt := testRuntime(t)

	var m tea.Model = newTUIModel(rt)
	m, _ = m.Update(tea.WindowSizeMsg{Width: 140, Height: 40})
	tm := m.(tuiModel)
	tm.screen = screenWizard
	tm.wizard.step = stepBooting
	m = tm

	m, _ = m.Update(listingDoneMsg{id: "lmnt-0000000d", err: errors.New("the guest image is not built yet")})
	tm = m.(tuiModel)

	if tm.screen != screenSessions {
		t.Errorf("screen = %v, want the sessions list", tm.screen)
	}

	if !strings.Contains(tm.View().Content, "not built yet") {
		t.Error("the boot failure is not reported")
	}
}

func TestWizardFinishesWhenTheMountIsUp(t *testing.T) {
	rt := testRuntime(t)
	rec := fakeMount(rt, "lmnt-00000004", true)
	rec.Share = "sftp"
	rec.URL = "sftp://lmnt@127.0.0.1:9000/"

	var m tea.Model = newTUIModel(rt)
	m, _ = m.Update(tea.WindowSizeMsg{Width: 140, Height: 40})
	tm := m.(tuiModel)
	tm.screen = screenWizard
	tm.wizard.step = stepMounting
	tm.wizard.mountID = rec.ID
	tm.registry = []datadir.Session{rec}
	m = tm

	m, _ = m.Update(mountUpMsg{id: rec.ID, record: rec})
	tm = m.(tuiModel)

	if tm.screen != screenSessions {
		t.Errorf("screen = %v, want the sessions list", tm.screen)
	}

	if !strings.Contains(tm.View().Content, "sftp://lmnt@127.0.0.1:9000/") {
		t.Error("the new mount's URL is not shown")
	}
}

// A mount that dies on its way up reports why, without closing the TUI.
func TestWizardReportsAFailedMount(t *testing.T) {
	rt := testRuntime(t)

	var m tea.Model = newTUIModel(rt)
	m, _ = m.Update(tea.WindowSizeMsg{Width: 140, Height: 40})
	tm := m.(tuiModel)
	tm.screen = screenWizard
	tm.wizard.step = stepMounting
	m = tm

	m, _ = m.Update(mountFailedMsg{id: "lmnt-0000000c", err: errors.New("no key available")})
	tm = m.(tuiModel)

	if tm.screen != screenSessions {
		t.Errorf("screen = %v, want the sessions list", tm.screen)
	}

	if !strings.Contains(tm.View().Content, "no key available") {
		t.Error("the failure is not shown")
	}
}

func TestUptimeAndTruncate(t *testing.T) {
	if got := uptime(time.Now().Add(-90 * time.Second)); got != "1m30s" {
		t.Errorf("uptime = %q", got)
	}
	if got := truncate("abcdefghij", 5); got != "…ghij" {
		t.Errorf("truncate = %q", got)
	}
	if got := truncateRight("abcdefghij", 5); got != "abcd…" {
		t.Errorf("truncateRight = %q", got)
	}
}

// The wizard offers a share password between the protocol and the LUKS step:
// an empty field leaves the mount to generate one.
func TestWizardSharePasswordStep(t *testing.T) {
	rt := testRuntime(t)
	var m tea.Model = newTUIModel(rt)
	m, _ = m.Update(tea.WindowSizeMsg{Width: 140, Height: 40})

	tm := m.(tuiModel)
	tm.screen = screenWizard
	tm.wizard.step = stepShare
	m = press(tm, keyEnter)

	if step := m.(tuiModel).wizard.step; step != stepSharePassword {
		t.Fatalf("step = %v, want the share password step", step)
	}

	// A password the backends cannot carry is refused on the spot.
	tm = m.(tuiModel)
	tm.wizard.sharePassIn.SetValue("pässwort")
	m = press(tm, keyEnter)
	tm = m.(tuiModel)

	if tm.wizard.step != stepSharePassword || tm.wizard.err == "" {
		t.Fatalf("a non-ASCII password was accepted: step=%v err=%q", tm.wizard.step, tm.wizard.err)
	}

	tm.wizard.sharePassIn.SetValue("hunter2")
	m = press(tm, keyEnter)
	tm = m.(tuiModel)

	if tm.wizard.step != stepLUKS || tm.wizard.err != "" {
		t.Fatalf("step = %v, err = %q", tm.wizard.step, tm.wizard.err)
	}

	// esc comes back to it.
	m = press(tm, keyEsc)
	if step := m.(tuiModel).wizard.step; step != stepSharePassword {
		t.Errorf("esc from the LUKS step went to %v", step)
	}
}

// What the wizard collected travels to the background mount in the spec, not
// on its command line.
func TestWizardHandsOverTheSharePassword(t *testing.T) {
	rt := testRuntime(t)

	var got mountSpec

	rt.spawn = func(spec mountSpec, _ mountChoice, _, _ []byte) (string, error) {
		got = spec

		return publishMount(t, rt, "lmnt-00000007"), nil
	}

	var m tea.Model = newTUIModel(rt)
	m, _ = m.Update(tea.WindowSizeMsg{Width: 140, Height: 40})
	tm := m.(tuiModel)
	tm.screen = screenWizard
	tm.wizard.step = stepBooting
	tm.wizard.spec = mountSpec{provider: providerQEMU, shareName: "smb", sharePassword: []byte("hunter2")}
	m = tm

	m, _ = m.Update(devicesMsg{id: "listing", devices: []guest.BlockDevice{
		{Name: "vdb1", Path: "/dev/vdb1", Type: "part", FSType: "ext4", Size: "92M"},
	}})

	m = press(m, keyEnter) // device → mount options

	_, cmd := pressCmd(m, keyEnter) // mount options → hand-over
	run(t, m, cmd)

	if string(got.sharePassword) != "hunter2" {
		t.Errorf("spawned with password %q, want hunter2", got.sharePassword)
	}
}
