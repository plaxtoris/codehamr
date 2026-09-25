package tui

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// pasteChipMinLines: line threshold above which a paste collapses into a chip;
// shorter pastes stay inline and readable. pasteChipMinChars: char fallback so
// long single line blobs (minified JSON, a huge log line) chip too.
const (
	pasteChipMinLines = 5
	pasteChipMinChars = 200
)

// promptInput wraps bubbles/textarea with an atomic chip model. A large paste
// collapses into a single inline label [Pasted text +N lines] that acts as one
// character for cursor moves and deletion; the original text lives in store,
// keyed by id, and is expanded back by Value() on LLM submission.
type promptInput struct {
	ta     textarea.Model
	store  map[int]chipContent
	spans  []chipSpan
	nextID int
}

// chipContent is the payload behind a chip, kept in store by id so the visible
// label can collapse while the real text survives for Value() and replay.
type chipContent struct {
	content string
	lines   int
}

// chipSpan marks the rune range [start, end) in the value holding a chip's
// label. Reconciled after any key event that can shift the value.
type chipSpan struct {
	id         int
	start, end int
}

// promptEntry is a frozen promptInput snapshot for history replay: displayed
// text plus the chip metadata needed to restore atomic chip behaviour on ↑/↓.
type promptEntry struct {
	display string
	store   map[int]chipContent
	spans   []chipSpan
}

// newPromptInput builds the configured textarea and chip bookkeeping. All
// styling lives here so model.go never touches textarea internals directly.
func newPromptInput() promptInput {
	ta := textarea.New()
	ta.Placeholder = "Ask codehamr. / or Tab for commands · Ctrl+C cancels"
	ta.Focus()
	ta.CharLimit = 0
	ta.MaxHeight = 0 // 0 = unbounded; recomputeLayout enforces the cap
	ta.ShowLineNumbers = false
	ta.SetHeight(1)
	ta.SetWidth(defaultWidth - 2)
	ta.Prompt = "▌ "

	bare := lipgloss.NewStyle()
	ta.FocusedStyle.Base = bare
	ta.FocusedStyle.CursorLine = bare
	ta.FocusedStyle.CursorLineNumber = bare
	ta.FocusedStyle.EndOfBuffer = bare
	ta.FocusedStyle.LineNumber = bare
	ta.FocusedStyle.Placeholder = styleDim
	ta.FocusedStyle.Prompt = stylePrompt
	ta.FocusedStyle.Text = bare
	ta.BlurredStyle = ta.FocusedStyle
	ta.Cursor.Style = styleHamr

	return promptInput{
		ta:    ta,
		store: map[int]chipContent{},
	}
}

// chipLabel is the visible form inserted into the textarea. Plain text, no
// ANSI, since textarea doesn't render inline styling inside its value.
func chipLabel(lines int) string {
	return fmt.Sprintf("[Pasted text +%d lines]", lines)
}

// Update is the promptInput's message entry point. Large pastes are swallowed
// into a chip and chip aware keys handled before delegation; everything else
// falls through to the textarea. reconcile() runs after any value shifting path.
func (p promptInput) Update(msg tea.Msg) (promptInput, tea.Cmd) {
	if kmsg, ok := msg.(tea.KeyMsg); ok {
		if looksLikePaste(kmsg) {
			if pasted := string(kmsg.Runes); shouldChip(pasted) {
				p.insertChip(pasted)
				return p, nil
			}
		}
		if handled, next := p.handlePageKey(kmsg); handled {
			return next, nil
		}
		if handled, next := p.handleChipKey(kmsg); handled {
			return next, nil
		}
		// A key the chip aware handlers didn't claim (a typed rune, or a cursor
		// move like Ctrl+B/Ctrl+F that the textarea owns) is about to reach the
		// textarea. Snap out of any chip first so the keystroke can't land inside
		// a label and split it: a split label desyncs reconcile, which, when two
		// chips share a label, cross maps the survivor to the wrong paste.
		p.snapCursorOutOfChip()
	}
	var cmd tea.Cmd
	p.ta, cmd = p.ta.Update(msg)
	p.reconcile()
	return p, cmd
}

