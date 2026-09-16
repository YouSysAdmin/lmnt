package cli

import (
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/yousysadmin/lmnt/internal/datadir"
	"github.com/yousysadmin/lmnt/internal/provider"
)

type screen int

const (
	screenSessions screen = iota
	screenWizard
	screenDetail
	screenLogs
	screenConfirmQuit
)

const (
	registryRefresh = 2 * time.Second
	logTailLines    = 500
	// endedRowTTL is how long a finished session stays listed so its
	// outcome can be read.
)

// tuiKeyMap holds the bindings shown in the footer.
type tuiKeyMap struct {
	New, Stop, Details, Logs, Shell, CopyURL, CopyPassword, Quit, Back, Confirm, Up, Down key.Binding
}

func newKeyMap() tuiKeyMap {
	return tuiKeyMap{
		New:          key.NewBinding(key.WithKeys("n"), key.WithHelp("n", "new mount")),
		Stop:         key.NewBinding(key.WithKeys("s", "x"), key.WithHelp("s", "stop")),
		Details:      key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "details")),
		Logs:         key.NewBinding(key.WithKeys("l"), key.WithHelp("l", "logs")),
		CopyURL:      key.NewBinding(key.WithKeys("c"), key.WithHelp("c", "copy URL")),
		CopyPassword: key.NewBinding(key.WithKeys("p"), key.WithHelp("p", "copy password")),
		Quit:         key.NewBinding(key.WithKeys("q", "ctrl+c"), key.WithHelp("q", "quit")),
		Back:         key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "back")),
		Confirm:      key.NewBinding(key.WithKeys("y"), key.WithHelp("y", "yes")),
		Up:           key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/↓", "navigate")),
		Down:         key.NewBinding(key.WithKeys("down", "j")),
	}
}

// sessionRow is one entry of the sessions list. Every mount runs in its own
// process, so a row is always a registry record, here marks the ones this
// TUI started, which are the ones quitting asks about.
type sessionRow struct {
	id     string
	record datadir.Session
	here   bool
}

// tuiModel is the Bubble Tea model of `lmnt tui`. The screen is split into a
// sidebar (logo, sessions or wizard steps) and a content panel.
type tuiModel struct {
	rt     *tuiRuntime
	screen screen
	width  int
	height int
	keys   tuiKeyMap

	rows     []sessionRow
	cursor   int
	registry []datadir.Session

	wizard  wizardModel
	logs    viewport.Model
	logsID  string
	detail  string
	spinner spinner.Model

	status   string
	quitting bool
}

func newTUIModel(rt *tuiRuntime) tuiModel {
	return tuiModel{
		rt:      rt,
		keys:    newKeyMap(),
		wizard:  newWizard(rt),
		logs:    viewport.New(viewport.WithWidth(80), viewport.WithHeight(20)),
		spinner: spinner.New(spinner.WithSpinner(spinner.Dot)),
	}
}

func (m tuiModel) Init() tea.Cmd {
	return tea.Batch(m.spinner.Tick, refreshRegistry(m.rt), tickEvery())
}

func refreshRegistry(rt *tuiRuntime) tea.Cmd {
	return func() tea.Msg {
		records, err := rt.dir.Sessions()
		return registryMsg{records: records, err: err}
	}
}

