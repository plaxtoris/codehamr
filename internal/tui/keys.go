package tui

import (
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// quitArmText is the status bar hint after the first idle Ctrl+C; a const so
// the arm/disarm sites compare against the same string.
const quitArmText = "press Ctrl+C again to quit"

// queueSlashHint is the status bar hint when queuePrompt refuses to join a
// slash command with a queued prompt; a const so endTurn can clear exactly
// this hint once the turn ends and the advice is obsolete.
const queueSlashHint = "a slash command can't join a queued prompt: send it when the turn ends"

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Any key that isn't Ctrl+C clears a pending quit arm: no stray quits.
	if msg.Type != tea.KeyCtrlC && !m.quitArmedAt.IsZero() {
		m.quitArmedAt = time.Time{}
		if m.status == quitArmText {
			m.status = ""
		}
	}
	switch msg.Type {
	case tea.KeyCtrlC:
		return m.handleCtrlC()
	case tea.KeyCtrlL:
		// Clear the typed prompt and force a full redraw. Scrollback and
		// history stay; Ctrl+L tidies input, /clear starts over. Close the
		// popover with the text it filtered on, like every other clearing
		// path; left open, its stale selection would hijack the next Enter.
		m.ta.Reset()
		m.closePopover()
		return m, tea.ClearScreen
	case tea.KeyBackspace:
		// Mid turn Backspace on an empty prompt pulls a queued prompt back into
		// the textarea for editing; any other Backspace is an ordinary character
		// delete and falls through to the textarea.
		if m.phase.active() && m.queued != nil && m.ta.Value() == "" {
			return m.unqueuePrompt()
		}
	case tea.KeyCtrlD:
		// Ctrl+D on empty input = EOF = quit; no operation on nonempty so a
		// reflexive press never destroys a draft, and no operation mid turn (the
		// textarea is empty then, since submit resets it) so a reflexive press
		// can't quit without cancelling turnCtx and orphan a running tool's
		// process group. Ctrl+C is the mid turn escape.
		if m.ta.Value() == "" && !m.phase.active() {
			return m, tea.Quit
		}
		return m, nil
	case tea.KeyUp:
		if m.popoverOpen() {
			return m.popoverMoveSelection(-1)
		}
		// ↑ is prompt only: cursor up if a row is above, else walk
		// history. The terminal owns scrollback (PgUp / wheel native).
		if !m.cursorOnFirstLine() {
			break
		}
		return m.historyUp(), nil
	case tea.KeyDown:
		if m.popoverOpen() {
			return m.popoverMoveSelection(1)
		}
		if !m.cursorOnLastLine() {
			break
		}
		return m.historyDown(), nil
	case tea.KeyTab:
		return m.handleTab(msg)
	case tea.KeyShiftTab:
		if !m.popoverOpen() {
			break
		}
		return m.popoverMoveSelection(-1)
	case tea.KeyEsc:
		if m.popoverOpen() {
			return m.handleEscInPopover()
		}
	case tea.KeyEnter:
		return m.handleEnter(msg)
	}
	return m.forwardToTextarea(msg)
}

// forwardToTextarea passes msg to the textarea and refreshes the popover to
// match. The "let the textarea handle it" fallback shared by handleKey,
// handleTab, and Alt+Enter.
func (m Model) forwardToTextarea(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	m.ta, cmd = m.ta.Update(msg)
	m.refreshSuggest()
	return m, cmd
}

// setPromptText overwrites the textarea, parks the cursor at the end, and
// refreshes the popover. Centralises the SetValue + CursorEnd + refreshSuggest
// dance shared by Tab completion and the Enter/Esc arg popover transitions.
func (m *Model) setPromptText(s string) {
	m.ta.SetValue(s)
	m.ta.CursorEnd()
	m.refreshSuggest()
}

// handleCtrlC implements Ctrl+C's three level precedence: in flight cancel >
// popover close > quit arming. Each level fully handles the key, no fallthrough.
func (m Model) handleCtrlC() (tea.Model, tea.Cmd) {
	if m.cancel != nil {
		// abortTurn flushes the partial block so streamed output stays
		// visible, drains turn stats for a clean next banner, then unwinds
		// the per turn context.
		dbgWritef("cancel", "user cancelled the turn (Ctrl+C)")
		m.abortTurn(styleWarn.Render("✗ cancelled"))
		m.quitArmedAt = time.Time{}
		m.status = ""
		return m, nil
	}
	if m.popoverOpen() {
		m.closePopover()
		m.quitArmedAt = time.Time{}
		m.status = ""
		return m, nil
	}
	if !m.quitArmedAt.IsZero() && time.Now().Before(m.quitArmedAt) {
		return m, tea.Quit
	}
	m.quitArmedAt = time.Now().Add(3 * time.Second)
	m.status = quitArmText
	return m, tea.Tick(3*time.Second, func(time.Time) tea.Msg { return quitArmResetMsg{} })
}

// historyUp walks one step toward older entries; caller gates on cursor on
// first line and popover closed. Empty history is a no operation.
func (m Model) historyUp() Model {
	if len(m.promptHistory) == 0 {
		return m
	}
	// Leaving the live line: stash the unsent draft so ↓ can restore it.
	if m.histIdx == -1 {
		m.histDraft = m.ta.Entry()
	}
	if m.histIdx+1 < len(m.promptHistory) {
		m.histIdx++
	}
	m.ta.Restore(m.promptHistory[len(m.promptHistory)-1-m.histIdx])
	return m
}

// historyDown walks one step toward newer entries; -1 is the live draft
// sentinel and restores an empty textarea.
func (m Model) historyDown() Model {
	if m.histIdx == -1 {
		return m
	}
	m.histIdx--
	if m.histIdx == -1 {
		m.ta.Restore(m.histDraft)
	} else {
		m.ta.Restore(m.promptHistory[len(m.promptHistory)-1-m.histIdx])
	}
	return m
}

