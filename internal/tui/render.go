package tui

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"
	"unicode"

	"github.com/charmbracelet/x/ansi"
	"github.com/mattn/go-runewidth"
)

// splashCode and splashHamr form the two tone "CODEHAMR" wordmark printed
// once at startup, pushed into scrollback via tea.Println; it scrolls up
// naturally as content arrives, so View() needs no hide on first content branch.
var splashCode = []string{
	" ██████  ██████  ██████   ███████ ",
	"██      ██    ██ ██   ██  ██      ",
	"██      ██    ██ ██   ██  █████   ",
	"██      ██    ██ ██   ██  ██      ",
	" ██████  ██████  ██████   ███████ ",
}

var splashHamr = []string{
	"██   ██  █████  ███    ███ ██████  ",
	"██   ██ ██   ██ ████  ████ ██   ██ ",
	"███████ ███████ ██ ████ ██ ██████  ",
	"██   ██ ██   ██ ██  ██  ██ ██   ██ ",
	"██   ██ ██   ██ ██      ██ ██   ██ ",
}

// appendLine queues a styled line for tea.Println on the next Update cycle,
// so the terminal, not us, owns the scrollback. scroll is a passive
// transcript: never rendered, replayed by handleResizeSettle after a width
// change, and read by tests.
func (m *Model) appendLine(s string) {
	m.scroll.WriteString(s + "\n")
	m.outbox = append(m.outbox, s)
}

// wrapForScrollback hard wraps every line of s to the terminal width before it
// goes to tea.Println. bubbletea's standard renderer dumps queued Println lines
// verbatim: unlike its View paint path it never truncates them: so a line
// wider than the terminal is soft wrapped by the terminal into extra physical
// rows the renderer never counted. Its cursor math then drifts and the prior
// frame's wrapped textarea rows survive un erased: the duplicated prompt
// fragment seen when submitting a long prompt. Mirrors the ansi.Wrap the live
// streaming view already applies. width <= 0 (no WindowSizeMsg yet) is a no operation.
func wrapForScrollback(s string, width int) string {
	if width <= 0 {
		return s
	}
	// Terminals advance a literal tab to the next 8 column stop, but ansi.Wrap
	// counts it as one cell, so a tab bearing line (glamour preserves tabs
	// inside code fences; a user echo can carry pasted ones) passes the width
	// check yet physically overflows: the exact drift this wrap exists to
	// prevent. Expand before counting; \t never occurs inside an ANSI escape
	// sequence, so a plain replace is safe on styled strings.
	s = strings.ReplaceAll(s, "\t", "    ")
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = ansi.Wrap(line, width, "")
	}
	return strings.Join(lines, "\n")
}

// flushStreaming ends the content phase: render the streaming buffer through
// glamour, queue it for tea.Println, reset. No operation on an empty buffer. Glamour
// errors fall back to raw so partial docs (unclosed code fence on cancel) survive.
func (m *Model) flushStreaming() {
	if m.streaming.Len() == 0 {
		return
	}
	raw := m.streaming.String()
	rendered, err := m.renderer.Render(raw)
	if err != nil {
		rendered = raw
	}
	// Strip glamour's trailing newline so tea.Println doesn't double space
	// the next prompt below the block.
	m.appendLine(strings.TrimRight(rendered, "\n"))
	m.streaming.Reset()
}

// chromeHeight is the non resizable vertical chrome (separator + status bar)
// recomputeLayout subtracts when capping the textarea against the window.
const chromeHeight = 2

// recomputeLayout caps the textarea height so a long paste can't push the
// status bar off screen: leave minViewport rows of breathing room above it.
// Cheap enough to run on every key press.
func (m *Model) recomputeLayout() {
	m.ta.SetHeight(max(1, min(m.visualPromptLines(), m.maxTextareaHeight())))
}

// maxTextareaHeight caps the textarea: terminal minus chrome, breathing room,
// the active popover, and the queued prompt box. Shared by recomputeLayout and
// preGrowTextarea.
func (m *Model) maxTextareaHeight() int {
	if m.height <= 0 {
		return 1
	}
	return max(1, m.height-minViewport-chromeHeight-m.popoverHeight()-m.queuedHeight())
}

// queuedBodyCap bounds the queued box body: only the first queuedBodyCap echo
// lines render (the rest collapse to a "+N more" line), so a long appended queue
// can't push the status bar off screen. Sibling to popoverCap.
const queuedBodyCap = 4

// queuedHeight is the rows renderQueued occupies in View(), 0 when nothing is
// queued. Subtracted from the textarea cap so a tall queued box can't crowd out
// the status bar, mirroring popoverHeight.
func (m Model) queuedHeight() int {
	if m.queued == nil {
		return 0
	}
	return strings.Count(m.renderQueued(), "\n") + 1
}