func tickEvery() tea.Cmd {
	return tea.Tick(registryRefresh, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// contentWidth is the inner width of the content panel.
func (m tuiModel) contentWidth() int {
	return max(m.width-sidebarWidth-1-4, 30)
}

func (m tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.logs.SetWidth(m.contentWidth())
		m.logs.SetHeight(max(msg.Height-8, 3))
		m.wizard.resize(m.contentWidth(), msg.Height)

		return m, nil

	case tickMsg:
		m.rebuildRows()

		return m, tea.Batch(tickEvery(), refreshRegistry(m.rt))

	case registryMsg:
		if m.quitting && msg.err == nil && len(m.rt.startedHere(msg.records)) == 0 {
			return m, tea.Quit
		}

		if msg.err != nil {
			m.status = styleWarn.Render("registry: " + msg.err.Error())
		}
		m.registry = msg.records
		m.rebuildRows()

		return m, nil

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)

		return m, cmd

	case quitRequestMsg:
		return m.requestQuit()

	case signalQuitMsg:
		// A signal is not an answer to the unmount question, and the mounts
		// are not this process's to end, so they keep running.
		return m, tea.Quit

	case quitDeadlineMsg:
		if m.quitting {
			return m, tea.Quit
		}

		return m, nil

	case devicesMsg, promptMsg, listingDoneMsg, mountUpMsg, mountFailedMsg:
		return m.updateSession(msg)

	case logsChangedMsg:
		if m.screen == screenLogs && m.logsID == msg.id {
			m.refreshLogs()
		}

		return m, nil

	case clipboardMsg:
		if msg.err != nil {
			m.status = styleWarn.Render("copy " + msg.what + ": " + msg.err.Error())
		} else {
			m.status = styleDone.Render(msg.what + " copied to the clipboard")
		}

		return m, nil

	case tea.KeyPressMsg:
		return m.handleKey(msg)
	}

	if m.screen == screenWizard {
		return m, m.wizard.update(msg)
	}

	return m, nil
}

// updateSession lets the wizard follow the listing guest and the background
// mount it started.
func (m tuiModel) updateSession(msg tea.Msg) (tea.Model, tea.Cmd) {
	if m.screen != screenWizard {
		return m, nil
	}

	var settled string

	switch msg := msg.(type) {
	case mountUpMsg:
		settled = msg.id
		m.status = styleDone.Render(fmt.Sprintf("%s: %s share is up at %s", msg.id, strings.ToUpper(msg.record.Share), msg.record.URL))
	case mountFailedMsg:
		text := msg.err.Error()
		if msg.id != "" {
			text = msg.id + ": " + text
		}

		m.status = styleError.Render(text)
	}

	cmd := m.wizard.follow(msg)

	if m.wizard.finished || m.wizard.aborted {
		// The wizard is about to be replaced, so anything it wants to say
		// has to move to the status line first.
		if m.wizard.failure != "" {
			m.status = styleError.Render(m.wizard.failure)
		}

		m.screen = screenSessions
		m.wizard = newWizard(m.rt)
		m.rebuildRows()

		if settled != "" {
			m.selectRow(settled)
		}
	}

	m.rebuildRows()

	return m, cmd
}

func (m tuiModel) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch m.screen {
	case screenConfirmQuit:
		switch msg.String() {
		case "l", "enter":
			return m.quitLeaving()
		case "u", "y":
			return m.quitUnmounting()
		}

		m.screen = screenSessions

		return m, nil

	case screenWizard:
		if key.Matches(msg, m.keys.Quit) && msg.String() == "ctrl+c" {
			return m.requestQuit()
		}

		cmd := m.wizard.update(msg)
		if m.wizard.finished || m.wizard.aborted {
			m.screen = screenSessions
			m.wizard = newWizard(m.rt)
			m.rebuildRows()
		}

		return m, cmd

	case screenDetail, screenLogs:
		switch {
		case key.Matches(msg, m.keys.Back), key.Matches(msg, m.keys.Quit) && msg.String() == "q":
			m.screen = screenSessions
			return m, nil
		case key.Matches(msg, m.keys.Quit):
			return m.requestQuit()
		case m.screen == screenDetail && key.Matches(msg, m.keys.Logs):
			if row, ok := m.selected(); ok {
				m.logsID = row.id
				m.screen = screenLogs
				m.refreshLogs()
			}

			return m, nil
		}

		if m.screen == screenLogs {
			var cmd tea.Cmd
			m.logs, cmd = m.logs.Update(msg)

			return m, cmd
		}

		return m, nil
	}

	// Sessions screen.
	switch {
	case key.Matches(msg, m.keys.Quit):
		return m.requestQuit()

	case key.Matches(msg, m.keys.Up):
		m.move(-1)
		return m, nil

	case key.Matches(msg, m.keys.Down):
		m.move(1)
		return m, nil

	case key.Matches(msg, m.keys.New):
		m.screen = screenWizard
		m.wizard = newWizard(m.rt)
		m.wizard.resize(m.contentWidth(), m.height)

		return m, m.wizard.start()

	case key.Matches(msg, m.keys.Stop):
		return m.stopSelected()

	case key.Matches(msg, m.keys.Details):
		if row, ok := m.selected(); ok {
			m.detail = row.id
			m.screen = screenDetail
		}

		return m, nil

	case key.Matches(msg, m.keys.Logs):
		if row, ok := m.selected(); ok {
			m.logsID = row.id
			m.screen = screenLogs
			m.refreshLogs()
		}

		return m, nil

	case key.Matches(msg, m.keys.CopyURL):
		return m.copySelected("URL", func(r sessionRow) string { return rowInfo(r).URL })

	case key.Matches(msg, m.keys.CopyPassword):
		return m.copySelected("password", func(r sessionRow) string { return rowInfo(r).Password })
	}

	return m, nil
}