// handlePageKey implements PgUp/PgDn. textarea disables its viewport keymap, so
// page keys aren't wired, so we translate them to N×CursorUp/CursorDown (one
// visible page, letting the viewport scroll to match), then snap out of any
// chip the move landed inside so the cursor never renders mid label.
func (p promptInput) handlePageKey(msg tea.KeyMsg) (bool, promptInput) {
	var step func()
	switch msg.Type {
	case tea.KeyPgUp:
		step = func() { p.ta.CursorUp() }
	case tea.KeyPgDown:
		step = func() { p.ta.CursorDown() }
	default:
		return false, p
	}
	n := max(p.ta.Height(), 1)
	for range n {
		step()
	}
	p.snapCursorOutOfChip()
	return true, p
}

// snapCursorOutOfChip snaps a cursor sitting strictly inside a chip to the
// nearer boundary. Returns the (possibly adjusted) offset so callers skip a
// second cursorRuneOffset read.
func (p *promptInput) snapCursorOutOfChip() int {
	cur := p.cursorRuneOffset()
	for _, chip := range p.spans {
		if cur > chip.start && cur < chip.end {
			target := chip.end
			if cur-chip.start < chip.end-cur {
				target = chip.start
			}
			p.setCursorRuneOffset(target)
			return target
		}
	}
	return cur
}

// chipAtBoundary returns the chip whose start (atStart=true) or end
// (atStart=false) coincides with cur, the check Backspace/Delete/Left/Right share.
func (p promptInput) chipAtBoundary(cur int, atStart bool) (chipSpan, bool) {
	for _, chip := range p.spans {
		boundary := chip.end
		if atStart {
			boundary = chip.start
		}
		if cur == boundary {
			return chip, true
		}
	}
	return chipSpan{}, false
}

// looksLikePaste recognises paste like key events. Primary signal: the
// bracketed paste Paste flag (terminal wraps content in \x1b[200~...\x1b[201~).
// Some terminals omit those markers, so a KeyRunes event containing a newline
// also counts: bubbletea breaks runs on control chars, so a single keystroke
// can never produce a newline inside one KeyMsg.
func looksLikePaste(msg tea.KeyMsg) bool {
	if msg.Paste {
		return true
	}
	if msg.Type != tea.KeyRunes {
		return false
	}
	for _, r := range msg.Runes {
		if r == '\n' || r == '\r' {
			return true
		}
	}
	return false
}

// shouldChip collapses a paste when either its line count or char count clears
// the threshold: lines catch multi line pastes, chars catch single line blobs.
func shouldChip(s string) bool {
	if countLines(s) >= pasteChipMinLines {
		return true
	}
	return utf8.RuneCountInString(s) >= pasteChipMinChars
}

// countLines returns a paste's visual line count. Terminals disagree on
// separators (\n unix, \r old mac, \r\n Windows); max of the \n and \r counts
// handles all three without double counting \r\n.
func countLines(s string) int {
	n := strings.Count(s, "\n")
	if r := strings.Count(s, "\r"); r > n {
		n = r
	}
	return n + 1
}

// handleChipKey gives chips atomic token semantics: Backspace/Delete at a
// boundary removes the whole chip, ←/→ jumps across it, and a cursor inside a
// chip is snapped to a boundary first. Returns (handled, updated).
func (p promptInput) handleChipKey(msg tea.KeyMsg) (bool, promptInput) {
	if len(p.spans) == 0 {
		return false, p
	}
	cur := p.snapCursorOutOfChip()
	switch msg.Type {
	case tea.KeyBackspace:
		if chip, ok := p.chipAtBoundary(cur, false); ok {
			p.deleteSpan(chip)
			return true, p
		}
	case tea.KeyDelete:
		if chip, ok := p.chipAtBoundary(cur, true); ok {
			p.deleteSpan(chip)
			return true, p
		}
	case tea.KeyLeft:
		if chip, ok := p.chipAtBoundary(cur, false); ok {
			p.setCursorRuneOffset(chip.start)
			return true, p
		}
	case tea.KeyRight:
		if chip, ok := p.chipAtBoundary(cur, true); ok {
			p.setCursorRuneOffset(chip.end)
			return true, p
		}
	}
	return false, p
}

