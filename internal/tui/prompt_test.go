package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// newChippablePrompt returns a realistically sized promptInput. Width matters:
// cursor navigation walks the wrapped grid; height is irrelevant here.
func newChippablePrompt() promptInput {
	p := newPromptInput()
	p.SetWidth(80)
	p.SetHeight(20)
	return p
}

// pasteKey builds the bracketed paste KeyMsg bubbletea emits on paste.
// Paste=true is the flag Update keys off of.
func pasteKey(s string) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s), Paste: true}
}

// makePaste returns a string with exactly n lines (n 1 newlines).
func makePaste(n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = fmt.Sprintf("line %d", i+1)
	}
	return strings.Join(parts, "\n")
}

// TestSetCursorRuneOffsetCrossesSoftWrappedLine: walking to a target row must
// traverse a soft wrapped logical line in between. bubbles' CursorUp inside a
// soft wrapped line changes only the visual position, not Line(), so a bail
// keyed on Line() alone reads that as "step did nothing" and strands the
// cursor on the wrong logical row (the audit bug: deleting a chip above a
// soft wrapped line dropped the cursor into the user's instruction text).
func TestSetCursorRuneOffsetCrossesSoftWrappedLine(t *testing.T) {
	p := newPromptInput()
	p.SetWidth(20)
	p.SetHeight(20)
	// Row 1 soft wraps into several visual rows at width 20.
	p.SetValue("short\n" + strings.Repeat("a", 100) + "\ntail line")
	p.ta.CursorEnd() // start at the bottom, target near the top

	p.setCursorRuneOffset(2)
	if got := p.cursorRuneOffset(); got != 2 {
		t.Fatalf("cursor landed at rune offset %d, want 2 (walk bailed inside the soft-wrapped row)", got)
	}
}

// TestSmallPasteStaysInline: a paste below threshold stays raw, no chip.
func TestSmallPasteStaysInline(t *testing.T) {
	p := newChippablePrompt()
	small := makePaste(3)
	p, _ = p.Update(pasteKey(small))

	if len(p.spans) != 0 {
		t.Fatalf("small paste must not create chip, got spans=%+v", p.spans)
	}
	if got := p.DisplayValue(); got != small {
		t.Fatalf("display value should equal raw paste, got %q", got)
	}
	if got := p.Value(); got != small {
		t.Fatalf("expanded value should equal raw paste, got %q", got)
	}
}

// TestLargePasteBecomesChip: a ≥pasteChipMinLines paste collapses to one chip.
// DisplayValue shows the label, Value expands back to the original.
func TestLargePasteBecomesChip(t *testing.T) {
	p := newChippablePrompt()
	big := makePaste(pasteChipMinLines + 4)
	p, _ = p.Update(pasteKey(big))

	if len(p.spans) != 1 {
		t.Fatalf("big paste should produce 1 chip, got %d spans", len(p.spans))
	}
	wantLabel := fmt.Sprintf("[Pasted text +%d lines]", pasteChipMinLines+4)
	if got := p.DisplayValue(); got != wantLabel {
		t.Fatalf("DisplayValue = %q, want %q", got, wantLabel)
	}
	if got := p.Value(); got != big {
		t.Fatalf("Value should expand chip back to original content")
	}
}

// TestBackspaceAtChipEndRemovesWholeChip: Backspace with the cursor right after
// the label deletes the whole chip and leaves surrounding text intact.
func TestBackspaceAtChipEndRemovesWholeChip(t *testing.T) {
	p := newChippablePrompt()
	// Prefix, big paste, suffix.
	p, _ = p.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("before ")})
	p, _ = p.Update(pasteKey(makePaste(10)))
	p, _ = p.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(" after")})

	// Cursor to the chip's trailing edge.
	p.setCursorRuneOffset(p.spans[0].end)

	p, _ = p.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	if len(p.spans) != 0 {
		t.Fatalf("Backspace at chip.end must remove the chip; spans=%+v", p.spans)
	}
	if got := p.DisplayValue(); got != "before  after" {
		t.Fatalf("after delete value = %q, want 'before  after'", got)
	}
}

