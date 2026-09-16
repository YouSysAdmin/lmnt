package cli

import (
	"strings"

	"charm.land/lipgloss/v2"
)

// Palette and layout styles of the TUI: a sidebar on the left with the logo
// and the navigation, a content panel on the right, one accent colour and
// semantic colours for states. Bubble Tea downsamples the colours for
// terminals that cannot show them and honours NO_COLOR.
var (
	colorPrimary   = lipgloss.Color("205") // pink
	colorSuccess   = lipgloss.Color("42")  // green
	colorMuted     = lipgloss.Color("241") // gray
	colorError     = lipgloss.Color("196") // red
	colorHighlight = lipgloss.Color("39")  // blue
	colorCyan      = lipgloss.Color("86")
	colorWarn      = lipgloss.Color("214") // orange
	colorText      = lipgloss.Color("252")
	colorHint      = lipgloss.Color("245")

	styleTitle     = lipgloss.NewStyle().Bold(true).Foreground(colorPrimary)
	styleActive    = lipgloss.NewStyle().Bold(true).Foreground(colorPrimary)
	styleDone      = lipgloss.NewStyle().Foreground(colorSuccess)
	styleMuted     = lipgloss.NewStyle().Foreground(colorMuted)
	styleError     = lipgloss.NewStyle().Bold(true).Foreground(colorError)
	styleWarn      = lipgloss.NewStyle().Foreground(colorWarn)
	styleHighlight = lipgloss.NewStyle().Foreground(colorHighlight)
	styleText      = lipgloss.NewStyle().Foreground(colorText)
	styleHint      = lipgloss.NewStyle().Foreground(colorHint)
	styleHintKey   = lipgloss.NewStyle().Bold(true).Foreground(colorText)
	styleCursor    = lipgloss.NewStyle().Foreground(colorPrimary)
	styleSelected  = lipgloss.NewStyle().Bold(true).Foreground(colorCyan)
	stylePrompt    = lipgloss.NewStyle().Bold(true).Foreground(colorHighlight)
	styleLabel     = lipgloss.NewStyle().Foreground(colorMuted).Width(12)
	styleValue     = lipgloss.NewStyle().Foreground(colorText)

	styleSidebar = lipgloss.NewStyle().
			BorderRight(true).
			BorderStyle(lipgloss.NormalBorder()).
			BorderForeground(colorMuted).
			Padding(1, 2)

	styleContent = lipgloss.NewStyle().Padding(1, 2)

	styleBannerOK = lipgloss.NewStyle().
			Bold(true).
			Foreground(colorSuccess).
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colorSuccess).
			Padding(0, 1)

	styleBannerWarn = lipgloss.NewStyle().
			Bold(true).
			Foreground(colorWarn).
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colorWarn).
			Padding(0, 1)
)

// sidebarWidth is the inner width of the sidebar, the border adds one column.
const sidebarWidth = 30

// logo is "LMNT" in box-drawing letters, three lines high.
const logo = "╦  ╔╦╗╔╗╔╔╦╗\n" +
	"║  ║║║║║║ ║ \n" +
	"╩═╝╩ ╩╝╚╝ ╩ "

// separator draws a thin horizontal rule of the given width.
func separator(width int) string {
	return styleMuted.Render(strings.Repeat("─", max(width, 1)))
}

// kv renders one "Label:  value" row of a detail block.
func kv(label, value string) string {
	return styleLabel.Render(label+":") + " " + styleValue.Render(value) + "\n"
}

// hints renders a keybinding help line: "key desc", with the keys
// standing out from their descriptions.
func hints(pairs ...string) string {
	var parts []string
	for i := 0; i+1 < len(pairs); i += 2 {
		parts = append(parts, styleHintKey.Render(pairs[i])+" "+styleHint.Render(pairs[i+1]))
	}

	return strings.Join(parts, "  ")
}

// markDone, markActive and markPending are the sidebar list bullets.
const (
	markDone    = "✓ "
	markActive  = "▸ "
	markPending = "· "
	markFailed  = "✗ "
	markWarn    = "⚠ "
)