// handleTab implements the three Tab behaviours: seed "/" on an empty prompt
// (opens the command popover), complete a unique match when the popover is
// open, or cycle the selection. Nonempty non popover Tabs fall through to the
// textarea so a user initiated indent isn't swallowed.
func (m Model) handleTab(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if !m.popoverOpen() {
		if m.ta.Value() == "" {
			m.setPromptText("/")
			return m, nil
		}
		return m.forwardToTextarea(msg)
	}
	// One match at command level: complete to its name, plus a trailing
	// space if it takes args so the arg popover opens on next refresh.
	// Otherwise cycle the selection (zsh style).
	if !m.suggestArgLevel && len(m.suggest) == 1 {
		sel := m.suggest[0]
		tail := ""
		if c := commandByName(sel.value); c != nil && c.args != nil {
			tail = " "
		}
		m.setPromptText(sel.value + tail)
		return m, nil
	}
	return m.popoverMoveSelection(1)
}

// handleEscInPopover implements Esc inside the popover: arg level steps back to
// the command menu; command level closes the popover and clears the textarea.
func (m Model) handleEscInPopover() (tea.Model, tea.Cmd) {
	if m.suggestArgLevel {
		// Drop the trailing space and any typed arg prefix so refreshSuggest
		// lands on the command level list filtered to the command we were in.
		cmdName, _, _ := strings.Cut(m.ta.Value(), " ")
		m.setPromptText(cmdName)
		return m, nil
	}
	m.ta.Reset()
	m.closePopover()
	return m, nil
}

// handleEnter implements the four way Enter dispatch. Alt+Enter inserts a
// newline; mid turn Enter queues the prompt (queuePrompt); command level Enter
// on an args taking command advances to the arg popover (same model as Tab);
// plain Enter commits.
func (m Model) handleEnter(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.Alt {
		// Strip the Alt flag before forwarding: the textarea's InsertNewline
		// binding matches "enter", and an alt flagged KeyEnter ("alt+enter")
		// matches no binding at all, falling through to a rune insert no operation.
		return m.forwardToTextarea(tea.KeyMsg{Type: tea.KeyEnter})
	}
	if m.phase.active() {
		return m.queuePrompt()
	}
	sel, hasSel := m.currentSuggestion()
	// Command level Enter on an args taking command advances to the arg
	// popover (same shape as Tab on a unique match).
	if hasSel && !m.suggestArgLevel {
		if c := commandByName(sel.value); c != nil && c.args != nil {
			m.setPromptText(sel.value + " ")
			return m, nil
		}
	}
	// Plain commit. Value() expands chip labels to full paste content (→ LLM);
	// DisplayValue() keeps them collapsed (→ echo + history). A popover
	// selection overrides both so a command prefix + Enter submits the full
	// command and no chips leak into a slash command.
	var sendText, echoText string
	var entry promptEntry
	if hasSel {
		if m.suggestArgLevel {
			sendText = m.activeCmd + " " + sel.value
		} else {
			sendText = sel.value
		}
		echoText = sendText
		entry = promptEntry{display: sendText}
	} else {
		sendText = strings.TrimSpace(m.ta.Value())
		echoText = strings.TrimSpace(m.ta.DisplayValue())
		entry = m.ta.Entry()
	}
	if sendText == "" {
		return m, nil
	}
	m.ta.Reset()
	m.closePopover()
	return m.submit(sendText, echoText, entry)
}

// queuePrompt stashes the textarea contents to auto submit when the running turn
// finishes (see fireQueued), then clears the input so the next prompt can be
// typed. A second call appends newline joined, so a multi part instruction builds
// up in one slot and fires as a single turn. Empty input is a no operation, so a
// reflexive mid turn Enter stays silent as it always did. Value() feeds the LLM
// (chips expanded), DisplayValue() the visible echo (chips collapsed), the same
// split submit uses. Slash text queues like any prompt and routes through submit
// when it fires.
func (m Model) queuePrompt() (tea.Model, tea.Cmd) {
	send := strings.TrimSpace(m.ta.Value())
	echo := strings.TrimSpace(m.ta.DisplayValue())
	if send == "" {
		return m, nil
	}
	if m.queued == nil {
		m.queued = &queuedPrompt{send: send, echo: echo}
	} else {
		// Never newline join across a slash boundary. The joined text either
		// starts with "/" and fires as ONE slash command whose Fields split
		// swallows the appended prose as bogus args (a queued "/clear" plus a
		// follow up instruction wipes the conversation and silently drops the
		// instruction), or it ships the slash line to the LLM as prose. Refuse
		// the append and keep the draft in the textarea so nothing is lost.
		if strings.HasPrefix(m.queued.send, "/") || strings.HasPrefix(send, "/") {
			m.status = queueSlashHint
			return m, nil
		}
		// A fresh pointer, not an in place edit: Model is copied by value across
		// bubbletea, so mutating *m.queued would also reach through the discarded
		// prior copy that still aliases the same struct.
		m.queued = &queuedPrompt{
			send: m.queued.send + "\n" + send,
			echo: m.queued.echo + "\n" + echo,
		}
	}
	m.ta.Reset()
	m.closePopover()
	return m, nil
}

// unqueuePrompt pulls the queued prompt back into the textarea and clears the
// slot, so a queued follow up can be edited or dropped with one Backspace.
// Reversible counterpart to queuePrompt; Ctrl+C still cancels the turn, a
// separate concern. The expanded text returns (no chip), content intact.
func (m Model) unqueuePrompt() (tea.Model, tea.Cmd) {
	send := m.queued.send
	m.queued = nil
	m.setPromptText(send)
	return m, nil
}