// renderQueued draws the pending prompt box shown above the divider while a turn
// runs: a faint title line plus a rounded panel with the collapsed echo, so the
// user sees what will auto submit when the turn ends and how to recall it. Empty
// when nothing is queued.
func (m Model) renderQueued() string {
	if m.queued == nil {
		return ""
	}
	// Width 3 leaves the border total at width 1, matching the divider's blank
	// last column (the macOS last column wrap guard in View). lipgloss wraps the
	// body to fit the inner width.
	inner := max(m.width-3, 1)
	// Wrap to the box's content width (inner minus Padding(0,1)) BEFORE capping,
	// so the cap bounds VISUAL rows: a single long echo line would otherwise
	// soft wrap inside lipgloss after the cap counted it as one line, and the
	// box could still push the status bar off screen.
	lines := strings.Split(ansi.Wrap(m.queued.echo, max(inner-2, 1), ""), "\n")
	extra := 0
	if len(lines) > queuedBodyCap {
		extra = len(lines) - queuedBodyCap
		lines = lines[:queuedBodyCap]
	}
	body := strings.Join(lines, "\n")
	if extra > 0 {
		body += fmt.Sprintf("\n+%d more", extra)
	}
	box := styleQueued.Width(inner).Render(body)
	return styleDim.Render("queued · Backspace to edit") + "\n" + box
}

// preGrowTextarea inflates Height to the cap *before* a KeyMsg is processed.
// bubbles/textarea's repositionView() scrolls the viewport down when the
// cursor drops below Height (e.g. a typed char wraps); since recomputeLayout
// runs only AFTER handleKey, without this the viewport stays anchored at the
// scrolled YOffset and hides the earliest wrap rows. Pre growing keeps the
// cursor in view so no scroll fires; recomputeLayout then shrinks back,
// leaving YOffset at 0 and every wrapped row visible from the top.
func (m *Model) preGrowTextarea() {
	cap := m.maxTextareaHeight()
	if cap > m.ta.Height() {
		m.ta.SetHeight(cap)
	}
}

// visualPromptLines counts the *visual* rows the textarea needs (a line that
// wraps to three screen rows wants a three row textarea), via wrapRows which
// mirrors bubbles/textarea's wrap() (see there for the grapheme cluster
// caveat). Reads DisplayValue so a chip counts as one line, not the hundreds
// its expanded content would.
func (m *Model) visualPromptLines() int {
	w := m.width - 4 // -2 for ta.SetWidth offset, -2 for "▌ " prompt
	if w < 1 {
		return 1
	}
	total := 0
	for line := range strings.SplitSeq(m.ta.DisplayValue(), "\n") {
		total += wrapRows(line, w)
	}
	if total < 1 {
		return 1
	}
	return total
}

// wrapRows mirrors bubbles/textarea.wrap()'s row count so the prompt's
// auto grow stays in lock step with what the textarea renders: word boundary
// aware with a hard wrap fallback for over wide words, plus the trailing
// cursor anchor row when content exactly fills the width.
// Adapted from charmbracelet/bubbles v0.20 textarea.
//
// Caveat: width is summed per rune (runewidth) rather than per grapheme cluster
// like bubbles' uniseg, so ASCII and CJK match exactly, but a multi rune cluster
// (a ZWJ family emoji, a keycap) can over count, harmlessly over growing the
// prompt by a row on emoji heavy input. Not worth pulling in uniseg for that.
func wrapRows(s string, width int) int {
	if width <= 0 {
		return 1
	}
	row := 0
	var lineW, wordW, spaces, charW int
	var hadWord bool

	for _, r := range s {
		charW = 0
		if unicode.IsSpace(r) {
			spaces++
		} else {
			charW = runewidth.RuneWidth(r)
			wordW += charW
			hadWord = true
		}

		switch {
		case spaces > 0:
			if lineW+wordW+spaces > width {
				row++
				lineW = wordW + spaces
			} else {
				lineW += wordW + spaces
			}
			wordW, spaces = 0, 0
			hadWord = false
		case hadWord && wordW+charW > width:
			// Space less word grew past the width; matches bubbles'
			// StringWidth(word)+lastCharLen check.
			if lineW > 0 {
				row++
			}
			lineW = wordW
			wordW = 0
			hadWord = false
		}
	}

	if lineW+wordW+spaces >= width {
		row++
	}
	return row + 1
}