// TestDeleteAtChipStartRemovesWholeChip: forward Delete with the cursor right
// before the label removes the whole chip.
func TestDeleteAtChipStartRemovesWholeChip(t *testing.T) {
	p := newChippablePrompt()
	p, _ = p.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("prefix")})
	p, _ = p.Update(pasteKey(makePaste(12)))

	// Cursor at chip start.
	p.setCursorRuneOffset(p.spans[0].start)

	p, _ = p.Update(tea.KeyMsg{Type: tea.KeyDelete})
	if len(p.spans) != 0 {
		t.Fatalf("Delete at chip.start must remove the chip; spans=%+v", p.spans)
	}
	if got := p.DisplayValue(); got != "prefix" {
		t.Fatalf("after delete value = %q, want 'prefix'", got)
	}
}

// TestLeftArrowAtChipEndJumpsToStart: ← jumps the cursor across the whole chip
// in one keystroke; it never lands on interior positions.
func TestLeftArrowAtChipEndJumpsToStart(t *testing.T) {
	p := newChippablePrompt()
	p, _ = p.Update(pasteKey(makePaste(9)))
	p.setCursorRuneOffset(p.spans[0].end)

	before := p.cursorRuneOffset()
	wantStart := p.spans[0].start
	p, _ = p.Update(tea.KeyMsg{Type: tea.KeyLeft})
	after := p.cursorRuneOffset()

	if before == after {
		t.Fatal("← should have moved the cursor")
	}
	if after != wantStart {
		t.Fatalf("← at chip.end should land at chip.start=%d, got %d", wantStart, after)
	}
	if len(p.spans) != 1 {
		t.Fatal("← must not delete the chip")
	}
}

// TestRightArrowAtChipStartJumpsToEnd: → skips the whole chip in one keystroke.
func TestRightArrowAtChipStartJumpsToEnd(t *testing.T) {
	p := newChippablePrompt()
	p, _ = p.Update(pasteKey(makePaste(9)))
	p.setCursorRuneOffset(p.spans[0].start)

	wantEnd := p.spans[0].end
	p, _ = p.Update(tea.KeyMsg{Type: tea.KeyRight})
	after := p.cursorRuneOffset()

	if after != wantEnd {
		t.Fatalf("→ at chip.start should land at chip.end=%d, got %d", wantEnd, after)
	}
	if len(p.spans) != 1 {
		t.Fatal("→ must not delete the chip")
	}
}

// TestTwoChipsTrackedIndependently: two pastes make two chips; deleting the
// first leaves the second intact with its span shifted left.
func TestTwoChipsTrackedIndependently(t *testing.T) {
	p := newChippablePrompt()
	p, _ = p.Update(pasteKey(makePaste(8)))
	p, _ = p.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(" mid ")})
	p, _ = p.Update(pasteKey(makePaste(15)))

	if len(p.spans) != 2 {
		t.Fatalf("expected 2 chips, got %d", len(p.spans))
	}
	if p.spans[0].start >= p.spans[1].start {
		t.Fatal("chip spans must be sorted left-to-right")
	}

	// Delete the first chip.
	p.setCursorRuneOffset(p.spans[0].end)
	p, _ = p.Update(tea.KeyMsg{Type: tea.KeyBackspace})

	if len(p.spans) != 1 {
		t.Fatalf("after deleting first chip, one should remain; got %d", len(p.spans))
	}
	// The remaining chip is the one with 15 lines.
	content, ok := p.store[p.spans[0].id]
	if !ok {
		t.Fatal("remaining span has no store entry")
	}
	if content.lines != 15 {
		t.Fatalf("remaining chip should be the 15-line one, got %d", content.lines)
	}
}

// TestDamagedLabelAmongIdenticalChipsDropsWholeGroup: two pastes with the same
// line count render identical labels. A word delete at the boundary (Ctrl+W,
// which handleChipKey doesn't claim and snapCursorOutOfChip can't prevent)
// damages one label; in order re binding would then map the surviving label to
// the FIRST span's paste, silently sending the wrong content on submit. The
// spans are indistinguishable, so reconcile drops the whole label group.
func TestDamagedLabelAmongIdenticalChipsDropsWholeGroup(t *testing.T) {
	mkLines := func(prefix string, n int) string {
		parts := make([]string, n)
		for i := range parts {
			parts[i] = fmt.Sprintf("%s %d", prefix, i+1)
		}
		return strings.Join(parts, "\n")
	}
	p := newChippablePrompt()
	p, _ = p.Update(pasteKey(mkLines("alpha", 8)))
	p, _ = p.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(" mid ")})
	p, _ = p.Update(pasteKey(mkLines("beta", 8)))
	if len(p.spans) != 2 {
		t.Fatalf("precondition: expected 2 chips, got %d", len(p.spans))
	}
	// Word delete backward from the first chip's end eats into its label.
	p.setCursorRuneOffset(p.spans[0].end)
	p, _ = p.Update(tea.KeyMsg{Type: tea.KeyCtrlW})
	if len(p.spans) != 0 {
		t.Fatalf("indistinguishable identical-label chips must drop as a group, got %d spans", len(p.spans))
	}
	if v := p.Value(); strings.Contains(v, "alpha 1") || strings.Contains(v, "beta 1") {
		t.Fatalf("no paste content may expand after the group drop: %q", v)
	}
}