func (m *tuiModel) move(delta int) {
	if len(m.rows) == 0 {
		m.cursor = 0
		return
	}

	m.cursor = min(max(m.cursor+delta, 0), len(m.rows)-1)
}

// selectRow puts the cursor on the row with the given id if it is listed.
func (m *tuiModel) selectRow(id string) {
	for i, r := range m.rows {
		if r.id == id {
			m.cursor = i
			return
		}
	}
}

func (m tuiModel) copySelected(what string, pick func(sessionRow) string) (tea.Model, tea.Cmd) {
	row, ok := m.selected()
	if !ok {
		return m, nil
	}

	text := pick(row)
	if text == "" {
		m.status = styleMuted.Render("nothing to copy yet")
		return m, nil
	}

	return m, func() tea.Msg { return clipboardMsg{what: what, err: copyToClipboard(text)} }
}

func (m tuiModel) stopSelected() (tea.Model, tea.Cmd) {
	row, ok := m.selected()
	if !ok {
		return m, nil
	}

	if err := m.rt.dir.RequestStop(row.id); err != nil {
		m.status = styleError.Render(err.Error())
	} else {
		m.status = styleWarn.Render("asked " + row.id + " to unmount;, it takes a few seconds")
	}

	m.rebuildRows()

	return m, nil
}

// requestQuit quits at once when this TUI started nothing that is still
// running, and otherwise asks what should happen to those mounts. They live
// in their own processes, so leaving them up is a real choice rather than an
// abandonment.
func (m tuiModel) requestQuit() (tea.Model, tea.Cmd) {
	if m.quitting {
		return m, nil
	}

	if len(m.rt.startedHere(m.registry)) == 0 {
		return m, tea.Quit
	}

	m.screen = screenConfirmQuit

	return m, nil
}

// quitLeaving quits and leaves the mounts running, `lmnt mounts` and the next
// `lmnt tui` will find them again.
func (m tuiModel) quitLeaving() (tea.Model, tea.Cmd) {
	return m, tea.Quit
}

// quitUnmounting asks the mounts started here to unmount, then quits when
// they are gone (or when the deadline runs out).
func (m tuiModel) quitUnmounting() (tea.Model, tea.Cmd) {
	ids := m.rt.startedHere(m.registry)

	m.quitting = true
	m.screen = screenSessions

	for _, id := range ids {
		if err := m.rt.dir.RequestStop(id); err != nil {
			m.status = styleError.Render(err.Error())

			return m, tea.Quit
		}
	}

	m.status = styleWarn.Render(fmt.Sprintf("unmounting %d mount(s) before quitting…", len(ids)))
	m.rebuildRows()

	return m, tea.Batch(
		refreshRegistry(m.rt),
		tea.Tick(provider.StopTimeout+10*time.Second, func(time.Time) tea.Msg { return quitDeadlineMsg{} }),
	)
}

func (m tuiModel) selected() (sessionRow, bool) {
	if m.cursor < 0 || m.cursor >= len(m.rows) {
		return sessionRow{}, false
	}

	return m.rows[m.cursor], true
}

