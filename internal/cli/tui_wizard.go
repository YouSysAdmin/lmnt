package cli

import (
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"github.com/yousysadmin/lmnt/internal/guest"
	"github.com/yousysadmin/lmnt/internal/provider"
	"github.com/yousysadmin/lmnt/internal/share"
	"github.com/yousysadmin/lmnt/internal/target"
)

type wizardStep int

const (
	stepTarget wizardStep = iota
	stepAccess
	stepProvider
	stepShare
	stepSharePassword
	stepLUKS
	stepContainer
	stepBooting
	stepDevice
	stepMountOptions
	stepPassphrase
	stepMounting
)

// stepTitles are the sidebar labels of the wizard steps, in order.
var stepTitles = map[wizardStep]string{
	stepTarget:        "Target",
	stepAccess:        "Access",
	stepProvider:      "Provider",
	stepShare:         "Share",
	stepSharePassword: "Share password",
	stepLUKS:          "LUKS container",
	stepContainer:     "Container device",
	stepBooting:       "Boot guest",
	stepDevice:        "Device",
	stepMountOptions:  "Mount options",
	stepPassphrase:    "Passphrase",
	stepMounting:      "Mount & share",
}

// luksModes are the choices of the LUKS step.
var luksModes = []string{"no LUKS container", "open a LUKS container first (LVM inside)", "the whole disk is a LUKS container"}

// accessModes are the choices of the access step, index 1 is read-only.
var accessModes = []string{"read-write", "read-only"}

var providerNames = []string{providerQEMU, providerDocker}

// wizardModel walks the user from a target to a running share. It boots one
// guest to list the disk's devices, then hands the mount to a detached
// process, so the mount outlives this TUI.
type wizardModel struct {
	rt   *tuiRuntime
	step wizardStep
	err  string

	// Collected input.
	targetIn    textinput.Model
	containerIn textinput.Model
	sharePassIn textinput.Model
	optionsIn   textinput.Model
	fstypeIn    textinput.Model
	passIn      textinput.Model
	accessIdx   int // index into accessModes
	providerIdx int
	shareIdx    int
	luksIdx     int
	optionField int  // 0 = fstype, 1 = options
	luksVolume  bool // mount the chosen device as a LUKS volume

	// awaitingListing marks a passphrase the listing guest is waiting for,
	// as opposed to one collected for the background mount.
	awaitingListing bool

	devs      []guest.BlockDevice
	devCursor int
	ls        *listingSession
	prompted  bool // a container passphrase was asked during listing
	width     int
	height    int
	finished  bool // the share is up, leave the wizard
	aborted   bool // the user backed out (or the session failed) before that
	failure   string

	// spec, fstype and options are what the background mount is started with.
	spec    mountSpec
	fstype  string
	options string
	// mountID is the background mount once it has been started.
	mountID string
	// volume is the passphrase for a LUKS device chosen from the list.
	volume []byte
}

func newWizard(rt *tuiRuntime) wizardModel {
	mk := func(placeholder string) textinput.Model {
		ti := textinput.New()
		ti.Placeholder = placeholder
		ti.Prompt = "> "
		ti.SetWidth(60)

		return ti
	}

	pass := mk("passphrase")
	pass.EchoMode = textinput.EchoPassword
	pass.EchoCharacter = '•'

	sharePass := mk("empty = a random password")
	sharePass.EchoMode = textinput.EchoPassword
	sharePass.EchoCharacter = '•'

	w := wizardModel{
		rt:          rt,
		targetIn:    mk("img:/path/to/disk.img   dev:/dev/disk4   usb:0781,5583"),
		containerIn: mk("vdb3"),
		sharePassIn: sharePass,
		optionsIn:   mk("noatime,subvol=@home (empty = defaults)"),
		fstypeIn:    mk("auto"),
		passIn:      pass,
		width:       80,
		height:      24,
	}

	if rt.app.opts.provider == providerDocker {
		w.providerIdx = 1
	}

	return w
}

func (w *wizardModel) resize(width, height int) {
	w.width, w.height = width, height
	for _, ti := range []*textinput.Model{&w.targetIn, &w.containerIn, &w.sharePassIn, &w.optionsIn, &w.fstypeIn, &w.passIn} {
		ti.SetWidth(max(min(width-4, 80), 20))
	}
}

// start focuses the first input.
func (w *wizardModel) start() tea.Cmd {
	return w.targetIn.Focus()
}