// TestDamagedLabelUniqueChipDropsOnlyItself: the group drop above must not
// over trigger; a damaged label with no twin drops alone and the other chip
// keeps its mapping.
func TestDamagedLabelUniqueChipDropsOnlyItself(t *testing.T) {
	p := newChippablePrompt()
	p, _ = p.Update(pasteKey(makePaste(8)))
	p, _ = p.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(" mid ")})
	p, _ = p.Update(pasteKey(makePaste(15)))
	p.setCursorRuneOffset(p.spans[0].end)
	p, _ = p.Update(tea.KeyMsg{Type: tea.KeyCtrlW})
	if len(p.spans) != 1 {
		t.Fatalf("only the damaged chip should drop, got %d spans", len(p.spans))
	}
	if c := p.store[p.spans[0].id]; c.lines != 15 {
		t.Fatalf("survivor should be the 15-line chip, got %d", c.lines)
	}
}

// TestValueExpandsAllChips: Value() interleaves surrounding text with full
// paste contents in order.
func TestValueExpandsAllChips(t *testing.T) {
	p := newChippablePrompt()
	paste1 := makePaste(9)
	paste2 := makePaste(11)
	p, _ = p.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("A ")})
	p, _ = p.Update(pasteKey(paste1))
	p, _ = p.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(" B ")})
	p, _ = p.Update(pasteKey(paste2))
	p, _ = p.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(" C")})

	want := "A " + paste1 + " B " + paste2 + " C"
	if got := p.Value(); got != want {
		t.Fatalf("Value expansion wrong:\nwant %q\ngot  %q", want, got)
	}
}

// TestTypingShiftsSpans: typing before a chip shifts its span right by one;
// reconcile tracks it automatically.
func TestTypingShiftsSpans(t *testing.T) {
	p := newChippablePrompt()
	p, _ = p.Update(pasteKey(makePaste(10)))
	origStart := p.spans[0].start
	if origStart != 0 {
		t.Fatalf("precondition: chip starts at 0, got %d", origStart)
	}

	// Move cursor home, type a character.
	p.setCursorRuneOffset(0)
	p, _ = p.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})

	if len(p.spans) != 1 {
		t.Fatalf("typing before chip must not destroy it, spans=%+v", p.spans)
	}
	if p.spans[0].start != 1 {
		t.Fatalf("chip start should shift to 1, got %d", p.spans[0].start)
	}
}

// TestEntryRestoreRoundTrip: Entry()+Restore() fully recovers chip state:
// display text, spans, expanded Value(). Backs ↑/↓ history replay.
func TestEntryRestoreRoundTrip(t *testing.T) {
	p := newChippablePrompt()
	paste := makePaste(12)
	p, _ = p.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("pre ")})
	p, _ = p.Update(pasteKey(paste))
	p, _ = p.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(" post")})

	entry := p.Entry()
	wantDisplay := p.DisplayValue()
	wantValue := p.Value()

	q := newChippablePrompt()
	q.Restore(entry)

	if got := q.DisplayValue(); got != wantDisplay {
		t.Fatalf("DisplayValue mismatch after Restore: %q vs %q", got, wantDisplay)
	}
	if got := q.Value(); got != wantValue {
		t.Fatalf("Value mismatch after Restore")
	}
	if len(q.spans) != 1 {
		t.Fatalf("expected 1 span after Restore, got %d", len(q.spans))
	}
}

// TestResetClearsChips: Reset empties store and spans, leaving only new text.
func TestResetClearsChips(t *testing.T) {
	p := newChippablePrompt()
	p, _ = p.Update(pasteKey(makePaste(10)))
	if len(p.spans) != 1 {
		t.Fatal("precondition: one chip")
	}
	p.Reset()
	if p.DisplayValue() != "" {
		t.Fatalf("Reset should empty the textarea, got %q", p.DisplayValue())
	}
	if len(p.spans) != 0 || len(p.store) != 0 {
		t.Fatal("Reset should clear chip state")
	}
}