// View renders only the live bottom region: in flight streaming tokens, the
// popover, a divider, the prompt, and the status bar. Everything else has
// already gone to scrollback via tea.Println, scrolled with the terminal's
// own wheel/PgUp like any shell session.
func (m Model) View() string {
	if m.width == 0 || m.suppressView {
		// No WindowSizeMsg yet, or a width resize mid drag: an empty frame
		// is safest. A 0 wide layout flashes garbled, and a real frame
		// mid drag races the renderer's stale flush window.
		return ""
	}
	var pieces []string
	if m.streaming.Len() > 0 {
		pieces = append(pieces, ansi.Wrap(m.streaming.String(), m.width, ""))
	}
	if q := m.renderQueued(); q != "" {
		pieces = append(pieces, q)
	}
	if p := m.renderPopover(); p != "" {
		pieces = append(pieces, p)
	}
	// Divider one cell narrower than m.width, and pieces joined with bare
	// "\n" (not lipgloss.JoinVertical's Left pad): a line ending in the last
	// column trips Apple Terminal.app's last column wrap (DECAWM xn)
	// inconsistently, drifting bubbletea's inline line count by one per frame:
	// a duplicated prompt line overwrites the status bar on macOS (other
	// terminals stay clean). Keeping the last column blank sidesteps it.
	pieces = append(pieces,
		styleDim.Render(strings.Repeat("─", max(m.width-1, 1))),
		m.ta.View(),
		m.renderStatusBar(),
	)
	return strings.Join(pieces, "\n")
}

// noPosixShell: tools.Bash runs every command through /bin/sh, which a native
// Windows host does not have, so every tool call there fails: a new user's
// first turn would be a confusing five failure streak ending in a nudge. Say
// it once, up front. Computed once at init: splashLines reruns on every
// resize, and main's pre TUI screen wipe erases anything printed earlier.
var noPosixShell = func() bool {
	if runtime.GOOS != "windows" {
		return false
	}
	_, err := os.Stat("/bin/sh")
	return err != nil
}()

// posixShellWarning is the one line splash warning for noPosixShell hosts.
const posixShellWarning = "  ⚠ no POSIX shell: the bash tool needs /bin/sh and will fail every call: run codehamr inside WSL2 or a devcontainer."

// splashLines builds the identity block for tea.Println. Below wordmarkWidth
// the ASCII art soft wraps into garbage, so collapse to plain text.
func (m Model) splashLines() []string {
	const wordmarkWidth = 70 // cells needed for CODE+HAMR side by side
	if m.width >= wordmarkWidth {
		lines := []string{""}
		for i := range splashCode {
			lines = append(lines, "  "+styleDim.Render(splashCode[i])+styleHamr.Render(splashHamr[i]))
		}
		lines = append(lines,
			"", styleDim.Render("  It's hamr time!"),
			"", styleDim.Render(fmt.Sprintf("  codehamr %s · %s @ %s",
				m.Version, m.cfg.ActiveProfile().LLM, m.cfg.Active)),
			"",
			styleDim.Render("  AI systems can make mistakes. Codehamr executes their commands with full shell and filesystem access."),
			styleDim.Render("  Run inside a devcontainer or VM where it cannot cause damage outside the sandbox."),
			"",
		)
		if noPosixShell {
			lines = append(lines, styleError.Render(posixShellWarning), "")
		}
		return lines
	}
	lines := []string{
		"",
		styleHamr.Render("  codehamr"),
		styleDim.Render(fmt.Sprintf("  %s · %s @ %s",
			m.Version, m.cfg.ActiveProfile().LLM, m.cfg.Active)),
		"",
		styleDim.Render("  AI shell with local access. Use a devcontainer or VM for isolation."),
		"",
	}
	if noPosixShell {
		lines = append(lines, styleError.Render(posixShellWarning), "")
	}
	return lines
}

func (m Model) renderStatusBar() string {
	sep := styleStatus.Render(" · ")
	segs := []string{backendLabel(m.cfg, m.connected)}

	if live := m.sessionTokens + m.streamingEstimate; live > 0 {
		segs = appendStatus(segs, humanTokens(live))
	}
	if label := m.phase.label(); label != "" {
		segs = appendStatus(segs, m.spinner.View()+" "+label)
		segs = appendStatus(segs, liveElapsed(time.Since(m.turnStart)))
	} else if mark := m.lastOutcome.marker(); mark != "" {
		// Frozen run summary at idle, until the next submit: outcome glyph,
		// wall clock duration, and the avg rate that divides into it.
		seg := mark + " " + liveElapsed(m.lastElapsed)
		if avg := humanRate(m.lastTokens, m.lastElapsed); avg != "" {
			seg += " · " + avg + " avg"
		}
		segs = appendStatus(segs, seg)
	}
	if m.status != "" {
		segs = appendStatus(segs, m.status)
	}
	return strings.Join(segs, sep)
}

// appendStatus wraps one segment in styleStatus and appends it, keeping
// renderStatusBar readable instead of repeating the wrap at each call site.
func appendStatus(segs []string, s string) []string {
	return append(segs, styleStatus.Render(s))
}