// insertChip splices a chip label in at the cursor, recording the new span at
// the right ORDER position so the following reconcile() walks the labels
// left to right correctly. reconcile re derives every span's start/end from the
// updated value, so the offsets on the inserted literal are placeholders it
// overwrites: only the insertion index matters here.
func (p *promptInput) insertChip(content string) {
	lines := countLines(content)
	id := p.nextID
	p.nextID++
	p.store[id] = chipContent{content: content, lines: lines}

	label := chipLabel(lines)
	labelLen := utf8.RuneCountInString(label)
	// Snap out of any chip the cursor is parked inside before choosing the
	// insertion point, so a paste can't splice a new label into the interior of
	// an existing one: reconcile would then fail to find again the broken label
	// and silently drop a chip, sending the wrong (or no) paste to the LLM.
	insertAt := p.snapCursorOutOfChip()

	insertIdx := 0
	for i, s := range p.spans {
		if s.start < insertAt {
			insertIdx = i + 1
		} else {
			break
		}
	}
	p.spans = slices.Insert(p.spans, insertIdx,
		chipSpan{id: id, start: insertAt, end: insertAt + labelLen})

	p.ta.InsertString(label)
	p.reconcile()
}

// deleteSpan removes the chip's label from the value and drops it from spans
// and store. Cursor lands at the vacated start; reconcile re validates later
// spans, which shift left by the removed label length.
func (p *promptInput) deleteSpan(chip chipSpan) {
	value := p.ta.Value()
	runes := []rune(value)
	if chip.end > len(runes) {
		return
	}
	spliced := string(runes[:chip.start]) + string(runes[chip.end:])
	p.ta.SetValue(spliced)
	p.setCursorRuneOffset(chip.start)
	delete(p.store, chip.id)
	p.spans = slices.DeleteFunc(p.spans, func(s chipSpan) bool { return s.id == chip.id })
	p.reconcile()
}

// reconcile finds again each chip's label in the value (searching past the prior
// span's end) and updates offsets. A span whose label has vanished (e.g.
// partially deleted by a non chip aware edit) is dropped along with its store
// entry, so the chip becomes plain text from then on.
//
// When several chips share a label (same line count) and an edit damaged one
// of them, in order re binding would silently map a survivor to the wrong
// paste (the cross map named in Update's snap rationale; word deletes can
// reach into a label from outside, which the cursor snap can't prevent). The
// spans are indistinguishable then, so drop the whole label group instead.
func (p *promptInput) reconcile() {
	value := p.ta.Value()
	valueRunes := []rune(value)
	spansPerLabel := map[string]int{}
	for _, span := range p.spans {
		if content, ok := p.store[span.id]; ok {
			spansPerLabel[chipLabel(content.lines)]++
		}
	}
	kept := make([]chipSpan, 0, len(p.spans))
	searchFrom := 0
	for _, span := range p.spans {
		content, ok := p.store[span.id]
		if !ok {
			continue
		}
		label := chipLabel(content.lines)
		labelRunes := []rune(label)
		if spansPerLabel[label] > runeCount(valueRunes, labelRunes) {
			delete(p.store, span.id)
			continue
		}
		idx := runeIndex(valueRunes[searchFrom:], labelRunes)
		if idx < 0 {
			delete(p.store, span.id)
			continue
		}
		start := searchFrom + idx
		end := start + len(labelRunes)
		kept = append(kept, chipSpan{id: span.id, start: start, end: end})
		searchFrom = end
	}
	p.spans = kept
}

// runeCount is the counting counterpart of runeIndex: non overlapping
// occurrences of needle in haystack. Exact for chip labels, which cannot
// self overlap (they start with the unique "[" of the label format).
func runeCount(haystack, needle []rune) int {
	if len(needle) == 0 {
		return 0
	}
	count, from := 0, 0
	for {
		idx := runeIndex(haystack[from:], needle)
		if idx < 0 {
			return count
		}
		count++
		from += idx + len(needle)
	}
}