func (w *wizardModel) update(msg tea.Msg) tea.Cmd {
	keyMsg, isKey := msg.(tea.KeyPressMsg)

	if isKey && keyMsg.String() == "esc" {
		return w.back()
	}

	switch w.step {
	case stepTarget:
		if isKey && keyMsg.String() == "enter" {
			return w.acceptTarget()
		}

		var cmd tea.Cmd
		w.targetIn, cmd = w.targetIn.Update(msg)

		return cmd

	case stepAccess:
		w.radio(msg, &w.accessIdx, len(accessModes), stepProvider)
		return nil

	case stepProvider:
		w.radio(msg, &w.providerIdx, len(providerNames), stepShare)
		return nil

	case stepShare:
		w.radio(msg, &w.shareIdx, len(share.Names()), stepSharePassword)
		if w.step == stepSharePassword {
			return w.sharePassIn.Focus()
		}

		return nil

	case stepSharePassword:
		if isKey && keyMsg.String() == "enter" {
			return w.acceptSharePassword()
		}

		var cmd tea.Cmd
		w.sharePassIn, cmd = w.sharePassIn.Update(msg)

		return cmd

	case stepLUKS:
		next := stepBooting
		if isKey && keyMsg.String() == "enter" && w.luksIdx == 1 {
			next = stepContainer
		}

		w.radio(msg, &w.luksIdx, len(luksModes), next)
		if w.step == stepContainer {
			return w.containerIn.Focus()
		}
		if w.step == stepBooting {
			return w.boot()
		}

		return nil

	case stepContainer:
		if isKey && keyMsg.String() == "enter" {
			name := strings.TrimSpace(w.containerIn.Value())
			if !guest.ValidDeviceName(name) {
				w.err = fmt.Sprintf("%q is not a device name like vdb3 or loop0p2", name)
				return nil
			}

			w.err = ""
			w.step = stepBooting

			return w.boot()
		}

		var cmd tea.Cmd
		w.containerIn, cmd = w.containerIn.Update(msg)

		return cmd

	case stepDevice:
		if isKey {
			switch keyMsg.String() {
			case "enter":
				return w.acceptDevice()
			case "l":
				w.luksVolume = !w.luksVolume
			case "up", "k":
				w.devCursor = max(w.devCursor-1, 0)
			case "down", "j":
				w.devCursor = min(w.devCursor+1, max(len(w.devs)-1, 0))
			}
		}

		return nil

	case stepMountOptions:
		if isKey {
			switch keyMsg.String() {
			case "enter":
				return w.acceptMountOptions()
			case "tab", "down", "up":
				w.optionField = 1 - w.optionField
				if w.optionField == 0 {
					w.optionsIn.Blur()
					return w.fstypeIn.Focus()
				}
				w.fstypeIn.Blur()

				return w.optionsIn.Focus()
			case "ctrl+l":
				w.luksVolume = !w.luksVolume
				return nil
			}
		}

		var cmd tea.Cmd
		if w.optionField == 0 {
			w.fstypeIn, cmd = w.fstypeIn.Update(msg)
		} else {
			w.optionsIn, cmd = w.optionsIn.Update(msg)
		}

		return cmd

	case stepPassphrase:
		if isKey && keyMsg.String() == "enter" {
			secret := []byte(w.passIn.Value())
			w.passIn.SetValue("")

			if w.awaitingListing {
				// The listing guest is blocked on this: it needs the
				// container open before it can see what is inside.
				w.awaitingListing = false
				w.ls.answerPrompt(promptReply{secret: secret})
				w.step = stepBooting

				return nil
			}

			w.volume = secret

			return w.launch()
		}

		var cmd tea.Cmd
		w.passIn, cmd = w.passIn.Update(msg)

		return cmd
	}

	return nil
}

// radio handles up/down/enter for a fixed list of options.
func (w *wizardModel) radio(msg tea.Msg, idx *int, n int, next wizardStep) {
	keyMsg, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return
	}

	switch keyMsg.String() {
	case "up", "k":
		*idx = (*idx + n - 1) % n
	case "down", "j", "tab":
		*idx = (*idx + 1) % n
	case "enter":
		w.step = next
	}
}

