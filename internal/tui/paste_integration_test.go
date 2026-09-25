package tui

import (
	"net/http"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// TestModelBracketedPasteCreatesChip checks a bracketed paste KeyMsg through
// Model.Update reaches promptInput.Update and yields a chip, guarding against
// an earlier handleKey branch swallowing the paste.
func TestModelBracketedPasteCreatesChip(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	paste := strings.Repeat("x\n", 20) + "end"
	pasteMsg := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(paste), Paste: true}

	out, _ := m.Update(pasteMsg)
	om := out.(Model)
	if len(om.ta.spans) != 1 {
		t.Fatalf("paste via Model.Update should create a chip; spans=%+v", om.ta.spans)
	}
	if !strings.HasPrefix(om.ta.DisplayValue(), "[Pasted text +") {
		t.Fatalf("display value should start with chip label, got %q", om.ta.DisplayValue())
	}
}