// runeIndex is a rune level strings.Index: first occurrence of needle in
// haystack, or -1. promptInput works in runes throughout because textarea's
// cursor is rune addressed (column = rune count, not byte count).
func runeIndex(haystack, needle []rune) int {
	if len(needle) == 0 {
		return 0
	}
	if len(needle) > len(haystack) {
		return -1
	}
	for i := 0; i <= len(haystack)-len(needle); i++ {
		match := true
		for j := range needle {
			if haystack[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

// cursorRuneOffset returns the cursor as an absolute rune index into Value().
// textarea only exposes (row, col) plus LineInfo, so we reconstruct it by
// walking prior lines' rune counts. SplitSeq avoids materialising a slice on
// every chip aware keypress.
func (p promptInput) cursorRuneOffset() int {
	row := p.ta.Line()
	info := p.ta.LineInfo()
	col := info.StartColumn + info.ColumnOffset

	offset, i := 0, 0
	for line := range strings.SplitSeq(p.ta.Value(), "\n") {
		if i == row {
			n := utf8.RuneCountInString(line)
			if col > n {
				col = n
			}
			return offset + col
		}
		offset += utf8.RuneCountInString(line) + 1 // +1 for the \n
		i++
	}
	return offset
}

// setCursorRuneOffset moves the cursor to an absolute rune position. textarea
// has no (row, col) setter, so we step CursorUp/Down to the target row then
// SetCursor the column. The step cap bounds the walk so a buggy textarea can't
// wedge the loop.
func (p *promptInput) setCursorRuneOffset(offset int) {
	value := p.ta.Value()
	targetRow, targetCol := runeOffsetToRowCol(value, offset)

	for range 2048 {
		curRow := p.ta.Line()
		if curRow == targetRow {
			break
		}
		info := p.ta.LineInfo()
		if curRow < targetRow {
			p.ta.CursorDown()
		} else {
			p.ta.CursorUp()
		}
		// Inside a soft wrapped logical line a cursor step moves only the
		// visual position (LineInfo), not Line(); that's progress toward the
		// target row, not a wedged textarea. Bail only when nothing moved, or
		// the walk stops one visual row short and SetCursor lands on the
		// wrong logical row.
		if next := p.ta.LineInfo(); p.ta.Line() == curRow &&
			next.RowOffset == info.RowOffset && next.ColumnOffset == info.ColumnOffset {
			break
		}
	}
	p.ta.SetCursor(targetCol)
}

// runeOffsetToRowCol converts an absolute rune offset into (row, col).
func runeOffsetToRowCol(value string, offset int) (int, int) {
	row, col := 0, 0
	i := 0
	for _, r := range value {
		if i == offset {
			return row, col
		}
		if r == '\n' {
			row++
			col = 0
		} else {
			col++
		}
		i++
	}
	return row, col
}

// View delegates to the textarea. Chip labels are already plain text in the
// value, so no post processing is needed.
func (p promptInput) View() string { return p.ta.View() }

// Value returns the prompt text with every chip label expanded to its original
// content, what goes to the LLM on submit.
func (p promptInput) Value() string {
	if len(p.spans) == 0 {
		return p.ta.Value()
	}
	value := p.ta.Value()
	runes := []rune(value)
	var b strings.Builder
	b.Grow(len(value))
	cursor := 0
	for _, span := range p.spans {
		content, ok := p.store[span.id]
		if !ok {
			continue
		}
		if span.start > cursor {
			b.WriteString(string(runes[cursor:span.start]))
		}
		b.WriteString(content.content)
		cursor = span.end
	}
	if cursor < len(runes) {
		b.WriteString(string(runes[cursor:]))
	}
	return b.String()
}

// DisplayValue returns the text as shown, chip labels stay collapsed. Used for
// echo to scroll on submit and the ↑/↓ history snapshot.
func (p promptInput) DisplayValue() string { return p.ta.Value() }

// Entry snapshots state for the history buffer, cloning store and spans so
// later edits to the live promptInput don't mutate the recorded entry.
func (p promptInput) Entry() promptEntry {
	return promptEntry{
		display: p.ta.Value(),
		store:   maps.Clone(p.store),
		spans:   slices.Clone(p.spans),
	}
}

// Restore replays a history entry: sets the display text, installs the
// snapshot's chip state, drops the cursor at the end.
func (p *promptInput) Restore(entry promptEntry) {
	p.ta.SetValue(entry.display)
	p.store = maps.Clone(entry.store)
	if p.store == nil {
		p.store = map[int]chipContent{}
	}
	p.spans = slices.Clone(entry.spans)
	p.ta.CursorEnd()
}

// Reset clears the typed text and all chip state. nextID is preserved so ids
// stay monotonic within a session.
func (p *promptInput) Reset() {
	p.ta.Reset()
	p.store = map[int]chipContent{}
	p.spans = nil
}

// SetValue installs a plain text value, dropping any chip state. Used by the
// slash popover's Tab completion path, where no chip can be injected.
func (p *promptInput) SetValue(s string) {
	p.ta.SetValue(s)
	p.store = map[int]chipContent{}
	p.spans = nil
}

func (p *promptInput) SetWidth(w int)  { p.ta.SetWidth(w) }
func (p *promptInput) SetHeight(h int) { p.ta.SetHeight(h) }
func (p promptInput) Height() int      { return p.ta.Height() }
func (p promptInput) Line() int        { return p.ta.Line() }
func (p promptInput) LineCount() int   { return p.ta.LineCount() }
func (p *promptInput) CursorEnd()      { p.ta.CursorEnd() }