func (w *wizardModel) back() tea.Cmd {
	switch w.step {
	case stepTarget:
		w.aborted = true
	case stepAccess:
		w.step = stepTarget
		return w.targetIn.Focus()
	case stepProvider:
		w.step = stepAccess
	case stepShare:
		w.step = stepProvider
	case stepSharePassword:
		w.sharePassIn.Blur()
		w.step = stepShare
	case stepLUKS:
		w.step = stepSharePassword

		return w.sharePassIn.Focus()
	case stepContainer:
		w.step = stepLUKS
	case stepBooting, stepDevice, stepMountOptions:
		// Backing out stops the listing guest, no mount was started yet.
		w.stopListing()
		w.aborted = true
	case stepMounting:
		// The mount is already its own process: leave it be and let the
		// sessions list show how it turns out.
		w.aborted = true
	case stepPassphrase:
		if w.awaitingListing {
			w.awaitingListing = false
			w.ls.answerPrompt(promptReply{err: errPromptCancelled})
		}

		w.stopListing()
		w.aborted = true
	}

	return nil
}

func (w *wizardModel) acceptTarget() tea.Cmd {
	t, err := target.Parse(strings.TrimSpace(w.targetIn.Value()))
	if err != nil {
		w.err = err.Error()
		return nil
	}

	// Root and mounted-on-host checks fail fast here rather than after a boot.
	if err := (&session{app: w.rt.app, target: &t}).checkTargetAccess(); err != nil {
		w.err = err.Error()
		return nil
	}

	w.err = ""
	w.targetIn.Blur()
	w.step = stepAccess

	return nil
}

// acceptSharePassword takes the share password, or leaves it to the mount to
// generate one when the field is empty.
func (w *wizardModel) acceptSharePassword() tea.Cmd {
	if p := w.sharePassIn.Value(); p != "" {
		if err := share.ValidatePassword(p); err != nil {
			w.err = err.Error()

			return nil
		}
	}

	w.err = ""
	w.sharePassIn.Blur()
	w.step = stepLUKS

	return nil
}

func (w *wizardModel) boot() tea.Cmd {
	t, err := target.Parse(strings.TrimSpace(w.targetIn.Value()))
	if err != nil {
		w.err = err.Error()
		w.step = stepTarget

		return w.targetIn.Focus()
	}

	spec := mountSpec{
		target:     t,
		provider:   providerNames[w.providerIdx],
		shareName:  share.Names()[w.shareIdx],
		luksEntire: w.luksIdx == 2,
		readOnly:   w.readOnly(),
	}
	if w.luksIdx == 1 {
		spec.luksContainer = strings.TrimSpace(w.containerIn.Value())
	}

	if p := w.sharePassIn.Value(); p != "" {
		spec.sharePassword = []byte(p)
	}

	if spec.provider == providerDocker && t.Kind != target.Image {
		w.err = "the docker provider can only attach disk images, pick qemu"
		w.step = stepProvider

		return nil
	}

	ls, err := w.rt.startListing(spec)
	if err != nil {
		w.err = err.Error()
		w.step = stepShare

		return nil
	}

	w.spec = spec
	w.ls = ls
	w.step = stepBooting

	return nil
}

// readOnly reports whether the access step chose read-only.
func (w *wizardModel) readOnly() bool {
	return w.accessIdx == 1
}

// stopListing ends the listing guest and forgets its cached passphrase.
func (w *wizardModel) stopListing() {
	if w.ls == nil {
		return
	}

	w.ls.stop()
	w.ls.wipe()
}

// launch hands the collected mount to a process of its own and waits for its
// share to come up.
func (w *wizardModel) launch() tea.Cmd {
	d, ok := w.currentDevice()
	if !ok {
		return nil
	}

	choice := mountChoice{Device: d.Device(), FSType: w.fstype, Options: w.options, LUKS: w.luksVolume}

	var container []byte
	if w.ls != nil {
		container = w.ls.container
	}

	// spawn takes ownership of the passphrases and scrubs them, the wizard
	// only drops its references.
	volume := w.volume
	w.volume = nil

	if w.ls != nil {
		w.ls.container = nil
	}

	rt, ls, spec := w.rt, w.ls, w.spec
	w.step = stepMounting

	return func() tea.Msg {
		// QEMU holds a write lock on the disk image for as long as its guest
		// lives, so the mount cannot open it until the listing guest is
		// really gone. Skipping this wait makes the mount's guest hang until
		// its boot times out.
		if ls != nil {
			select {
			case <-ls.done:
			case <-time.After(provider.StopTimeout):
			}
		}

		id, err := rt.spawn(spec, choice, container, volume)
		if err != nil {
			return mountFailedMsg{err: err}
		}

		return rt.awaitMount(id)()
	}
}