// rebuildRows lists every running mount and keeps the cursor on a valid row.
func (m *tuiModel) rebuildRows() {
	m.rows = m.rows[:0]

	here := make(map[string]bool)
	for _, id := range m.rt.startedHere(m.registry) {
		here[id] = true
	}

	for _, rec := range m.registry {
		m.rows = append(m.rows, sessionRow{id: rec.ID, record: rec, here: here[rec.ID]})
	}

	m.move(0)
}

func rowInfo(r sessionRow) (info struct{ URL, Password string }) {
	info.URL, info.Password = r.record.URL, r.record.Password

	return info
}

func uptime(since time.Time) string {
	return uptimeBetween(since, time.Now())
}

func uptimeBetween(since, until time.Time) string {
	if since.IsZero() {
		return ""
	}

	d := until.Sub(since).Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

// truncate keeps the tail of s, which is the informative part of a path.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}

	if n <= 1 {
		return "…"
	}

	return "…" + s[len(s)-(n-1):]
}

func truncateRight(s string, n int) string {
	if len(s) <= n {
		return s
	}

	return s[:max(n-1, 0)] + "…"
}

func (m *tuiModel) refreshLogs() {
	lines := m.rt.mountLog(m.logsID, logTailLines)
	if len(lines) == 0 {
		lines = []string{"(this mount has written no log yet)"}
	}

	m.logs.SetContent(strings.Join(lines, "\n"))
	m.logs.GotoBottom()
}

// View composes the sidebar and the content panel.
func (m tuiModel) View() tea.View {
	panelHeight := max(m.height-2, 0)

	sidebar := styleSidebar.Width(sidebarWidth).Height(panelHeight).Render(m.sidebarView())
	content := styleContent.Width(m.contentWidth()).Height(panelHeight).Render(m.contentView())

	v := tea.NewView(lipgloss.JoinHorizontal(lipgloss.Top, sidebar, content))
	v.AltScreen = true
	v.WindowTitle = "lmnt"

	return v
}

// sidebarView shows the logo and, depending on the screen, the wizard's
// steps or the list of sessions.
func (m tuiModel) sidebarView() string {
	var b strings.Builder

	b.WriteString(styleTitle.Render(logo) + "\n\n")

	if m.screen == screenWizard {
		b.WriteString(m.wizard.stepsView())
	} else {
		b.WriteString(m.sessionListView())
	}

	b.WriteString("\n" + hints("q", "quit"))

	return b.String()
}

// sessionListView is the sidebar list: one line per session with a state
// mark, the selected one highlighted. Foreign sessions carry a ⇡.
func (m tuiModel) sessionListView() string {
	var b strings.Builder

	b.WriteString(stylePrompt.Render("Mounts") + "\n")

	if len(m.rows) == 0 {
		b.WriteString(styleMuted.Render("  none yet") + "\n")
		b.WriteString(styleMuted.Render("  press n to mount a disk") + "\n")

		return b.String()
	}

	for i, r := range m.rows {
		mark, markStyle := rowMark(r)

		label := r.id
		lineStyle := styleText
		cursor := "  "
		if i == m.cursor {
			cursor = styleCursor.Render("> ")
			lineStyle = styleSelected
		}

		line := cursor + markStyle.Render(mark) + lineStyle.Render(label)
		if !r.here {
			line += styleMuted.Render(" ⇡")
		}

		b.WriteString(line + "\n")
	}

	return b.String()
}

// rowMark is the bullet of a mount in the sidebar.
func rowMark(r sessionRow) (string, lipgloss.Style) {
	if r.record.Mounted {
		return markDone, styleDone
	}

	return markActive, styleActive
}

