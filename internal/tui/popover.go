package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// popoverOpen reports whether the slash autocomplete popover should render
// and consume ↑/↓/Tab/Esc.
func (m Model) popoverOpen() bool { return m.suggestOpen }

// currentSuggestion returns the highlighted popover row, or false when the
// popover is closed or empty.
func (m Model) currentSuggestion() (argOption, bool) {
	if !m.suggestOpen || len(m.suggest) == 0 {
		return argOption{}, false
	}
	return m.suggest[m.suggestIdx], true
}

// popoverMoveSelection wraps the selection index modulo the filtered list.
func (m Model) popoverMoveSelection(delta int) (tea.Model, tea.Cmd) {
	if len(m.suggest) == 0 {
		return m, nil
	}
	m.suggestIdx = (m.suggestIdx + delta + len(m.suggest)) % len(m.suggest)
	return m, nil
}

// refreshSuggest recomputes the popover from the textarea: command level before
// the first space, argument level after it.
func (m *Model) refreshSuggest() {
	text := m.ta.Value()
	if strings.Contains(text, "\n") || !strings.HasPrefix(text, "/") {
		m.closePopover()
		return
	}

	cmdName, rest, hasSpace := strings.Cut(text, " ")
	if !hasSpace {
		var ms []argOption
		for _, c := range commands {
			if strings.HasPrefix(c.name, text) {
				ms = append(ms, argOption{value: c.name, description: c.description})
			}
		}
		m.setPopover(ms, false, "")
		return
	}

	c := commandByName(cmdName)
	if c == nil || c.args == nil {
		m.closePopover()
		return
	}
	// Reload cfg on the cmd→arg transition (or a different arg level command) so
	// lists like /models <name> reflect external edits before submit. Errors are
	// silent: runSlash surfaces them on submit, not on every keystroke. Never
	// mid turn: a reload can rebuildClient and swap the live client
	// under the in flight turn; submit is phase gated anyway, so
	// runSlash's own reload covers correctness once the turn is over.
	if !m.phase.active() && (!m.suggestArgLevel || m.activeCmd != cmdName) {
		_ = m.reloadConfigFromDisk()
	}
	argPrefix := strings.TrimLeft(rest, " ")
	var ms []argOption
	for _, o := range c.args(*m) {
		if strings.HasPrefix(o.value, argPrefix) {
			ms = append(ms, o)
		}
	}
	m.setPopover(ms, true, cmdName)
}

// setPopover swaps in a new suggestion set. Selection priority: the current row
// (e.g. active profile), else the first.
func (m *Model) setPopover(ms []argOption, argLevel bool, cmdName string) {
	if len(ms) == 0 {
		m.closePopover()
		return
	}
	m.suggest = ms
	m.suggestArgLevel = argLevel
	m.activeCmd = cmdName
	m.suggestOpen = true
	m.suggestIdx = selectInitialIdx(ms)
}

// selectInitialIdx picks the starting row: the current row (e.g. active
// profile), else the first.
func selectInitialIdx(ms []argOption) int {
	for i, o := range ms {
		if o.current {
			return i
		}
	}
	return 0
}

func (m *Model) closePopover() {
	m.suggestOpen = false
	m.suggest = nil
	m.suggestIdx = 0
	m.suggestArgLevel = false
	m.activeCmd = ""
}

// popoverHeight is the rows the popover occupies in View(): 0 when closed,
// capped at popoverCap to leave the viewport breathing room.
func (m Model) popoverHeight() int {
	if !m.suggestOpen {
		return 0
	}
	return min(len(m.suggest), popoverCap)
}

// renderPopover draws the suggestion list: value flush left, description right
// aligned, one row each. Selection is a colour change (stylePopoverSelected);
// the current row is bold, no colour. No marker/background/box, plain text
// with a highlighted row renders cleanest in the terminal.
func (m Model) renderPopover() string {
	if !m.suggestOpen {
		return ""
	}
	// Window the rows around the selection: when suggestions exceed popoverCap,
	// slide start just enough to keep suggestIdx inside [start, start+h). Else
	// the highlighted row is off screen and the user commits a row they can't see.
	h := m.popoverHeight()
	start := 0
	if m.suggestIdx >= h {
		start = m.suggestIdx - h + 1
	}
	rows := m.suggest[start : start+h]
	var b strings.Builder
	for i, c := range rows {
		abs := start + i
		vw := lipgloss.Width(c.value)
		dw := lipgloss.Width(c.description)
		gap := max(m.width-vw-dw, 1)
		line := c.value + strings.Repeat(" ", gap) + c.description
		switch {
		case abs == m.suggestIdx:
			line = stylePopoverSelected.Render(line)
		case c.current:
			line = stylePopoverCurrent.Render(line)
		default:
			line = stylePopoverRow.Render(line)
		}
		b.WriteString(line)
		if i < len(rows)-1 {
			b.WriteByte('\n')
		}
	}
	return b.String()
}
