package tui

import (
	"net/http"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/codehamr/codehamr/internal/llm"
)

// A prompt echo (or any content line) wider than the terminal must be wrapped
// before it reaches tea.Println: bubbletea dumps queued Println lines verbatim,
// so an over width line soft wraps in the terminal into rows the renderer never
// counted, drifting its cursor math and leaving a duplicated prompt fragment on
// screen. wrapForScrollback is the guard; every emitted line must fit the width.
func TestWrapForScrollbackCapsEveryLineToWidth(t *testing.T) {
	const width = 80
	longEcho := stylePrompt.Render("▌ ") + styleUser.Render(strings.Repeat("word ", 60))
	for line := range strings.SplitSeq(wrapForScrollback(longEcho, width), "\n") {
		if w := ansi.StringWidth(line); w > width {
			t.Fatalf("wrapped line width %d exceeds terminal width %d: %q", w, width, line)
		}
	}
}

// A long unbroken token (no spaces) must still be hard wrapped, or it would
// soft wrap and re trigger the drift the helper exists to prevent.
func TestWrapForScrollbackHardWrapsUnbrokenToken(t *testing.T) {
	const width = 40
	out := wrapForScrollback(strings.Repeat("z", 200), width)
	for line := range strings.SplitSeq(out, "\n") {
		if w := ansi.StringWidth(line); w > width {
			t.Fatalf("unbroken token line width %d exceeds %d", w, width)
		}
	}
	if got := len(strings.Split(out, "\n")); got < 5 {
		t.Fatalf("expected the 200-rune token split across multiple rows, got %d", got)
	}
}

// Terminals expand a literal tab to the next 8 column stop while ansi.Wrap
// counts it as one cell, so a tab bearing line (glamour preserves tabs inside
// code fences) could pass the width check yet physically overflow. Tabs must
// be expanded before counting; none may survive into the output.
func TestWrapForScrollbackExpandsTabs(t *testing.T) {
	const width = 20
	// 3 tabs + 16 chars: 19 counted cells with tab=1 (passes unwrapped), but
	// 12 + 16 = 28 real cells once expanded: must wrap.
	out := wrapForScrollback("\t\t\tabcdefghijklmnop", width)
	if strings.Contains(out, "\t") {
		t.Fatalf("tabs must not survive into scrollback output: %q", out)
	}
	if !strings.Contains(out, "\n") {
		t.Fatalf("the expanded over-width line must be wrapped: %q", out)
	}
	for line := range strings.SplitSeq(out, "\n") {
		if w := ansi.StringWidth(line); w > width {
			t.Fatalf("expanded line width %d exceeds %d: %q", w, width, line)
		}
	}
}

// TestRenderQueuedCapsVisualRows: queuedBodyCap bounds RENDERED rows, not
// logical lines. A single long echo line soft wraps inside lipgloss, so an
// uncapped wrap would let the queued box push the status bar off screen,
// exactly what the cap's own comment promises can't happen.
func TestRenderQueuedCapsVisualRows(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	m.width = 40
	m.queued = &queuedPrompt{echo: strings.Repeat("word ", 100)} // one ~500 cell logical line
	// title line + rounded border (2) + capped body + "+N more" line.
	if rows, maxRows := m.queuedHeight(), 1+2+queuedBodyCap+1; rows > maxRows {
		t.Fatalf("queued box renders %d rows for a single long line, want <= %d", rows, maxRows)
	}
	if !strings.Contains(m.renderQueued(), "more") {
		t.Fatal("overflow must collapse into a +N more line")
	}
}

// TestApplyContentExpandsTabs: the live streaming buffer must never carry
// tabs. View's ansi.Wrap counts a tab as one cell while the terminal expands
// it to an 8 column stop: the same width drift wrapForScrollback guards
// against for scrollback, on the path that renders every streamed token.
func TestApplyContentExpandsTabs(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	m.phase = phaseThinking
	m.applyContent(llm.Event{Kind: llm.EventContent, Content: "a\tb"})
	if got := m.streaming.String(); got != "a    b" {
		t.Fatalf("tabs must be expanded entering the live buffer, got %q", got)
	}
}

// width <= 0 (before the first WindowSizeMsg) is a no operation passthrough.
func TestWrapForScrollbackNoWidthIsNoop(t *testing.T) {
	s := "▌ some text"
	if got := wrapForScrollback(s, 0); got != s {
		t.Fatalf("zero width should pass through unchanged, got %q", got)
	}
}
