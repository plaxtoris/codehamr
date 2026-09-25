package tui

import (
	"net/http"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// typeChars feeds each rune of s as a separate KeyMsg, rendering View()
// after each. Non cosmetic: the viewport only populates its `lines` slice
// (and thus its YOffset clamp) inside View(), so batching keys without it
// keeps maxYOffset at 0 and never reproduces real scroll bugs. Mirrors
// real bubbletea, which renders after every Update.
func typeChars(model tea.Model, s string) tea.Model {
	for _, r := range s {
		out, _ := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		model = out
		_ = model.View()
	}
	return model
}

// TestTypingPreservesEarlyContent: a marker at the START of a line that
// wraps to many rows must stay visible in the textarea View.
//
// Guards a scroll bug: textarea's repositionView scrolls down when the
// cursor crosses below its Height; recomputeLayout grew Height *after*
// that scroll, leaving the marker's wrap row clipped off the top.
// preGrowTextarea inflates first so the cursor stays in view and
// repositionView never scrolls.
func TestTypingPreservesEarlyContent(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	final := typeChars(tea.Model(m), "ZZZSTART"+strings.Repeat("a", 300))
	mm := final.(Model)
	view := mm.ta.View()
	if !strings.Contains(view, "ZZZSTART") {
		t.Fatalf("first chars typed are not visible: viewport scrolled past them.\n"+
			"ta height=%d visualLines=%d width=%d\nView:\n%s",
			mm.ta.Height(), mm.visualPromptLines(), mm.width, view)
	}
}

// TestTypingOverflowKeepsCursorVisible: once content overflows
// maxTextareaHeight, pre grow must NOT pin the viewport to the top: that
// would scroll the cursor off the bottom and the user types blind. The
// natural bottom anchored scroll is correct, so the LAST char must show.
func TestTypingOverflowKeepsCursorVisible(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	// Force a small cap: height=10, minViewport=5, chrome=2 → maxTA=3.
	out, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 10})
	// Five wrapped rows; ~90 chars per wrap at width 100 keeps the count predictable.
	wide := strings.Repeat("a", 90)
	body := "ROW0START" + wide + wide + wide + wide + "ROW4END"
	final := typeChars(out, body)
	mm := final.(Model)
	if mm.maxTextareaHeight() >= 5 {
		t.Fatalf("precondition: maxTextareaHeight < 5 needed to force overflow, got %d",
			mm.maxTextareaHeight())
	}
	view := mm.ta.View()
	if !strings.Contains(view, "ROW4END") {
		t.Fatalf("end of overflowing content not visible: cursor scrolled off.\n"+
			"maxTA=%d ta height=%d visualLines=%d width=%d\nView:\n%s",
			mm.maxTextareaHeight(), mm.ta.Height(), mm.visualPromptLines(), mm.width, view)
	}
}