// TestSetValueClearsChips: SetValue installs plain text and drops prior chips,
// used by slash popover Tab completion, where chips can't be part of a replacement.
func TestSetValueClearsChips(t *testing.T) {
	p := newChippablePrompt()
	p, _ = p.Update(pasteKey(makePaste(10)))
	p.SetValue("/models work")
	if p.DisplayValue() != "/models work" {
		t.Fatalf("SetValue should install text verbatim, got %q", p.DisplayValue())
	}
	if len(p.spans) != 0 {
		t.Fatal("SetValue should drop any existing chips")
	}
}

// TestBackspaceNotAtChipBoundaryFallsThrough: Backspace away from a chip boundary
// deletes one char normally, chip untouched.
func TestBackspaceNotAtChipBoundaryFallsThrough(t *testing.T) {
	p := newChippablePrompt()
	p, _ = p.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("hello")})
	p, _ = p.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(" ")})
	p, _ = p.Update(pasteKey(makePaste(9)))
	// Cursor ends past the chip; backspace must remove "X", not the chip.
	p, _ = p.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("X")})

	p, _ = p.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	if len(p.spans) != 1 {
		t.Fatal("Backspace after non-chip character must not remove the chip")
	}
	if strings.HasSuffix(p.DisplayValue(), "X") {
		t.Fatal("Backspace should have removed the trailing 'X'")
	}
}

// TestPageKeysMoveCursorByHeight: PgUp/PgDn move the cursor by one prompt height.
// bubbles/textarea ships an empty viewport keymap, so without our handling
// these keys are no operations (mouse wheel already scrolls via the MouseMsg path).
func TestPageKeysMoveCursorByHeight(t *testing.T) {
	p := newChippablePrompt()
	// Fill with many rows. SetValue installs all at once, avoiding the chip
	// threshold and per line typing.
	var b strings.Builder
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&b, "row%02d\n", i)
	}
	p.SetValue(b.String())
	p.CursorEnd()
	startRow := p.ta.Line()
	if startRow < 10 {
		t.Fatalf("precondition: cursor should be deep into the content, got row %d", startRow)
	}

	// PgUp moves the cursor up; repositionView scrolls the viewport with it.
	p, _ = p.Update(tea.KeyMsg{Type: tea.KeyPgUp})
	if p.ta.Line() >= startRow {
		t.Fatalf("PgUp should move cursor up from row %d, ended at %d",
			startRow, p.ta.Line())
	}

	upRow := p.ta.Line()
	p, _ = p.Update(tea.KeyMsg{Type: tea.KeyPgDown})
	if p.ta.Line() <= upRow {
		t.Fatalf("PgDn should move cursor down from row %d, ended at %d",
			upRow, p.ta.Line())
	}
}

// TestCarriageReturnLineEndings: lone-\r separators (old mac style, some VS Code
// TERM setups) must still yield a correct chip line count. bubbles/textarea
// splits only on \n, so we count separators ourselves.
func TestCarriageReturnLineEndings(t *testing.T) {
	p := newChippablePrompt()
	// 10 line paste, \r separators only.
	paste := strings.Repeat("line\r", 9) + "end"
	p, _ = p.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(paste), Paste: true})

	if len(p.spans) != 1 {
		t.Fatalf("\\r-separated paste should still chip; spans=%+v", p.spans)
	}
	wantLabel := "[Pasted text +10 lines]"
	if got := p.DisplayValue(); got != wantLabel {
		t.Fatalf("label should report the real line count; got %q want %q",
			got, wantLabel)
	}
}

// TestCRLFLineEndings: Windows \r\n counts each line once, not twice.
func TestCRLFLineEndings(t *testing.T) {
	p := newChippablePrompt()
	paste := strings.Repeat("line\r\n", 9) + "end"
	p, _ = p.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(paste), Paste: true})

	if len(p.spans) != 1 {
		t.Fatal("CRLF paste should chip")
	}
	wantLabel := "[Pasted text +10 lines]"
	if got := p.DisplayValue(); got != wantLabel {
		t.Fatalf("label line count wrong for CRLF; got %q want %q",
			got, wantLabel)
	}
}