// follow reacts to events of the listing guest and of the background mount
// the wizard started.
func (w *wizardModel) follow(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case devicesMsg:
		w.devs = msg.devices
		w.devCursor = firstMountable(w.devs)
		w.step = stepDevice

	case promptMsg:
		w.step = stepPassphrase
		w.prompted = true
		w.awaitingListing = true

		return w.passIn.Focus()

	case listingDoneMsg:
		// The listing guest ending is expected once its list is out, only a
		// failure before that ends the wizard.
		if w.step != stepDevice && w.step != stepMountOptions && w.step != stepPassphrase && w.step != stepMounting {
			w.aborted = true

			if msg.err != nil && !errorsIsInterrupted(msg.err) {
				w.failure = msg.err.Error()
			}
		}

	case mountUpMsg:
		w.finished = true

	case mountFailedMsg:
		w.aborted = true
		w.failure = msg.err.Error()
	}

	return nil
}

// passphraseFor says which passphrase the step is asking for.
func (w *wizardModel) passphraseFor() string {
	if w.awaitingListing {
		return "the LUKS container has to be opened before its contents show up"
	}

	if d, ok := w.currentDevice(); ok {
		return "for the LUKS volume " + d.Device()
	}

	return "for the LUKS volume"
}

// firstMountable picks the first device that is not a container of others.
func firstMountable(devs []guest.BlockDevice) int {
	for i, d := range devs {
		if !d.IsContainer() && d.Type != "disk" && d.Type != "loop" {
			return i
		}
	}

	for i, d := range devs {
		if !d.IsContainer() {
			return i
		}
	}

	return 0
}

func (w *wizardModel) currentDevice() (guest.BlockDevice, bool) {
	if w.devCursor < 0 || w.devCursor >= len(w.devs) {
		return guest.BlockDevice{}, false
	}

	return w.devs[w.devCursor], true
}

func (w *wizardModel) acceptDevice() tea.Cmd {
	d, ok := w.currentDevice()
	if !ok {
		return nil
	}

	if d.FSType == "crypto_LUKS" {
		w.luksVolume = true
	} else if d.IsContainer() {
		w.err = d.Device() + " holds other devices (" + d.FSType + "), pick one of them"
		return nil
	}

	w.err = ""
	w.step = stepMountOptions
	w.optionField = 0

	return w.fstypeIn.Focus()
}

func (w *wizardModel) acceptMountOptions() tea.Cmd {
	d, ok := w.currentDevice()
	if !ok {
		return nil
	}

	fstype := strings.TrimSpace(w.fstypeIn.Value())
	if fstype == "auto" {
		fstype = ""
	}

	opts := strings.TrimSpace(w.optionsIn.Value())
	if err := guest.ValidMountOptions(opts); err != nil {
		w.err = err.Error()
		return nil
	}

	w.err = ""
	w.fstypeIn.Blur()
	w.optionsIn.Blur()
	w.fstype = fstype
	w.options = opts

	// A LUKS device needs its passphrase before the mount can be handed over.
	if w.luksVolume {
		w.step = stepPassphrase
		w.awaitingListing = false

		return w.passIn.Focus()
	}

	_ = d

	return w.launch()
}

// stepsView is the sidebar list of steps: done, active and pending.
func (w *wizardModel) stepsView() string {
	var b strings.Builder

	b.WriteString(stylePrompt.Render("New mount") + "\n")

	for _, s := range []wizardStep{stepTarget, stepAccess, stepProvider, stepShare, stepSharePassword, stepLUKS, stepContainer, stepBooting, stepDevice, stepMountOptions, stepPassphrase, stepMounting} {
		if s == stepContainer && w.luksIdx != 1 {
			continue
		}
		if s == stepPassphrase && !w.prompted && !w.luksVolume && w.luksIdx == 0 {
			continue
		}

		switch {
		case s == w.step:
			b.WriteString(styleActive.Render("  "+markActive) + styleActive.Render(stepTitles[s]) + "\n")
		case s < w.step:
			b.WriteString(styleDone.Render("  "+markDone) + styleText.Render(stepTitles[s]) + w.stepSuffix(s) + "\n")
		default:
			b.WriteString(styleMuted.Render("  "+markPending+stepTitles[s]) + "\n")
		}
	}

	return b.String()
}

// stepValue is the short summary shown next to a completed step.
func (w *wizardModel) stepValue(s wizardStep) string {
	switch s {
	case stepAccess:
		return accessModes[w.accessIdx]
	case stepProvider:
		return providerNames[w.providerIdx]
	case stepShare:
		return share.Names()[w.shareIdx]
	case stepSharePassword:
		if w.sharePassIn.Value() != "" {
			return "set"
		}

		return "random"
	case stepDevice:
		if d, ok := w.currentDevice(); ok && w.step > stepDevice {
			return d.Device()
		}
	}

	return ""
}