// contentView is the right panel for the current screen.
func (m tuiModel) contentView() string {
	var body string

	switch m.screen {
	case screenWizard:
		body = m.wizard.view(m.spinner.View())
	case screenDetail:
		body = m.detailView(true)
	case screenLogs:
		body = stylePrompt.Render("Logs: "+m.logsID) + "\n\n" + m.logs.View() + "\n\n" + hints("↑/↓", "scroll", "esc", "back")
	case screenConfirmQuit:
		n := len(m.rt.startedHere(m.registry))
		body = m.detailView(false) + "\n" +
			styleBannerWarn.Render(fmt.Sprintf("⚠ %d mount(s) started here are still up.", n)) + "\n\n" +
			styleText.Render("Leave them running in the background, or unmount them first?") + "\n" +
			styleMuted.Render("Left running, they keep serving, `lmnt mounts` lists them and\n`lmnt stop` or the next `lmnt tui` ends them.") + "\n\n" +
			hints("l/enter", "leave running", "u", "unmount all", "esc", "back")
	default:
		body = m.detailView(false) + "\n" + m.sessionHints()
	}

	if m.status != "" {
		body = m.status + "\n\n" + body
	}

	return body
}

func (m tuiModel) sessionHints() string {
	row, ok := m.selected()
	if !ok {
		return hints("n", "new mount", "q", "quit")
	}

	pairs := []string{"↑/↓", "navigate", "n", "new mount", "s", "unmount", "enter", "details", "l", "logs"}
	if rowInfo(row).URL != "" {
		pairs = append(pairs, "c", "copy URL", "p", "copy password")
	}

	return hints(append(pairs, "q", "quit")...)
}

// detailView describes the selected session: a compact block on the
// sessions screen, the full one (with log lines) on the detail screen.
func (m tuiModel) detailView(full bool) string {
	id := m.detail
	if !full {
		row, ok := m.selected()
		if !ok {
			return stylePrompt.Render("No mounts") + "\n\n" +
				styleMuted.Render("Nothing is mounted yet. Press n to attach a disk image or a physical\ndisk, pick the device to mount and share it over SMB, FTP or SFTP.") + "\n"
		}

		id = row.id
	}

	for _, r := range m.rows {
		if r.id == id {
			return m.mountDetail(r, full)
		}
	}

	return styleMuted.Render("This mount is gone.") + "\n"
}

func (m tuiModel) mountDetail(r sessionRow, full bool) string {
	rec := r.record

	var b strings.Builder

	origin := fmt.Sprintf("background process, pid %d", rec.PID)
	if !r.here {
		origin = fmt.Sprintf("started outside this TUI, pid %d", rec.PID)
	}

	b.WriteString(stylePrompt.Render("Mount "+rec.ID) + "  " + styleMuted.Render(origin) + "\n")
	b.WriteString(separator(min(m.contentWidth(), 60)) + "\n")
	b.WriteString(kv("Target", rec.Target))
	b.WriteString(kv("Provider", rec.Provider))
	if rec.Device != "" {
		b.WriteString(kv("Device", rec.Device))
	}
	if rec.Mounted {
		b.WriteString(kv("Access", accessMode(rec.ReadOnly)))
	}
	b.WriteString(kv("Started", rec.Started.Format(time.DateTime)))
	b.WriteString(kv("Uptime", uptime(rec.Started)))

	if rec.Mounted {
		b.WriteString("\n" + styleBannerOK.Render("✓ "+strings.ToUpper(rec.Share)+" share is up") + "\n\n")
		b.WriteString(kv("URL", styleHighlight.Render(rec.URL)))
		b.WriteString(kv("User", rec.User))
		b.WriteString(kv("Password", rec.Password))
		for _, h := range rec.Hints {
			b.WriteString(kv("Note", h))
		}
	} else {
		b.WriteString("\n" + m.spinner.View() + " " + styleMuted.Render("booting…") + "\n")
	}

	if full {
		b.WriteString("\n" + styleMuted.Render("Recent log lines") + "\n" + separator(min(m.contentWidth(), 60)) + "\n")
		for _, line := range m.rt.mountLog(rec.ID, 10) {
			b.WriteString(styleMuted.Render(truncateRight(line, max(m.contentWidth(), 20))) + "\n")
		}

		b.WriteString("\n" + hints("esc", "back", "l", "full log", "s", "unmount", "q", "quit"))
	}

	return b.String()
}

func errorsIsInterrupted(err error) bool {
	return err != nil && strings.Contains(err.Error(), provider.ErrInterrupted.Error())
}