// TestPasteWithoutFlagButWithNewlineStillChips: some terminals omit the
// bracketed paste flag, yet multi line content in one KeyMsg can't come from
// typing (the rune collector breaks on \n), so we treat it as a paste anyway.
func TestPasteWithoutFlagButWithNewlineStillChips(t *testing.T) {
	p := newChippablePrompt()
	msg := tea.KeyMsg{
		Type:  tea.KeyRunes,
		Runes: []rune(makePaste(10)),
		Paste: false, // no flag
	}
	p, _ = p.Update(msg)
	if len(p.spans) != 1 {
		t.Fatalf("paste-like KeyMsg without Paste flag should still chip; spans=%+v", p.spans)
	}
}

// TestLongSingleLinePasteChipsByCharCount: a long single line blob (zero
// newlines) still collapses: char threshold catches minified JSON and
// one line stack traces that line count alone would miss.
func TestLongSingleLinePasteChipsByCharCount(t *testing.T) {
	p := newChippablePrompt()
	big := strings.Repeat("x", 500)
	p, _ = p.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(big), Paste: true})
	if len(p.spans) != 1 {
		t.Fatalf("long single-line paste should chip by char threshold; spans=%+v", p.spans)
	}
	wantLabel := "[Pasted text +1 lines]"
	if got := p.DisplayValue(); got != wantLabel {
		t.Fatalf("DisplayValue = %q, want %q", got, wantLabel)
	}
}

// TestBackspaceImmediatelyAfterChipRemovesIt: atomic delete regression guard,
// after a paste the cursor sits at chip.end, so one Backspace deletes the chip
// without any manual cursor setup.
func TestBackspaceImmediatelyAfterChipRemovesIt(t *testing.T) {
	p := newChippablePrompt()
	p, _ = p.Update(pasteKey(makePaste(10)))
	// InsertString leaves the cursor at chip.end, tripping the atomic path.
	p, _ = p.Update(tea.KeyMsg{Type: tea.KeyBackspace})

	if len(p.spans) != 0 {
		t.Fatalf("single Backspace right after paste should remove chip, spans=%+v", p.spans)
	}
	if p.DisplayValue() != "" {
		t.Fatalf("prompt should be empty after chip delete, got %q", p.DisplayValue())
	}
}

// TestPasteIntoChipInteriorKeepsBothPastes: a paste while the cursor sits inside
// an existing chip's label must not splice the new label into the old one.
// insertChip snaps out of the chip first; without that, reconcile loses a
// chip's content and the LLM payload is silently wrong.
func TestPasteIntoChipInteriorKeepsBothPastes(t *testing.T) {
	p := newChippablePrompt()
	n := pasteChipMinLines + 4
	a := strings.Repeat("AAAA\n", n-1) + "Aend"
	b := strings.Repeat("BBBB\n", n-1) + "Bend"
	p, _ = p.Update(pasteKey(a))
	if len(p.spans) != 1 {
		t.Fatalf("setup: expected 1 chip, got %d", len(p.spans))
	}
	p.setCursorRuneOffset(p.spans[0].start + 3) // strictly inside chip A's label
	p, _ = p.Update(pasteKey(b))
	if len(p.spans) != 2 {
		t.Fatalf("expected 2 intact chips after interior paste, got %d", len(p.spans))
	}
	v := p.Value()
	if !strings.Contains(v, "Aend") {
		t.Fatalf("paste A content lost: %q", v)
	}
	if !strings.Contains(v, "Bend") {
		t.Fatalf("paste B content lost/cross-mapped: %q", v)
	}
}

// TestTypingIntoChipInteriorKeepsContent: typing a rune while the cursor sits
// inside a chip label, when two chips share an identical label, must not split
// the label and cross map the survivor to the wrong paste. Update snaps out of
// the chip before delegating the rune to the textarea.
func TestTypingIntoChipInteriorKeepsContent(t *testing.T) {
	p := newChippablePrompt()
	n := pasteChipMinLines + 4
	a := strings.Repeat("AAAA\n", n-1) + "Aend" // n lines
	b := strings.Repeat("BBBB\n", n-1) + "Bend" // n lines -> identical label
	p, _ = p.Update(pasteKey(a))
	p, _ = p.Update(pasteKey(b))
	if len(p.spans) != 2 {
		t.Fatalf("setup: expected 2 chips, got %d", len(p.spans))
	}
	p.setCursorRuneOffset(p.spans[0].start + 3) // strictly inside the first label
	p, _ = p.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("Z")})
	v := p.Value()
	if !strings.Contains(v, "Aend") {
		t.Fatalf("paste A content lost/cross-mapped: %q", v)
	}
	if !strings.Contains(v, "Bend") {
		t.Fatalf("paste B content lost/cross-mapped: %q", v)
	}
}