func (w *wizardModel) stepSuffix(s wizardStep) string {
	if v := w.stepValue(s); v != "" {
		return styleMuted.Render(" " + truncate(v, 14))
	}

	return ""
}

// view renders the content panel of the current step.
func (w *wizardModel) view(spinnerView string) string {
	var b strings.Builder

	switch w.step {
	case stepTarget:
		b.WriteString(stylePrompt.Render("What should the guest get?") + "\n")
		b.WriteString(styleMuted.Render("img:<file> needs no privileges, dev: and usb: need root and the qemu provider.") + "\n\n")
		b.WriteString(w.targetIn.View() + "\n")
		b.WriteString(w.errView())
		b.WriteString("\n" + hints("enter", "next", "esc", "back"))

	case stepAccess:
		b.WriteString(stylePrompt.Render("May the disk be written to?") + "\n")
		b.WriteString(styleMuted.Render("Read-only attaches the disk so the guest cannot write to it, mounts with -o ro and serves a read-only share.") + "\n\n")
		b.WriteString(radioView(accessModes, w.accessIdx, map[string]string{
			"read-write": "normal use, files can be added, changed and deleted",
			"read-only":  "nothing on the disk changes, safest for a disk you only want to read",
		}))
		b.WriteString(w.errView())
		b.WriteString("\n" + hints("↑/↓", "navigate", "enter", "select", "esc", "back"))

	case stepProvider:
		b.WriteString(stylePrompt.Render("How should Linux run?") + "\n\n")
		b.WriteString(radioView(providerNames, w.providerIdx, map[string]string{
			providerQEMU:   "virtual machine, physical disks, USB and images",
			providerDocker: "privileged container, disk images only, starts in a second",
		}))
		b.WriteString(w.errView())
		b.WriteString("\n" + hints("↑/↓", "navigate", "enter", "select", "esc", "back"))

	case stepShare:
		b.WriteString(stylePrompt.Render("Share protocol") + "\n\n")
		b.WriteString(radioView(share.Names(), w.shareIdx, map[string]string{
			"smb":  "Finder, writes as root",
			"ftp":  "fallback, read-only for root-owned files",
			"sftp": "any SFTP client, writes as root, easiest over the network",
		}))
		b.WriteString(w.errView())
		b.WriteString("\n" + hints("↑/↓", "navigate", "enter", "select", "esc", "back"))

	case stepSharePassword:
		b.WriteString(stylePrompt.Render("Share password") + "\n")
		b.WriteString(styleMuted.Render("The share user is "+share.User+". Leave this empty to get a random password.") + "\n\n")
		b.WriteString(w.sharePassIn.View() + "\n")
		b.WriteString(w.errView())
		b.WriteString("\n" + hints("enter", "next", "esc", "back"))

	case stepLUKS:
		b.WriteString(stylePrompt.Render("LUKS container") + "\n")
		b.WriteString(styleMuted.Render("For disks where LVM lives inside an encrypted container (Ubuntu, Fedora, Debian full-disk encryption).") + "\n\n")
		b.WriteString(radioView(luksModes, w.luksIdx, nil))
		b.WriteString("\n" + hints("↑/↓", "navigate", "enter", "select", "esc", "back"))

	case stepContainer:
		b.WriteString(stylePrompt.Render("Container device") + "\n")
		b.WriteString(styleMuted.Render("The guest device holding the LUKS container, e.g. vdb3 or loop0p3.") + "\n\n")
		b.WriteString(w.containerIn.View() + "\n")
		b.WriteString(w.errView())
		b.WriteString("\n" + hints("enter", "boot", "esc", "back"))

	case stepBooting:
		b.WriteString(stylePrompt.Render("Booting the guest") + "\n\n")
		b.WriteString(spinnerView + " " + styleMuted.Render("starting the guest and attaching the target…") + "\n\n")
		b.WriteString(w.logTail())
		b.WriteString("\n" + hints("esc", "abort"))

	case stepDevice:
		b.WriteString(w.deviceView())

	case stepMountOptions:
		d, _ := w.currentDevice()
		luks := "no"
		if w.luksVolume {
			luks = "yes"
		}
		b.WriteString(stylePrompt.Render("Mount "+d.Device()) + "  " + styleMuted.Render("LUKS volume: "+luks+" (ctrl+l toggles)  ·  "+accessModes[w.accessIdx]) + "\n\n")
		b.WriteString(fieldLabel("File system type", w.optionField == 0) + styleMuted.Render("  empty = auto") + "\n")
		b.WriteString(w.fstypeIn.View() + "\n\n")
		b.WriteString(fieldLabel("Mount options", w.optionField == 1) + styleMuted.Render("  mount -o, e.g. noatime,subvol=@home") + "\n")
		b.WriteString(w.optionsIn.View() + "\n")
		b.WriteString(w.errView())
		b.WriteString("\n" + hints("tab", "switch field", "enter", "mount", "esc", "abort"))

	case stepPassphrase:
		b.WriteString(stylePrompt.Render("Passphrase") + "\n")
		b.WriteString(styleMuted.Render(w.passphraseFor()) + "\n\n")
		b.WriteString(w.passIn.View() + "\n")
		b.WriteString("\n" + hints("enter", "unlock", "esc", "cancel"))

	case stepMounting:
		b.WriteString(stylePrompt.Render("Mounting and starting the share") + "\n\n")
		b.WriteString(spinnerView + " " + styleMuted.Render("the mount is starting in its own process…") + "\n\n")
		b.WriteString(styleMuted.Render("It keeps running when you quit the TUI, you will be asked about\nit on the way out.") + "\n\n")
		b.WriteString(w.logTail())
		b.WriteString("\n" + hints("esc", "back to the list"))
	}

	return b.String()
}

// deviceView is the device picker: the guest's block device tree with the
// selected device's details underneath.
func (w *wizardModel) deviceView() string {
	var b strings.Builder

	luks := "off"
	if w.luksVolume {
		luks = "on"
	}

	b.WriteString(stylePrompt.Render("Pick the device to mount") + "  " + styleMuted.Render("LUKS volume: "+luks) + "\n\n")

	if len(w.devs) == 0 {
		b.WriteString(styleMuted.Render("  the guest reports no block devices") + "\n")
	}

	for i, d := range w.devs {
		name := strings.Repeat("  ", d.Depth) + d.Device()
		meta := fmt.Sprintf("%-6s %-5s %s", d.Size, d.Type, d.FSType)

		switch {
		case i == w.devCursor:
			b.WriteString(styleCursor.Render("> ") + styleSelected.Render(name) + "  " + styleMuted.Render(meta) + "\n")
		case d.IsContainer():
			b.WriteString("  " + styleMuted.Render(name+"  "+meta) + "\n")
		default:
			b.WriteString("  " + styleText.Render(name) + "  " + styleMuted.Render(meta) + "\n")
		}
	}

	if d, ok := w.currentDevice(); ok {
		b.WriteString("\n" + separator(min(w.width, 60)) + "\n")
		b.WriteString(kv("Device", "/dev/"+d.Device()))
		b.WriteString(kv("Size", d.Size))
		b.WriteString(kv("Type", d.Type))
		b.WriteString(kv("File system", orDash(d.FSType)))
		b.WriteString(kv("Label", orDash(d.Label)))
		if d.IsContainer() {
			b.WriteString(styleWarn.Render("⚠ holds other devices, pick one of them") + "\n")
		}
	}

	b.WriteString(w.errView())
	b.WriteString("\n" + hints("↑/↓", "navigate", "enter", "mount this", "l", "LUKS volume", "esc", "abort & stop guest"))

	return b.String()
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}

	return s
}

func fieldLabel(label string, active bool) string {
	if active {
		return stylePrompt.Render(label)
	}

	return styleMuted.Render(label)
}

func (w *wizardModel) errView() string {
	if w.err == "" {
		return ""
	}

	return "\n" + styleError.Render("✗ "+w.err) + "\n"
}

func (w *wizardModel) logTail() string {
	if w.ls == nil {
		return ""
	}

	var b strings.Builder
	for _, line := range w.ls.logs.Tail(6) {
		b.WriteString(styleMuted.Render(truncateRight(line, max(w.width, 20))) + "\n")
	}

	return b.String()
}

func radioView(options []string, selected int, notes map[string]string) string {
	var b strings.Builder

	for i, o := range options {
		if i == selected {
			b.WriteString(styleCursor.Render("> ") + styleSelected.Render(o))
		} else {
			b.WriteString("  " + styleText.Render(o))
		}

		if n, ok := notes[o]; ok {
			b.WriteString("  " + styleMuted.Render(n))
		}

		b.WriteString("\n")
	}

	return b.String()
}
