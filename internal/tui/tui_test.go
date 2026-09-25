package tui

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/codehamr/codehamr/internal/config"
	chmctx "github.com/codehamr/codehamr/internal/ctx"
	"github.com/codehamr/codehamr/internal/llm"
	"github.com/codehamr/codehamr/internal/tools"
)

// newTestModel wires a model against a mock OpenAI SSE server so we can
// exercise submit → stream → done without the real stack. The server is
// torn down via t.Cleanup.
func newTestModel(t *testing.T, handler http.HandlerFunc) Model {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	cfg, _, err := config.Bootstrap(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg.ActiveProfile().URL = srv.URL
	// Persist so the reload on slash path reads the mock URL back, not the
	// seeded localhost default.
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	client := llm.New(srv.URL, cfg.ActiveProfile().LLM, "")
	m := New(cfg, client, t.TempDir(), "test")
	// give it a size so view() doesn't panic
	sized, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	return sized.(Model)
}

// TestSystemPromptIncludesWorkingDirAndInvestigateRule: the system prompt must
// (a) tell the model to investigate files itself rather than ask the user to
// paste them, and (b) end with the working directory so "hier" / "here"
// resolves to a concrete path.
func TestSystemPromptIncludesWorkingDirAndInvestigateRule(t *testing.T) {
	cfg, _, _ := config.Bootstrap(t.TempDir())
	projectDir := "/workspaces/codehamr"
	m := New(cfg, llm.New("http://x", cfg.ActiveProfile().LLM, ""), projectDir, "test")
	if !strings.Contains(m.system, "open it yourself with `read_file`") {
		t.Fatalf("system prompt missing the investigate-files-yourself rule:\n%s", m.system)
	}
	if !strings.Contains(m.system, "Working directory: "+projectDir) {
		t.Fatalf("system prompt missing working-directory anchor for %q:\n%s",
			projectDir, m.system)
	}
}

// TestSystemPromptFitsFixedSystemReservation pins ctx.FixedSystem against the
// embedded prompt. Grow the prompt past the reservation and Pack over allocates
// to history on small ctx profiles, so the next request exceeds the server's
// limit and 400s (or is silently truncated). On failure, raise ctx.FixedSystem;
// don't loosen the assertion.
func TestSystemPromptFitsFixedSystemReservation(t *testing.T) {
	cfg, _, _ := config.Bootstrap(t.TempDir())
	m := New(cfg, llm.New("http://x", cfg.ActiveProfile().LLM, ""), "/workspaces/codehamr", "test")
	cost := chmctx.Message{Role: chmctx.RoleSystem, Content: m.system}.Tokens()
	if cost > chmctx.FixedSystem {
		t.Fatalf("system prompt costs %d tokens, FixedSystem reserves only %d: "+
			"raise ctx.FixedSystem so Budget() doesn't over-allocate to history",
			cost, chmctx.FixedSystem)
	}
}

// TestCtrlDEmptyQuits: Ctrl+D on empty textarea returns a Quit command.
func TestCtrlDEmptyQuits(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	if m.ta.Value() != "" {
		t.Fatal("precondition: textarea empty")
	}
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlD})
	if cmd == nil {
		t.Fatal("Ctrl+D on empty input should return tea.Quit")
	}
}

// TestCtrlDNonEmptyNoOp: Ctrl+D with text in the textarea is a no operation: no
// quit and no character deletion.
func TestCtrlDNonEmptyNoOp(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	m.ta.SetValue("half-written prompt")
	out, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlD})
	if cmd != nil {
		t.Fatal("Ctrl+D with nonempty input must not quit")
	}
	if got := out.(Model).ta.Value(); got != "half-written prompt" {
		t.Fatalf("textarea was modified: %q", got)
	}
}

// TestCtrlDMidTurnDoesNotQuit: the textarea is empty during a running turn
// (submit resets it), so without the phase gate a reflexive Ctrl+D would quit
// instantly, skipping turnCtx cancel and orphaning a running tool's process
// group. Ctrl+C is the mid turn escape; Ctrl+D must be inert until idle.
func TestCtrlDMidTurnDoesNotQuit(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	m.phase = phaseThinking
	if m.ta.Value() != "" {
		t.Fatal("precondition: textarea empty")
	}
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlD})
	if cmd != nil {
		t.Fatal("Ctrl+D mid-turn must not quit")
	}
}

// TestPlaceholderMentionsTab: placeholder names both "/" and "Tab" as entry
// points into the popover, so new users discover either way.
func TestPlaceholderMentionsTab(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	view := m.View()
	if !strings.Contains(view, "/ or Tab for commands") {
		t.Fatalf("placeholder should mention both / and Tab: %s", view)
	}
	if strings.Contains(view, "/models") || strings.Contains(view, "/clear") {
		t.Fatalf("placeholder still enumerates commands: %s", view)
	}
}

// TestCtrlCIdleArmsThenQuits: first Ctrl+C in idle arms the status bar and
// returns a Tick cmd; second Ctrl+C before the window expires returns Quit.
func TestCtrlCIdleArmsThenQuits(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	first, cmd1 := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	fm := first.(Model)
	if fm.quitArmedAt.IsZero() {
		t.Fatal("first Ctrl+C should arm quitArmedAt")
	}
	if !strings.Contains(fm.renderStatusBar(), "press Ctrl+C again") {
		t.Fatalf("status bar should show arming hint, got: %s", fm.renderStatusBar())
	}
	if cmd1 == nil {
		t.Fatal("first press should return tea.Tick cmd")
	}
	_, cmd2 := fm.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd2 == nil {
		t.Fatal("second Ctrl+C should return tea.Quit cmd")
	}
}

// TestCtrlCPopoverClosesInsteadOfQuitting: with the popover open and no
// in flight op, Ctrl+C dismisses the popover and does not arm quit.
func TestCtrlCPopoverClosesInsteadOfQuitting(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	mm := typeInto(m, "/")
	if !mm.popoverOpen() {
		t.Fatal("precondition: popover should be open")
	}
	out, _ := mm.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	om := out.(Model)
	if om.popoverOpen() {
		t.Fatal("Ctrl+C should close popover")
	}
	if !om.quitArmedAt.IsZero() {
		t.Fatal("popover-close should not arm quit")
	}
}

// TestCtrlCCancelsInflightOp: with a turn in flight, Ctrl+C cancels, clears
// pending tool calls, and leaves a "✗ cancelled" line in scrollback.
func TestCtrlCCancelsInflightOp(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	ctx, cancel := context.WithCancel(context.Background())
	m.turnCtx = ctx
	m.cancel = cancel
	m.phase = phaseThinking
	m.pending = []chmctx.ToolCall{{Name: "bash"}}

	out, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	om := out.(Model)

	if om.phase.active() {
		t.Fatalf("phase should be idle after cancel, got %v", om.phase)
	}
	if om.cancel != nil {
		t.Fatal("cancel should be cleared after use")
	}
	if len(om.pending) != 0 {
		t.Fatalf("pending should be cleared: %+v", om.pending)
	}
	if !strings.Contains(om.scroll.String(), "cancelled") {
		t.Fatalf("expected ✗ cancelled in scrollback: %s", om.scroll.String())
	}
	select {
	case <-ctx.Done():
		// ctx propagated cancel, good
	default:
		t.Fatal("underlying context was not cancelled")
	}
}

// TestNonCtrlCKeypressResetsArming: once arming is live, pressing anything
// other than Ctrl+C clears the arm so the next idle Ctrl+C rearms cleanly
// (no accidental quits).
func TestNonCtrlCKeypressResetsArming(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	first, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	fm := first.(Model)
	if fm.quitArmedAt.IsZero() {
		t.Fatal("precondition: quit should be armed")
	}
	typed, _ := fm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
	if !typed.(Model).quitArmedAt.IsZero() {
		t.Fatal("any other keystroke must clear arming")
	}
}

// typeInto feeds text one rune at a time, as a keyboard would, exercising the
// refreshSuggest hook on the KeyRunes fall through.
func typeInto(m Model, text string) Model {
	var mm tea.Model = m
	for _, r := range text {
		out, _ := mm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		mm = out
	}
	return mm.(Model)
}

func suggestNames(m Model) []string {
	out := make([]string, 0, len(m.suggest))
	for _, c := range m.suggest {
		out = append(out, c.value)
	}
	return out
}

// TestPopoverTriggersOnSlash: typing / into an empty textarea opens the
// popover with every command; typing more characters filters by prefix.
func TestPopoverTriggersOnSlash(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	mm := typeInto(m, "/")
	if !mm.popoverOpen() {
		t.Fatal("popover should open after typing /")
	}
	if len(mm.suggest) != len(commands) {
		t.Fatalf("all commands should match empty prefix, got %d of %d",
			len(mm.suggest), len(commands))
	}
	mm2 := typeInto(mm, "mod") // "/mod"
	names := suggestNames(mm2)
	if len(names) != 1 || names[0] != "/models" {
		t.Fatalf("expected exactly /models to match /mod, got %v", names)
	}
}

// TestPopoverClosesWhenPrefixMatchesNothing: typing a prefix that no command
// satisfies closes the popover automatically (no empty frame).
func TestPopoverClosesWhenPrefixMatchesNothing(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	mm := typeInto(m, "/zzz")
	if mm.popoverOpen() {
		t.Fatalf("popover should close when no prefix matches: %+v", mm.suggest)
	}
}

// TestPopoverTabCyclesSelection: Tab moves the selection to the next row
// without touching the textarea, zsh style cycling.
func TestPopoverTabCyclesSelection(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	mm := typeInto(m, "/")
	if !mm.popoverOpen() || len(mm.suggest) < 2 {
		t.Fatalf("precondition: popover open with ≥2 options, got %d", len(mm.suggest))
	}
	start := mm.suggestIdx
	mm2, _ := mm.Update(tea.KeyMsg{Type: tea.KeyTab})
	if mm2.(Model).suggestIdx != (start+1)%len(mm.suggest) {
		t.Fatalf("Tab should advance selection, got idx=%d (was %d)",
			mm2.(Model).suggestIdx, start)
	}
	if got := mm2.(Model).ta.Value(); got != "/" {
		t.Fatalf("textarea must not change on Tab cycle: %q", got)
	}
	if !mm2.(Model).popoverOpen() {
		t.Fatal("popover should stay open after Tab")
	}
}

// TestPopoverTabOnEmptyOpensCommandList: Tab on an empty textarea is
// equivalent to typing "/": it opens the popover with the full command
// list. Subsequent Tabs cycle.
func TestPopoverTabOnEmptyOpensCommandList(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	if m.ta.Value() != "" || m.popoverOpen() {
		t.Fatal("precondition: empty textarea, popover closed")
	}
	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyTab})
	om := mm.(Model)
	if !om.popoverOpen() {
		t.Fatal("Tab on empty should open the command popover")
	}
	if om.ta.Value() != "/" {
		t.Fatalf("textarea should be seeded with '/' got %q", om.ta.Value())
	}
	if len(om.suggest) != len(commands) {
		t.Fatalf("popover should show all commands, got %d of %d",
			len(om.suggest), len(commands))
	}
	// second Tab cycles (selection moves from 0 to 1)
	mm2, _ := om.Update(tea.KeyMsg{Type: tea.KeyTab})
	if mm2.(Model).suggestIdx != 1 {
		t.Fatalf("second Tab should cycle to idx 1, got %d", mm2.(Model).suggestIdx)
	}
}

// TestPopoverTabCompletesUniquePrefix: with one match, Tab completes the name
// and, because /models takes args, appends a space that flips the popover into
// arg level mode. Flow: "/mod<Tab>" → "/models " + arg popover.
func TestPopoverTabCompletesUniquePrefix(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	mm := typeInto(m, "/mod") // only /models matches
	if len(mm.suggest) != 1 {
		t.Fatalf("precondition: one match for /mod, got %d", len(mm.suggest))
	}
	mm2, _ := mm.Update(tea.KeyMsg{Type: tea.KeyTab})
	om := mm2.(Model)
	if got := om.ta.Value(); got != "/models " {
		t.Fatalf("Tab should complete to '/models ' (with trailing space), got %q", got)
	}
	if !om.suggestArgLevel || om.activeCmd != "/models" {
		t.Fatalf("popover should have transitioned to arg-level for /models: "+
			"level=%v cmd=%q", om.suggestArgLevel, om.activeCmd)
	}
}

// TestPopoverTabOnClearCommandHasNoArgSpace: /clear takes no args, so Tab
// completes to "/clear" WITHOUT a trailing space.
func TestPopoverTabOnClearCommandHasNoArgSpace(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	mm := typeInto(m, "/cl") // only /clear matches
	if len(mm.suggest) != 1 {
		t.Fatalf("precondition: one match for /cl, got %d", len(mm.suggest))
	}
	mm2, _ := mm.Update(tea.KeyMsg{Type: tea.KeyTab})
	if got := mm2.(Model).ta.Value(); got != "/clear" {
		t.Fatalf("Tab should complete to '/clear' (no trailing space for no-arg cmds), got %q", got)
	}
}

// TestPopoverShiftTabCyclesUp: Shift+Tab walks the selection backwards,
// wrapping at the top.
func TestPopoverShiftTabCyclesUp(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	mm := typeInto(m, "/")
	n := len(mm.suggest)
	if !mm.popoverOpen() || n < 2 {
		t.Fatalf("precondition: popover open with ≥2 options, got %d", n)
	}
	mm2, _ := mm.Update(tea.KeyMsg{Type: tea.KeyShiftTab})
	if mm2.(Model).suggestIdx != (mm.suggestIdx-1+n)%n {
		t.Fatalf("Shift+Tab should move selection up, got idx=%d", mm2.(Model).suggestIdx)
	}
}

// TestEscFromCommandLevelClosesAndClears: Esc at command level closes the
// popover AND clears the textarea, so the user returns to a blank prompt.
// Typing "/" from the blank slate reopens the popover from scratch.
func TestEscFromCommandLevelClosesAndClears(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	mm := typeInto(m, "/")
	mm2, _ := mm.Update(tea.KeyMsg{Type: tea.KeyEsc})
	om := mm2.(Model)
	if om.popoverOpen() {
		t.Fatal("Esc should close popover")
	}
	if om.ta.Value() != "" {
		t.Fatalf("Esc at command-level should clear textarea, got %q", om.ta.Value())
	}
	// Typing "/" from a blank slate reopens the popover.
	mm3 := typeInto(om, "/")
	if !mm3.popoverOpen() {
		t.Fatal("typing '/' after Esc should re-open popover")
	}
}

// TestPopoverArrowKeysMoveSelection: while popover is open, ↑/↓ move the
// selection (NOT arrow history), and the textarea is not clobbered.
func TestPopoverArrowKeysMoveSelection(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	m.promptHistory = []promptEntry{{display: "old prompt"}} // should NOT be recalled while popover open
	mm := typeInto(m, "/")
	start := mm.suggestIdx
	n := len(mm.suggest)
	mm2, _ := mm.Update(tea.KeyMsg{Type: tea.KeyDown})
	if mm2.(Model).suggestIdx != (start+1)%n {
		t.Fatalf("↓ should move selection, got idx=%d (was %d)",
			mm2.(Model).suggestIdx, start)
	}
	if got := mm2.(Model).ta.Value(); got != "/" {
		t.Fatalf("textarea must not be overwritten by history while popover open: %q", got)
	}
}

// TestPopoverEnterAdvancesIntoArgsForArgsCommand: Enter at command level on a
// command that takes args does NOT submit: it opens the arg level popover,
// same as Tab.
func TestPopoverEnterAdvancesIntoArgsForArgsCommand(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	mm := typeInto(m, "/mod") // popover has only /models (has args)
	if !mm.popoverOpen() {
		t.Fatal("popover should be open")
	}
	out, _ := mm.Update(tea.KeyMsg{Type: tea.KeyEnter})
	om := out.(Model)
	if om.ta.Value() != "/models " {
		t.Fatalf("Enter should advance textarea to '/models ', got %q", om.ta.Value())
	}
	if !om.suggestArgLevel || om.activeCmd != "/models" {
		t.Fatalf("Enter should open arg-popover for /models, got level=%v cmd=%q",
			om.suggestArgLevel, om.activeCmd)
	}
	// scroll should NOT contain a submitted /models, nothing has been sent yet
	if strings.Contains(om.scroll.String(), "▌ /models") {
		t.Fatalf("Enter must not have submitted: scroll: %s", om.scroll.String())
	}
}

// TestPopoverEnterSubmitsNoArgCommand: Enter at command level on a command
// without args still submits immediately (/clear).
func TestPopoverEnterSubmitsNoArgCommand(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	mm := typeInto(m, "/cl") // /clear matches, no args
	if !mm.popoverOpen() {
		t.Fatal("popover should be open")
	}
	out, _ := mm.Update(tea.KeyMsg{Type: tea.KeyEnter})
	om := out.(Model)
	if om.popoverOpen() {
		t.Fatal("popover should close after submit")
	}
	if om.ta.Value() != "" {
		t.Fatalf("textarea should reset: %q", om.ta.Value())
	}
	// /clear fires a "✓ conversation reset" line into scrollback
	if !strings.Contains(om.scroll.String(), "conversation reset") {
		t.Fatalf("/clear should have executed: %s", om.scroll.String())
	}
}

// TestArgPopoverOpensForModels: "/models " shows the profile names (no synthetic
// "next", Tab cycles instead) with the active profile preselected.
func TestArgPopoverOpensForModels(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	// Add a second profile to exercise argument completion.
	m.cfg.Models["remote"] = &config.Profile{
		LLM: "cloud-model", URL: "http://r", Key: "sk-r", ContextSize: 200000,
	}
	// Persist so the popover's cmd→arg reload reads back the test setup.
	if err := m.cfg.Save(); err != nil {
		t.Fatal(err)
	}
	mm := typeInto(m, "/models ")
	if !mm.suggestArgLevel || mm.activeCmd != "/models" {
		t.Fatalf("expected arg-level for /models: level=%v cmd=%q",
			mm.suggestArgLevel, mm.activeCmd)
	}
	names := suggestNames(mm)
	// sorted: [local, remote], no "next"
	if len(names) != 2 || names[0] != "local" || names[1] != "remote" {
		t.Fatalf("expected [local remote], got %v", names)
	}
	if mm.suggest[mm.suggestIdx].value != "local" {
		t.Fatalf("default selection should be active profile 'local', got %q",
			mm.suggest[mm.suggestIdx].value)
	}
}

// TestHistoryUpDownReplayLastSubmission: ↑ on first line replaces textarea
// with the most recent submitted line; ↓ steps back toward the draft.
func TestHistoryUpDownReplayLastSubmission(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	m.promptHistory = []promptEntry{{display: "first question"}, {display: "second question"}}
	m.histIdx = -1

	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyUp})
	if got := mm.(Model).ta.Value(); got != "second question" {
		t.Fatalf("↑ should replay newest, got %q", got)
	}
	mm2, _ := mm.(Model).Update(tea.KeyMsg{Type: tea.KeyUp})
	if got := mm2.(Model).ta.Value(); got != "first question" {
		t.Fatalf("↑↑ should replay oldest, got %q", got)
	}
	mm3, _ := mm2.(Model).Update(tea.KeyMsg{Type: tea.KeyUp})
	if got := mm3.(Model).ta.Value(); got != "first question" {
		t.Fatalf("↑ past oldest should stay, got %q", got)
	}
	mm4, _ := mm3.(Model).Update(tea.KeyMsg{Type: tea.KeyDown})
	if got := mm4.(Model).ta.Value(); got != "second question" {
		t.Fatalf("↓ should move toward draft, got %q", got)
	}
	mm5, _ := mm4.(Model).Update(tea.KeyMsg{Type: tea.KeyDown})
	if got := mm5.(Model).ta.Value(); got != "" {
		t.Fatalf("↓ past newest should restore empty draft, got %q", got)
	}
	mm6, _ := mm5.(Model).Update(tea.KeyMsg{Type: tea.KeyDown})
	if got := mm6.(Model).ta.Value(); got != "" {
		t.Fatalf("↓ at draft should be a no-op, got %q", got)
	}
}

// TestHistoryDownRestoresUnsentDraft: an unsent draft survives a ↑ into history
// and is restored when ↓ walks back to the live line.
func TestHistoryDownRestoresUnsentDraft(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	m.promptHistory = []promptEntry{{display: "old prompt"}}
	m.histIdx = -1
	m.ta.SetValue("my unsent draft")

	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyUp})
	if got := mm.(Model).ta.Value(); got != "old prompt" {
		t.Fatalf("↑ should replay history, got %q", got)
	}
	mm2, _ := mm.(Model).Update(tea.KeyMsg{Type: tea.KeyDown})
	if got := mm2.(Model).ta.Value(); got != "my unsent draft" {
		t.Fatalf("↓ should restore the unsent draft, got %q", got)
	}
}

// TestHistoryPushesOnSubmit: successful submit appends to promptHistory and
// resets the walker index to -1.
func TestHistoryPushesOnSubmit(t *testing.T) {
	m := newTestModel(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "data: "+`{"type":"response.output_text.delta","delta":"ok"}`+"\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{}}\n\n")
	})
	m.histIdx = 2 // simulate "was navigating history"
	mm, _ := m.submit("hello", "hello", promptEntry{display: "hello"})
	final := mm.(Model)
	if len(final.promptHistory) != 1 || final.promptHistory[0].display != "hello" {
		t.Fatalf("promptHistory wrong: %+v", final.promptHistory)
	}
	if final.histIdx != -1 {
		t.Fatalf("histIdx should reset to -1 after submit, got %d", final.histIdx)
	}
}

// TestBackendLabelShowsActiveProfile: the label echoes the currently active
// profile name. Default Bootstrap ships one profile called "local". No
// brackets: the label is just the name (bold).
func TestBackendLabelShowsActiveProfile(t *testing.T) {
	cfg, _, _ := config.Bootstrap(t.TempDir())
	if cfg.Active != "local" {
		t.Fatalf("default Active expected local, got %q", cfg.Active)
	}
	label := stripANSI(backendLabel(cfg, true))
	if label != "local" {
		t.Fatalf("expected plain 'local' label, got %q", label)
	}
	// second profile → label follows Active when switched
	cfg.Models["remote"] = &config.Profile{LLM: "m", URL: "http://r"}
	cfg.Active = "remote"
	if got := stripANSI(backendLabel(cfg, true)); got != "remote" {
		t.Fatalf("expected 'remote' after switch, got %q", got)
	}
}

// TestPrintHelpListsAllCommands: tui.PrintHelp formats every command in the
// central slice. Guards against a command being added to runSlash dispatch
// but forgotten in --help.
func TestPrintHelpListsAllCommands(t *testing.T) {
	var buf bytes.Buffer
	PrintHelp(&buf)
	for _, want := range []string{"/clear", "/models"} {
		if !strings.Contains(buf.String(), want) {
			t.Fatalf("PrintHelp missing %q:\n%s", want, buf.String())
		}
	}
}

// TestSlashModelSwitchesActive: /models <name> sets Active and rebuilds the
// llm client's base URL / token / model to the new profile's values.
func TestSlashModelSwitchesActive(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	m.cfg.Models["remote"] = &config.Profile{
		LLM: "cloud-model", URL: "http://remote:9000", Key: "sk-r", ContextSize: 200000,
	}
	if err := m.cfg.Save(); err != nil {
		t.Fatal(err)
	}
	m2, _ := m.runSlash("/models remote")
	final := m2.(Model)
	if final.cfg.Active != "remote" {
		t.Fatalf("Active should be 'remote', got %q", final.cfg.Active)
	}
	if final.cli.BaseURL != "http://remote:9000" {
		t.Fatalf("client.BaseURL not rebuilt: %q", final.cli.BaseURL)
	}
	if final.cli.Model != "cloud-model" {
		t.Fatalf("client.Model not rebuilt: %q", final.cli.Model)
	}
	if final.cli.Token != "sk-r" {
		t.Fatalf("client.Token not rebuilt: %q", final.cli.Token)
	}
}

// TestDebugLogFilePermsAreOwnerOnly: the log captures every prompt and tool call
// payload: bash args can carry heredoc secrets, so a world readable log leaks
// them. 0o600 only.
func TestDebugLogFilePermsAreOwnerOnly(t *testing.T) {
	dir := t.TempDir()
	OpenDebugLog(dir)
	t.Cleanup(CloseDebugLog)
	st, err := os.Stat(filepath.Join(dir, "log.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Mode().Perm(); got != 0o600 {
		t.Fatalf("log.txt perms = %v, want 0o600", got)
	}
}

// TestVerboseLogCapturesTurnRecords drives a realistic two round turn (reasoning
// → bash tool call → final answer) with logging on, and asserts the verbose
// records that make a session reconstructable for later debugging actually land
// in log.txt: the session header, the per round request/packing summary, the
// streamed reasoning, the tool result, and the round/turn metrics. Also pins the
// dated timestamp: a bare clock can't be correlated across a day boundary. This
// is the regression guard against a refactor silently gutting the debug log.
func TestVerboseLogCapturesTurnRecords(t *testing.T) {
	var round int
	handler := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		round++
		if round == 1 {
			fmt.Fprintf(w, "data: %s\n\n", `{"type":"response.reasoning_text.delta","delta":"let me check the file"}`)
			fmt.Fprintf(w, "data: %s\n\n", `{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","call_id":"c1","name":"bash","arguments":"{\"cmd\":\"echo HAMMER\"}"}}`)
			fmt.Fprintf(w, "data: %s\n\n", `{"type":"response.completed","response":{"usage":{"output_tokens":5}}}`)
			return
		}
		fmt.Fprintf(w, "data: %s\n\n", `{"type":"response.output_text.delta","delta":"all done"}`)
		fmt.Fprintf(w, "data: %s\n\n", `{"type":"response.completed","response":{"usage":{"output_tokens":2}}}`)
	}

	dir := t.TempDir()
	OpenDebugLog(dir)
	t.Cleanup(CloseDebugLog)
	// Logging is on before New runs (inside newTestModel), so the session header
	// is captured too: it writes to the global dbgFile, not cfg.Dir.
	m := newTestModel(t, handler)
	mm, cmd := m.submit("inspect the repo", "inspect the repo", promptEntry{display: "inspect the repo"})
	drain(mm, cmd)
	CloseDebugLog()

	raw, err := os.ReadFile(filepath.Join(dir, "log.txt"))
	if err != nil {
		t.Fatal(err)
	}
	logStr := string(raw)

	for _, want := range []string{
		"] session", "context_size=", // backend + budget header
		"] user", "inspect the repo", // the prompt
		"] request", "packed=", // per round packing summary
		"] reasoning", "let me check the file", // streamed chain of thought
		"] tool_result",            // the bash result
		"] assistant",              // final assistant message
		"] round_done", "elapsed=", // per round metrics
		"] turn_end", // turn totals
	} {
		if !strings.Contains(logStr, want) {
			t.Fatalf("verbose log missing %q\n--- log.txt ---\n%s", want, logStr)
		}
	}

	// Dated timestamp: "[2006-01-02 15:04:05.000]", not the old bare clock.
	first := logStr[:strings.IndexByte(logStr, '\n')]
	if len(first) < 21 || first[0] != '[' || first[5] != '-' || first[8] != '-' || first[11] != ' ' {
		t.Fatalf("log timestamp missing date component: %q", first)
	}
}

// TestCtxPressureTripwire: the round_done companion warning fires only when the
// server counted prompt reaches 95% of the active window. The bytes/4 packer
// undercounts code heavy history (~1.6x measured on a real run), so the server
// count is the only signal that the real prompt is about to spill past the
// window into silent server side truncation.
func TestCtxPressureTripwire(t *testing.T) {
	dir := t.TempDir()
	OpenDebugLog(dir)
	t.Cleanup(CloseDebugLog)
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	m.cfg.ActiveProfile().ContextSize = 10000

	m.applyDone(llm.Event{PromptTokens: 9499}) // below threshold: silent
	m.applyDone(llm.Event{})                   // server reported nothing: silent
	m.applyDone(llm.Event{PromptTokens: 9500}) // 95%: fire
	CloseDebugLog()

	raw, err := os.ReadFile(filepath.Join(dir, "log.txt"))
	if err != nil {
		t.Fatal(err)
	}
	logStr := string(raw)
	if got := strings.Count(logStr, "] ctx_pressure"); got != 1 {
		t.Fatalf("tripwire must fire exactly once (at >=95%% of the window), got %d\n%s", got, logStr)
	}
	if !strings.Contains(logStr, "prompt_tokens=9500 at >=95% of ctx=10000") {
		t.Fatalf("tripwire record must name the offending count and window:\n%s", logStr)
	}
}

// TestSlashModelSwitchDropsStickyFallbackState: llm.Client's noReasoning flag
// ("this server 400'd on the reasoning effort, stop sending it") is correct for
// one Client but wrong across a profile switch to a different endpoint. rebuildClient swaps in a fresh Client; this asserts the pointer
// changed so the sticky bit can't survive.
func TestSlashModelSwitchDropsStickyFallbackState(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	m.cfg.Models["remote"] = &config.Profile{
		LLM: "cloud-model", URL: "http://remote:9000", Key: "sk-r", ContextSize: 200000,
	}
	if err := m.cfg.Save(); err != nil {
		t.Fatal(err)
	}
	before := m.cli
	out, _ := m.runSlash("/models remote")
	final := out.(Model)
	if final.cli == before {
		t.Fatal("rebuildClient must replace the *llm.Client pointer to drop sticky reasoning fallback state")
	}
	if final.cli.BaseURL != "http://remote:9000" || final.cli.Model != "cloud-model" || final.cli.Token != "sk-r" {
		t.Fatalf("fresh client missing one of the new profile's fields: %+v", final.cli)
	}
}

// TestSlashModelRejectsUnknown: unknown name is a quiet warn, not a switch.
func TestSlashModelRejectsUnknown(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	before := m.cfg.Active
	m2, _ := m.runSlash("/models ghost")
	if m2.(Model).cfg.Active != before {
		t.Fatal("unknown name must not change Active")
	}
}

func TestSlashClearResetsHistory(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	m.history = append(m.history,
		chmctx.Message{Role: chmctx.RoleUser, Content: "hi"},
		chmctx.Message{Role: chmctx.RoleAssistant, Content: "hey"},
	)
	m2, _ := m.runSlash("/clear")
	if len(m2.(Model).history) != 0 {
		t.Fatal("/clear must drop history")
	}
}

// TestSlashClearWipesTerminalScrollback: tea.ClearScreen (\x1b[2J) clears only
// the visible region; the saved lines buffer needs the DECSED 3 sequence from
// eraseScrollback. /clear must emit both, or prior lines stay scrollable above
// the reset banner.
func TestSlashClearWipesTerminalScrollback(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	_, cmd := m.runSlash("/clear")
	if !cmdYieldsClearScreen(cmd) {
		t.Error("/clear must wipe the visible viewport via tea.ClearScreen")
	}
	if !cmdYieldsScrollbackErase(cmd) {
		t.Error("/clear must also emit eraseScrollback (\\x1b[3J): otherwise old replies stay scrollable above the reset banner")
	}
}

// TestArgPopoverReloadsCfgOnEntry: the arg popover builds its list from
// m.cfg.Models. Without the cmd→arg reload in refreshSuggest, the first
// "/models " sees stale in memory cfg and misses a hand added profile;
// reload at popover open makes it visible on the first entry, not the second.
func TestArgPopoverReloadsCfgOnEntry(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	// Hand write a "remote" profile straight to config.yaml, bypassing
	// cfg.Save(): simulates an external editor.
	yaml := []byte(`active: local
models:
  local:
    llm: local-model
    url: ` + m.cfg.Models["local"].URL + `
    key: ""
    context_size: 256000
  remote:
    llm: cloud-model
    url: http://remote:9000
    key: sk-r
    context_size: 128000
`)
	if err := os.WriteFile(filepath.Join(m.cfg.Dir, "config.yaml"), yaml, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.cfg.Models["remote"]; ok {
		t.Fatal("precondition: in-memory cfg must not know about 'remote' yet")
	}
	mm := typeInto(m, "/models ")
	names := suggestNames(mm)
	if !slices.Contains(names, "remote") {
		t.Fatalf("external 'remote' profile missing from arg popover on first /models entry, got %v", names)
	}
}

// TestArgPopoverSkipsConfigReloadMidTurn: typing is allowed mid turn, but the
// cmd→arg popover transition must not reload config then: a reload can
// rebuildClient and swap the live llm.Client under the
// in flight turn. The stale list is fine; submit is phase gated and runSlash
// reloads once the turn is over.
func TestArgPopoverSkipsConfigReloadMidTurn(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	yaml := []byte(`active: local
models:
  local:
    llm: local-model
    url: ` + m.cfg.Models["local"].URL + `
    key: ""
    context_size: 256000
  remote:
    llm: cloud-model
    url: http://remote:9000
    key: sk-r
    context_size: 128000
`)
	if err := os.WriteFile(filepath.Join(m.cfg.Dir, "config.yaml"), yaml, 0o600); err != nil {
		t.Fatal(err)
	}
	m.phase = phaseThinking
	mm := typeInto(m, "/models ")
	names := suggestNames(mm)
	if slices.Contains(names, "remote") {
		t.Fatalf("mid-turn popover transition must not reload config, got %v", names)
	}
	if !slices.Contains(names, "local") {
		t.Fatalf("popover should still list the in-memory profiles, got %v", names)
	}
}

// TestRunSlashPicksUpExternalConfigEdits: runSlash rereads
// .codehamr/config.yaml before dispatching, so a profile a user hand added
// to the file mid session shows up on the next /models without a restart.
func TestRunSlashPicksUpExternalConfigEdits(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	// Hand write a config adding "remote" alongside the seeded local,
	// bypassing cfg.Save(): what a user would do in an external editor.
	yaml := []byte(`active: local
models:
  local:
    llm: local-model
    url: ` + m.cfg.Models["local"].URL + `
    key: ""
    context_size: 131072
  remote:
    llm: cloud-model
    url: http://remote:9000
    key: sk-r
    context_size: 200000
`)
	if err := os.WriteFile(filepath.Join(m.cfg.Dir, "config.yaml"), yaml, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.cfg.Models["remote"]; ok {
		t.Fatal("precondition: in-memory cfg must not know about 'remote' yet")
	}
	out, _ := m.runSlash("/models")
	final := out.(Model)
	if _, ok := final.cfg.Models["remote"]; !ok {
		t.Fatalf("external edit not picked up: Models keys: %v",
			final.cfg.ModelNames())
	}
}

// TestRunSlashWarnsOnBrokenConfig: a typo in config.yaml must not lock the
// user out of slash commands. The reload prints a one line warning and
// keeps the previous in memory cfg, so /models (and further editing) keep
// working with last known good state.
func TestRunSlashWarnsOnBrokenConfig(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	prevActive := m.cfg.Active
	prevModels := len(m.cfg.Models)
	if err := os.WriteFile(filepath.Join(m.cfg.Dir, "config.yaml"),
		[]byte("active: [unterminated\nmodels: {{"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, _ := m.runSlash("/models")
	final := out.(Model)
	scroll := stripANSI(final.scroll.String())
	if !strings.Contains(scroll, "config.yaml") {
		t.Fatalf("scrollback should warn about broken config.yaml:\n%s", scroll)
	}
	if final.cfg.Active != prevActive {
		t.Fatalf("broken-config reload must preserve previous Active, got %q want %q",
			final.cfg.Active, prevActive)
	}
	if len(final.cfg.Models) != prevModels {
		t.Fatalf("broken-config reload must preserve previous Models, got %d want %d",
			len(final.cfg.Models), prevModels)
	}
}

// TestSlashClearSurvivesBrokenConfig: with config.yaml unparseable, /clear must
// still wipe history. The reload warning gets wiped with the rest of scrollback
// (fine, it reappears on the next non-/clear slash); the reset still works.
func TestSlashClearSurvivesBrokenConfig(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	m.history = append(m.history,
		chmctx.Message{Role: chmctx.RoleUser, Content: "hi"},
		chmctx.Message{Role: chmctx.RoleAssistant, Content: "hey"},
	)
	if err := os.WriteFile(filepath.Join(m.cfg.Dir, "config.yaml"),
		[]byte("active: [unterminated\nmodels: {{"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, _ := m.runSlash("/clear")
	if len(out.(Model).history) != 0 {
		t.Fatal("/clear must still drop history with broken config")
	}
}

func TestSubmitStreamsUpToDone(t *testing.T) {
	m := newTestModel(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\n", `{"type":"response.output_text.delta","delta":"pong"}`)
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{}}\n\n")
	})
	mm, cmd := m.submit("ping", "ping", promptEntry{display: "ping"})
	out, _ := drain(mm, cmd)
	if got := out.(Model).scroll.String(); !strings.Contains(got, "pong") {
		t.Fatalf("assistant output missing: %q", got)
	}
}

func TestToolCallRoundTripExecutesBash(t *testing.T) {
	// Turn 1: bash tool call. Turn 2: plain content, no tool call. A turn ends
	// when the assistant stops emitting tool calls (handleStreamClosed →
	// finalizeTurn → endTurn).
	turn := 0
	m := newTestModel(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		turn++
		switch turn {
		case 1:
			fmt.Fprintf(w, "data: %s\n\n", `{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","call_id":"c1","name":"bash","arguments":"{\"cmd\":\"echo HAMMER\"}"}}`)
			fmt.Fprintf(w, "data: %s\n\n", `{"type":"response.completed","response":{"usage":{"output_tokens":5}}}`)
		default:
			fmt.Fprintf(w, "data: %s\n\n", `{"type":"response.output_text.delta","delta":"echoed HAMMER for you"}`)
			fmt.Fprintf(w, "data: %s\n\n", `{"type":"response.completed","response":{"usage":{"output_tokens":1}}}`)
		}
	})
	mm, cmd := m.submit("run echo", "run echo", promptEntry{display: "run echo"})
	out, _ := drain(mm, cmd)
	final := out.(Model)

	if turn != 2 {
		t.Fatalf("expected 2 LLM turns, got %d", turn)
	}
	// history: user, assistant(bash call), tool(bash result), assistant(content)
	if len(final.history) != 4 {
		t.Fatalf("history wrong: %d messages", len(final.history))
	}
	if final.history[2].Role != "tool" || !strings.Contains(final.history[2].Content, "HAMMER") {
		t.Fatalf("tool result missing: %+v", final.history[2])
	}
	if !strings.Contains(stripANSI(final.scroll.String()), "echoed HAMMER for you") {
		t.Fatalf("final assistant content missing from scroll: %q", final.scroll.String())
	}
	// No tool calls → idle, control back to the user.
	if final.phase.active() {
		t.Fatalf("turn ending with no tool calls must return to idle, phase=%v", final.phase)
	}
	// The frozen run summary must sum tokens across both LLM rounds, not
	// overwrite. Round 1 reports usage.completion_tokens=5, round 2 reports 1.
	// finalizeTurn freezes turnTokens into lastTokens (the avg rate divisor). Sum = 6.
	if final.lastTokens != 6 {
		t.Fatalf("per-turn tokens should sum across rounds (5+1), got %d", final.lastTokens)
	}
	// A clean finish (no tool calls) freezes the ✓ outcome for the idle footer.
	if final.lastOutcome != outcomeDone {
		t.Fatalf("clean finish should freeze outcomeDone, got %v", final.lastOutcome)
	}
}

// TestToolArgsStreamBumpsEstimateAndPhase: a tool call argument fragment (a file
// streaming into write_file) ticks the live token estimate AND flips the phase
// to "generating", so the counter doesn't freeze through a long file write: the
// bug where only chat content and reasoning were counted, not tool arguments.
func TestToolArgsStreamBumpsEstimateAndPhase(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	m.phase = phaseThinking // a tool only round starts here, before any content
	out, _ := m.handleStream(llm.Event{Kind: llm.EventToolArgs, Content: strings.Repeat("x", 40)})
	m = out.(Model)
	if m.phase != phaseStreaming {
		t.Fatalf("EventToolArgs should flip thinking→generating, phase=%v", m.phase)
	}
	if m.streamingEstimate != 10 { // 40 chars / 4
		t.Fatalf("EventToolArgs should bump the estimate by len/4, got %d", m.streamingEstimate)
	}
}

// TestBuildToolsExposesExactlyFourTools pins the tool roster: bash, read_file,
// write_file, edit_file, in that order, with no loop/control tool. Order is
// part of the contract: the model sees the tools in the payload in this sequence.
func TestBuildToolsExposesExactlyFourTools(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	got := m.buildTools()
	want := []string{
		tools.BashName,
		tools.ReadFileName,
		tools.WriteFileName,
		tools.EditFileName,
	}
	if len(got) != len(want) {
		t.Fatalf("buildTools returned %d tools, want %d: %+v", len(got), len(want), got)
	}
	for i, name := range want {
		if got[i].Name != name {
			t.Fatalf("tool[%d] = %q, want %q (order matters)", i, got[i].Name, name)
		}
	}
}

// TestTurnEndsWhenAssistantEmitsNoToolCalls pins the turn end contract: a final
// assistant message with no tool calls returns to phaseIdle and hands control
// back: no nudge, no forced tool, no extra message appended to history.
func TestTurnEndsWhenAssistantEmitsNoToolCalls(t *testing.T) {
	m := newTestModel(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\n", `{"type":"response.output_text.delta","delta":"all done, nothing to run"}`)
		fmt.Fprintf(w, "data: %s\n\n", `{"type":"response.completed","response":{"usage":{"output_tokens":4}}}`)
	})
	mm, cmd := m.submit("just answer", "just answer", promptEntry{display: "just answer"})
	out, _ := drain(mm, cmd)
	final := out.(Model)

	if final.phase.active() {
		t.Fatalf("no-tool-call turn must return to idle, phase=%v", final.phase)
	}
	// History should be exactly: user prompt + assistant reply. No nudge
	// message of any role appended.
	if len(final.history) != 2 {
		t.Fatalf("expected 2 history messages (user + assistant), got %d: %+v",
			len(final.history), final.history)
	}
	if final.history[0].Role != chmctx.RoleUser {
		t.Fatalf("history[0] should be the user prompt, got %+v", final.history[0])
	}
	if final.history[1].Role != chmctx.RoleAssistant {
		t.Fatalf("history[1] should be the assistant reply, got %+v", final.history[1])
	}
	if !strings.Contains(stripANSI(final.scroll.String()), "all done, nothing to run") {
		t.Fatalf("assistant content missing from scroll:\n%s", final.scroll.String())
	}
}

// TestHandleStreamClosedEndsTurnWithNoPending: handleStreamClosed with an empty
// pending queue finalizes the turn, returns to idle, and returns no follow up
// Cmd: nothing to enforce, no reentry into chat.
func TestHandleStreamClosedEndsTurnWithNoPending(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	m.phase = phaseStreaming
	m.installTurnContext() // live turnCtx/cancel
	m.stream = make(chan llm.Event)
	before := len(m.history)

	out, cmd := m.handleStreamClosed()
	om := out.(Model)

	if cmd != nil {
		t.Fatal("a no-pending stream close must end the turn, not start another (nil Cmd)")
	}
	if om.phase.active() {
		t.Fatalf("turn must return to idle, phase=%v", om.phase)
	}
	if len(om.history) != before {
		t.Fatalf("turn end must not append any message, history grew by %d", len(om.history)-before)
	}
	if om.cancel != nil || om.turnCtx != nil {
		t.Fatal("endTurn must clear cancel/turnCtx")
	}
}

// runTurn wires a model against handler, submits `text`, drains the command
// chain, and returns the Model. token, when nonempty, is installed on both the
// active profile and the live llm.Client so cloud auth headers travel as in
// production.
func runTurn(t *testing.T, handler http.HandlerFunc, token, text string) Model {
	t.Helper()
	m := newTestModel(t, handler)
	if token != "" {
		m.cfg.ActiveProfile().Key = token
		m.cli.Token = token
	}
	mm, cmd := m.submit(text, text, promptEntry{display: text})
	out, _ := drain(mm, cmd)
	return out.(Model)
}

// TestStalePingForOldBackendDoesNotOverwriteConnectedFlag: a ping launched
// against the old profile's URL can land after the user /models'd to a new
// reachable profile. Without the URL tag, the stale "unreachable" ping flickers
// connected false and shows a bogus "!" warning for the live backend.
func TestStalePingForOldBackendDoesNotOverwriteConnectedFlag(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	m.connected = true
	live := m.cli.BaseURL

	// pingMsg from a stale URL (different from the live client's BaseURL):
	out, _ := m.Update(pingMsg{ok: false, baseURL: "http://stale-prior-backend"})
	if !out.(Model).connected {
		t.Fatal("stale ping for old backend overwrote live connected=true")
	}

	// Sanity: a ping with the matching URL DOES update.
	out, _ = m.Update(pingMsg{ok: false, baseURL: live})
	if out.(Model).connected {
		t.Fatal("ping for the live backend must update connected")
	}
}

// TestStaleProbeForOldProfileDoesNotOverwriteConnectedFlag mirrors the pingMsg
// guard for probeMsg: a probe for a no longer active profile must not mutate the
// live reachability indicator, or the stale outcome flickers on the new badge.
func TestStaleProbeForOldProfileDoesNotOverwriteConnectedFlag(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	m.cfg.Active = "local"
	m.connected = true

	// Stale success probe for a profile other than the active one must not
	// confirm "connected" on behalf of the live backend.
	out, _ := m.handleProbe(probeMsg{profile: "remote"})
	if !out.(Model).connected {
		t.Fatal("stale success probe overwrote live connected=true")
	}

	// Stale failure probe must not flip the live backend to disconnected.
	m.connected = true
	out, _ = m.handleProbe(probeMsg{profile: "remote", err: llm.ErrUnauthorized, silent: true})
	if !out.(Model).connected {
		t.Fatal("stale failure probe overwrote live connected=true")
	}

	// Sanity: a probe for the live profile with the live client DOES update.
	out, _ = m.handleProbe(probeMsg{profile: "local", cli: m.cli, err: llm.ErrUnauthorized, silent: true})
	if out.(Model).connected {
		t.Fatal("probe for the live profile must update connected")
	}
}

// TestStaleProbeForSameProfileDoesNotOverwriteFreshOutcome: two probes for the
// SAME profile can be in flight (startup probe + a reactivation); the profile
// name alone can't tell them apart, so staleness is keyed on the client
// pointer (rebuildClient swaps it per reactivation). A hung old probe's
// failure landing after the fresh probe's success must neither flip connected
// nor print a contradictory "⚠ probe" banner after the "✓ active" line.
func TestStaleProbeForSameProfileDoesNotOverwriteFreshOutcome(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	m.cfg.Active = "local"
	staleCli := m.cli
	m.rebuildClient() // reactivation swapped the client; staleCli's probe is now superseded
	m.connected = true

	out, _ := m.handleProbe(probeMsg{profile: "local", cli: staleCli, err: llm.ErrUnauthorized})
	final := out.(Model)
	if !final.connected {
		t.Fatal("stale same-profile probe failure must not flip connected")
	}
	if got := stripANSI(final.scroll.String()); strings.Contains(got, "⚠ probe") {
		t.Fatalf("stale same-profile probe must not print a failure banner:\n%s", got)
	}
}

// TestViewHandlesZeroWidth reproduces the "shrunk UI" startup flash: before
// WindowSizeMsg arrives, View must not panic or emit garbled layout.
func TestViewHandlesZeroWidth(t *testing.T) {
	cfg, _, _ := config.Bootstrap(t.TempDir())
	m := New(cfg, llm.New("http://x", cfg.ActiveProfile().LLM, ""), t.TempDir(), "test")
	m.width = 0 // simulate no WindowSizeMsg yet
	if got := m.View(); got != "" {
		t.Fatalf("zero-width view should be empty, got %q", got)
	}
	// After a real WindowSizeMsg the full frame should render.
	sized, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	if sized.(Model).View() == "" {
		t.Fatal("sized view should not be empty")
	}
}

// TestStatusBarShowsSpinnerWhenWaiting verifies the micro animation text
// appears in the bottom bar while a request is in flight, and disappears
// when not.
func TestStatusBarShowsSpinnerWhenWaiting(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	if strings.Contains(m.renderStatusBar(), "thinking") {
		t.Fatal("idle status bar must not show thinking indicator")
	}
	m.phase = phaseThinking
	if !strings.Contains(m.renderStatusBar(), "thinking") {
		t.Fatalf("thinking status bar must show thinking indicator: %q", m.renderStatusBar())
	}
	m.phase = phaseStreaming
	if !strings.Contains(m.renderStatusBar(), "generating") {
		t.Fatalf("streaming status bar must show generating indicator: %q", m.renderStatusBar())
	}
	m.phase = phaseRunning
	if !strings.Contains(m.renderStatusBar(), "running") {
		t.Fatalf("running status bar must show running indicator: %q", m.renderStatusBar())
	}
}

// TestBackendLabelReflectsConnectedState asserts the backend label renders
// differently when connected vs not: the user's at a glance "are we
// talking to a server?" signal. Disconnected appends a `!` marker so the
// distinction survives on colour stripped terminals.
func TestBackendLabelReflectsConnectedState(t *testing.T) {
	cfg, _, _ := config.Bootstrap(t.TempDir())
	ok := backendLabel(cfg, true)
	bad := backendLabel(cfg, false)
	if ok == bad {
		t.Fatalf("connected and disconnected labels must render differently, got %q for both", ok)
	}
	if stripANSI(ok) != "local" {
		t.Fatalf("connected label should be plain profile name: %q", stripANSI(ok))
	}
	if got := stripANSI(bad); !strings.Contains(got, "local") || !strings.Contains(got, "!") {
		t.Fatalf("disconnected label must include profile name and '!' marker: %q", got)
	}
}

// TestErrorMessageUnreachable verifies the unreachable hint names the active
// profile's URL and steers the user toward /models to switch profiles.
func TestErrorMessageUnreachable(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	m.cfg.ActiveProfile().URL = "http://localhost:11434"

	msg := m.errorMessage(llm.Event{Err: llm.ErrUnreachable{Err: fmt.Errorf("dial: refused")}})
	if !strings.Contains(msg, "unreachable") {
		t.Fatalf("error must say 'unreachable': %q", msg)
	}
	if !strings.Contains(msg, m.cfg.ActiveProfile().URL) {
		t.Fatalf("error must include the configured URL: %q", msg)
	}
	if !strings.Contains(msg, "/models") {
		t.Fatalf("error must hint at /models: %q", msg)
	}
	if strings.Contains(msg, "ollama") {
		t.Fatalf("error should be backend-neutral (no 'ollama'): %q", msg)
	}
}

// TestErrorMessageUnauthorized: 401 names the rejected key and the exact
// config path so the user can fix it without guessing.
func TestErrorMessageUnauthorized(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	msg := m.errorMessage(llm.Event{Err: llm.ErrUnauthorized})
	if !strings.Contains(msg, "key rejected") {
		t.Fatalf("401 error should say 'key rejected': %q", msg)
	}
	if !strings.Contains(msg, "models."+m.cfg.Active+".key") {
		t.Fatalf("401 error should name the active profile's key path: %q", msg)
	}
}

// TestCtrlLClearsPromptNotScrollback: Ctrl+L matches Claude Code: it
// clears the typed input and forces a terminal redraw, but conversation
// scrollback stays. /clear is the only way to wipe history.
func TestCtrlLClearsPromptNotScrollback(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	m.ta.SetValue("half-written thought")
	m.scroll.WriteString("prior assistant message\n")

	out, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlL})
	om := out.(Model)

	if om.ta.Value() != "" {
		t.Fatalf("Ctrl+L must clear typed prompt, got %q", om.ta.Value())
	}
	if !strings.Contains(om.scroll.String(), "prior assistant message") {
		t.Fatalf("Ctrl+L must NOT wipe scrollback, got %q", om.scroll.String())
	}
	if cmd == nil {
		t.Fatal("Ctrl+L should return tea.ClearScreen cmd to force terminal redraw")
	}
}

// TestCtrlLClosesPopover: Ctrl+L clears the popover along with the prompt.
// Left open on stale suggestions, the next Enter would take the has selection
// path and insert a ghost command into the freshly emptied prompt.
func TestCtrlLClosesPopover(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	o1, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	m1 := o1.(Model)
	if !m1.popoverOpen() {
		t.Fatal("precondition: typing '/' opens the command popover")
	}

	o2, _ := m1.Update(tea.KeyMsg{Type: tea.KeyCtrlL})
	m2 := o2.(Model)
	if m2.popoverOpen() {
		t.Fatal("Ctrl+L must close the popover along with the prompt")
	}
	o3, _ := m2.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if got := o3.(Model).ta.Value(); got != "" {
		t.Fatalf("Enter after Ctrl+L must be a no-op, got ghost completion %q", got)
	}
}

// TestAltEnterInsertsNewline: Alt+Enter composes a multi line prompt. The
// alt flagged KeyEnter ("alt+enter") matches no textarea binding, so the flag
// must be stripped before forwarding or the key is a silent no operation and
// multi line prompts can only be pasted.
func TestAltEnterInsertsNewline(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	m.ta.SetValue("first line")
	out, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter, Alt: true})
	om := out.(Model)
	if got := om.ta.Value(); got != "first line\n" {
		t.Fatalf("Alt+Enter must insert a newline, got %q", got)
	}
	if len(om.history) != 0 {
		t.Fatal("Alt+Enter must not submit the prompt")
	}
}

// TestHumanIntFormat: thin comma formatting must handle the edge cases the
// activation line cares about: single digits, exact 4 digit, exact powers,
// and very large windows that would otherwise read as a wall of digits.
func TestHumanIntFormat(t *testing.T) {
	cases := map[int]string{
		0:           "0",
		7:           "7",
		999:         "999",
		1000:        "1,000",
		12345:       "12,345",
		1_000_000:   "1,000,000",
		262144:      "262,144",
		8_000_000:   "8,000,000",
		262_144_000: "262,144,000",
	}
	for n, want := range cases {
		if got := humanInt(n); got != want {
			t.Errorf("humanInt(%d) = %q, want %q", n, got, want)
		}
	}
}

// TestHumanTokensFormat: the session counter renders compactly: plain int under
// 1000, then `k`/`M` with a constant single decimal. The decimal is always kept
// (`2.0k`, not `2k`) so the live counter doesn't jump width as it ticks past a
// round thousand.
func TestHumanTokensFormat(t *testing.T) {
	cases := []struct {
		n    int
		want string
	}{
		{0, "0 tok"},
		{1, "1 tok"},
		{900, "900 tok"},
		{999, "999 tok"},
		{1000, "1.0k tok"},
		{1200, "1.2k tok"},
		{1900, "1.9k tok"}, // the pair the user flagged: must stay constant width
		{2000, "2.0k tok"}, // not "2k tok"
		{9999, "10.0k tok"},
		{10_000, "10.0k tok"},
		{42_000, "42.0k tok"},
		{999_999, "1000.0k tok"},
		{1_000_000, "1.0M tok"},
		{1_500_000, "1.5M tok"},
		{12_345_678, "12.3M tok"},
	}
	for _, c := range cases {
		if got := humanTokens(c.n); got != c.want {
			t.Errorf("humanTokens(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}

// TestLiveElapsed: the running wall clock readout, whole seconds under a
// minute (no spinning sub second decimal at the spinner's refresh rate), then
// `6m 51s` / `1h 14m`, with the lower unit always two digits and never
// dropped (`8m 00s`, not `8m`), so the readout never shrinks mid turn.
func TestLiveElapsed(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{0, "0s"},
		{800 * time.Millisecond, "0s"},
		{12_300 * time.Millisecond, "12s"},
		{59_900 * time.Millisecond, "59s"},
		{60 * time.Second, "1m 00s"},
		{90 * time.Second, "1m 30s"},
		{411_100 * time.Millisecond, "6m 51s"},
		{479 * time.Second, "7m 59s"},
		{480 * time.Second, "8m 00s"},
		{3599 * time.Second, "59m 59s"},
		{3600 * time.Second, "1h 00m"},
		{3660 * time.Second, "1h 01m"},
		{7200 * time.Second, "2h 00m"},
		{7500 * time.Second, "2h 05m"},
	}
	for _, c := range cases {
		if got := liveElapsed(c.d); got != c.want {
			t.Errorf("liveElapsed(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}

// TestHumanRateFormat: throughput as `N tok/s`. Degenerate inputs (zero tokens
// or zero elapsed) collapse to "" so the banner omits the segment. Sub 10 tok/s
// keeps one decimal: reasoning models hover near 1 tok/s where it's the signal.
func TestHumanRateFormat(t *testing.T) {
	cases := []struct {
		tokens int
		d      time.Duration
		want   string
	}{
		{0, time.Second, ""},
		{10, 0, ""},
		{-3, time.Second, ""},
		{86, 3400 * time.Millisecond, "25 tok/s"},
		{50, time.Second, "50 tok/s"},
		{53, 10 * time.Second, "5.3 tok/s"},
		{1, 2 * time.Second, "0.5 tok/s"},
		{120, 120 * time.Second, "1.0 tok/s"},
		{100, time.Second, "100 tok/s"},
	}
	for _, c := range cases {
		if got := humanRate(c.tokens, c.d); got != c.want {
			t.Errorf("humanRate(%d, %v) = %q, want %q", c.tokens, c.d, got, c.want)
		}
	}
}

// TestSessionTokensAccumulateAcrossTurns: the session counter is separate
// from the per turn counter. finalizeTurn resets turnTokens; sessionTokens
// keeps growing for the rest of the session.
func TestSessionTokensAccumulateAcrossTurns(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	// Simulate three Done events. phase must be active or handleStream will
	// (correctly) drop events as stale post cancel buffer: EventDone does
	// not move phase, so we seed streaming once and let each Done carry
	// through.
	m.phase = phaseStreaming
	for _, n := range []int{7, 13, 22} {
		out, _ := m.handleStream(llm.Event{Kind: llm.EventDone, Tokens: n})
		m = out.(Model)
	}
	if m.sessionTokens != 7+13+22 {
		t.Fatalf("session counter should sum all Done events: got %d, want %d",
			m.sessionTokens, 7+13+22)
	}
	if m.turnTokens != 7+13+22 {
		// Without a streamClosedMsg between events, turnTokens keeps summing
		// too, verifying the two counters are in lockstep before finalize.
		t.Fatalf("precondition: turn counter should equal session before finalize, got %d", m.turnTokens)
	}
}

// TestSessionTokensSurviveFinalizeTurn: finalizeTurn clears turnTokens but
// must NOT touch sessionTokens. turnStart must be set or finalizeTurn no operations.
func TestSessionTokensSurviveFinalizeTurn(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	m.turnTokens = 50
	m.turnStart = time.Now()
	m.sessionTokens = 123
	m.finalizeTurn(outcomeDone)
	if m.turnTokens != 0 {
		t.Fatalf("turnTokens should be reset by finalizeTurn, got %d", m.turnTokens)
	}
	if m.sessionTokens != 123 {
		t.Fatalf("sessionTokens must not be touched by finalizeTurn, got %d", m.sessionTokens)
	}
	if m.lastOutcome != outcomeDone {
		t.Fatalf("finalizeTurn should freeze the outcome, got %v", m.lastOutcome)
	}
}

// TestFinalizeFoldsInFlightEstimate: a turn aborted mid stream (Ctrl+C, error)
// has no EventDone to fold the current round's tokens into turnTokens; they sit
// in streamingEstimate. finalizeTurn must commit that estimate so the frozen avg
// counts what was generated up to the interrupt and the session total doesn't
// drop backward. (On a clean finish the estimate is already 0, so this is a no operation.)
func TestFinalizeFoldsInFlightEstimate(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	m.turnStart = time.Now()
	m.turnTokens = 40        // one completed round
	m.streamingEstimate = 60 // an in flight round cancelled before EventDone
	m.sessionTokens = 200
	m.finalizeTurn(outcomeStopped)
	if m.lastTokens != 100 {
		t.Fatalf("frozen tokens should fold the in-flight estimate (40+60), got %d", m.lastTokens)
	}
	if m.sessionTokens != 260 {
		t.Fatalf("session total should absorb the in-flight estimate (200+60), got %d", m.sessionTokens)
	}
	if m.streamingEstimate != 0 {
		t.Fatalf("finalizeTurn should consume the estimate, got %d", m.streamingEstimate)
	}
	if m.lastOutcome != outcomeStopped {
		t.Fatalf("abort should freeze outcomeStopped, got %v", m.lastOutcome)
	}
}

// TestToolTargetKey pins the identity the repeated failure nudge keys on: file
// tools key on tool+path, bash on tool + the trimmed first command line, else
// the bare tool name. Deliberately NOT the full args: a cosmetic retry change
// (regenerated body, reworded tail) must not reset the streak.
func TestToolTargetKey(t *testing.T) {
	cases := []struct {
		name string
		call chmctx.ToolCall
		want string
	}{
		{"write_file keys on path", chmctx.ToolCall{
			Name: tools.WriteFileName, Arguments: map[string]any{"path": "/a/b.go", "content": "anything"},
		}, tools.WriteFileName + "|/a/b.go"},
		{"edit_file keys on path", chmctx.ToolCall{
			Name: tools.EditFileName, Arguments: map[string]any{"path": "/a/b.go", "old_string": "x", "new_string": "y"},
		}, tools.EditFileName + "|/a/b.go"},
		{"read_file keys on path", chmctx.ToolCall{
			Name: tools.ReadFileName, Arguments: map[string]any{"path": "/a/b.go"},
		}, tools.ReadFileName + "|/a/b.go"},
		{"bash keys on first line of cmd", chmctx.ToolCall{
			Name: tools.BashName, Arguments: map[string]any{"cmd": "go test ./...\necho done"},
		}, tools.BashName + "|go test ./..."},
		{"bash trims surrounding whitespace", chmctx.ToolCall{
			Name: tools.BashName, Arguments: map[string]any{"cmd": "   ls -la   \nmore"},
		}, tools.BashName + "|ls -la"},
		{"unknown tool keys on name", chmctx.ToolCall{
			Name: "context7", Arguments: map[string]any{"query": "x"},
		}, "context7"},
	}
	for _, c := range cases {
		if got := toolTargetKey(c.call); got != c.want {
			t.Errorf("%s: toolTargetKey = %q, want %q", c.name, got, c.want)
		}
	}
}

// TestToolResultFailed pins the failure classifier the nudge keys on: a
// "(cancelled)" result (user Ctrl+C) is never a failure; write/edit fail iff the
// trimmed result opens with "(" (their error convention); read_file returns raw
// content on success, which can start with "(", so it fails only on its two
// real error outputs; bash fails iff it carries "\n(exit: " or "(timeout after "
// or is exactly "(empty command)"; a clean result is not a failure.
func TestToolResultFailed(t *testing.T) {
	cases := []struct {
		name   string
		tool   string
		result string
		want   bool
	}{
		{"cancelled is never a failure", tools.BashName, "partial\n(cancelled)", false},
		{"cancelled file op is not a failure", tools.WriteFileName, "(cancelled)", false},
		{"bash nonzero exit fails", tools.BashName, "boom\n(exit: exit status 1)", true},
		{"bash timeout fails", tools.BashName, "slow\n(timeout after 2s)", true},
		{"bash empty-command fails", tools.BashName, "(empty command)", true},
		{"bash clean success", tools.BashName, "all green\n", false},
		{"bash leading-paren output is not a failure", tools.BashName, "(3 rows affected)\n", false},
		{"write_file error fails", tools.WriteFileName, "(write error: permission denied)", true},
		{"write_file success", tools.WriteFileName, "wrote 5 bytes to /tmp/x", false},
		{"edit_file not-found fails", tools.EditFileName, "(not found: old_string)", true},
		{"edit_file success", tools.EditFileName, "edited /tmp/x", false},
		{"read_file error fails", tools.ReadFileName, "(read error: no such file)", true},
		{"read_file empty-path fails", tools.ReadFileName, "(empty path)", true},
		{"read_file success", tools.ReadFileName, "package main\n", false},
		{"read_file Lisp content is not a failure", tools.ReadFileName, "(ns foo)\n(defn bar [] 1)\n", false},
		{"read_file leading-paren prose is not a failure", tools.ReadFileName, "(this file starts with a paren)", false},
		// Router level failures bypass the per tool shapes and must still feed
		// the streak: truncated args reemitted forever was exactly the loop
		// the failure nudge was built for.
		{"invalid-JSON args fail for bash", tools.BashName, "(tool arguments were not valid JSON: unexpected end of JSON input, most likely the arguments were truncated)", true},
		{"invalid-JSON args fail for read_file", tools.ReadFileName, "(tool arguments were not valid JSON: x)", true},
		{"unknown tool fails", "made_up_tool", "(unknown tool: made_up_tool)", true},
	}
	for _, c := range cases {
		if got := toolResultFailed(c.tool, c.result); got != c.want {
			t.Errorf("%s: toolResultFailed(%q, %q) = %v, want %v",
				c.name, c.tool, c.result, got, c.want)
		}
	}
}

// TestRepeatedFailureNudgeFiresOnceAfterFiveSameTargetFailures drives the
// repeated failure backstop end to end. Five consecutive failures of the SAME
// target append exactly one RoleSystem nudge and reset the streak; the nudge
// text reports the count.
func TestRepeatedFailureNudgeFiresOnceAfterFiveSameTargetFailures(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	m.installTurnContext()
	m.phase = phaseThinking
	// Same failing bash target five times: stamp lastToolKey as dispatchNextTool
	// would, then feed the failing result through Update's toolResultMsg case.
	m.lastToolKey = toolTargetKey(chmctx.ToolCall{
		Name: tools.BashName, Arguments: map[string]any{"cmd": "make build"},
	})
	failResult := chmctx.Message{
		Role: chmctx.RoleTool, ToolName: tools.BashName,
		Content: "ld: symbol not found\n(exit: exit status 1)",
	}

	var mm tea.Model = m
	for i := 0; i < maxToolFailStreak; i++ {
		out, _ := mm.(Model).recordAndNudge(failResult)
		mm = out
	}
	final := mm.(Model)

	// Exactly one system nudge, naming the count, after the 5th failure.
	nudges := 0
	var last chmctx.Message
	for _, msg := range final.history {
		if msg.Role == chmctx.RoleSystem {
			nudges++
			last = msg
		}
	}
	if nudges != 1 {
		t.Fatalf("expected exactly one RoleSystem nudge after %d same target failures, got %d:\n%+v",
			maxToolFailStreak, nudges, final.history)
	}
	if !strings.Contains(last.Content, fmt.Sprintf("last %d tool calls", maxToolFailStreak)) {
		t.Fatalf("nudge should name the streak count: %q", last.Content)
	}
	// Streak resets after firing so it can't double fire on the next failure.
	if final.failStreak != 0 || final.failKey != "" {
		t.Fatalf("nudge must reset failKey/failStreak, got key=%q streak=%d", final.failKey, final.failStreak)
	}
}

// recordAndNudge replays Update's toolResultMsg "queue drained" branch: record
// the outcome, then consider the failure nudge. Lets the streak tests exercise
// the real recordToolOutcome + maybeFailureNudge without a live SSE turn.
func (m Model) recordAndNudge(result chmctx.Message) (tea.Model, tea.Cmd) {
	m.history = append(m.history, result)
	m.recordToolOutcome(result.ToolName, result.Content)
	m.maybeFailureNudge()
	return m, nil
}

// TestRepeatedFailureNudgeSuccessResetsStreak: a single success in the middle
// of a run of failures resets the streak, so the nudge never fires.
func TestRepeatedFailureNudgeSuccessResetsStreak(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	m.lastToolKey = tools.BashName + "|make build"
	fail := chmctx.Message{Role: chmctx.RoleTool, ToolName: tools.BashName, Content: "x\n(exit: exit status 1)"}
	ok := chmctx.Message{Role: chmctx.RoleTool, ToolName: tools.BashName, Content: "all green"}

	var mm tea.Model = m
	// 4 failures, then one success, then 4 more failures, never 5 in a row.
	for i := 0; i < 4; i++ {
		out, _ := mm.(Model).recordAndNudge(fail)
		mm = out
	}
	out, _ := mm.(Model).recordAndNudge(ok)
	mm = out
	for i := 0; i < 4; i++ {
		out, _ := mm.(Model).recordAndNudge(fail)
		mm = out
	}
	final := mm.(Model)
	for _, msg := range final.history {
		if msg.Role == chmctx.RoleSystem {
			t.Fatalf("a success in the middle must prevent the nudge, but one fired:\n%+v", final.history)
		}
	}
}

// TestRepeatedFailureNudgeDifferentTargetResetsStreak: switching targets
// between failures resets the streak: exploration across distinct operations
// must not trip the nudge.
func TestRepeatedFailureNudgeDifferentTargetResetsStreak(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	fail := chmctx.Message{Role: chmctx.RoleTool, ToolName: tools.BashName, Content: "x\n(exit: exit status 1)"}

	var mm tea.Model = m
	// Each iteration fails, but against a different bash target, so the streak
	// keeps resetting to 1 and never reaches maxToolFailStreak.
	for i := 0; i < maxToolFailStreak+2; i++ {
		cur := mm.(Model)
		cur.lastToolKey = fmt.Sprintf("%s|echo %d", tools.BashName, i)
		out, _ := cur.recordAndNudge(fail)
		mm = out
	}
	final := mm.(Model)
	for _, msg := range final.history {
		if msg.Role == chmctx.RoleSystem {
			t.Fatalf("distinct targets must not trip the nudge, but one fired:\n%+v", final.history)
		}
	}
	if final.failStreak != 1 {
		t.Fatalf("after distinct-target failures the streak should sit at 1, got %d", final.failStreak)
	}
}

// TestSubmitResetsFailureStreak: a fresh user submission is a new goal, so a
// stale streak from the previous goal must not carry over and trip the nudge
// early. submit's non slash path zeroes failKey/failStreak.
func TestSubmitResetsFailureStreak(t *testing.T) {
	m := newTestModel(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "data: "+`{"type":"response.output_text.delta","delta":"ok"}`+"\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{}}\n\n")
	})
	m.failKey = tools.BashName + "|make build"
	m.failStreak = maxToolFailStreak - 1 // one away from firing
	mm, _ := m.submit("new goal", "new goal", promptEntry{display: "new goal"})
	final := mm.(Model)
	if final.failStreak != 0 || final.failKey != "" {
		t.Fatalf("submit must reset the failure streak for a new goal, got key=%q streak=%d",
			final.failKey, final.failStreak)
	}
}

// TestClearResetsFailureStreak: /clear starts the conversation over, including
// the repeated failure backstop's counters.
func TestClearResetsFailureStreak(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	m.failKey = tools.BashName + "|make build"
	m.failStreak = maxToolFailStreak - 1
	out, _ := m.runSlash("/clear")
	final := out.(Model)
	if final.failStreak != 0 || final.failKey != "" {
		t.Fatalf("/clear must reset the failure streak, got key=%q streak=%d",
			final.failKey, final.failStreak)
	}
}

// countSystem counts RoleSystem messages, the shape every nudge appends.
func countSystem(history []chmctx.Message) int {
	n := 0
	for _, msg := range history {
		if msg.Role == chmctx.RoleSystem {
			n++
		}
	}
	return n
}

// TestRunawayNudgeFiresAtMaxLLMRounds: the per turn round trip counter trips
// one soft system note when it reaches maxLLMRounds, and not before. Counted in
// round trips, not tool calls: batching inflates the call count, so a call based
// cap would fire on a healthy turn that batched its reads.
func TestRunawayNudgeFiresAtMaxLLMRounds(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})

	m.llmRounds = maxLLMRounds - 1
	m.maybeRunawayNudge()
	if n := countSystem(m.history); n != 0 {
		t.Fatalf("below the cap must not nudge, got %d system notes", n)
	}

	m.llmRounds = maxLLMRounds
	m.maybeRunawayNudge()
	if n := countSystem(m.history); n != 1 {
		t.Fatalf("at the cap expected one system nudge, got %d:\n%+v", n, m.history)
	}
	last := m.history[len(m.history)-1]
	if !strings.Contains(last.Content, fmt.Sprintf("%d round trips", maxLLMRounds)) {
		t.Fatalf("runaway nudge should name the count: %q", last.Content)
	}

	// Immediately after, and anywhere inside the interval, it must stay quiet.
	m.llmRounds = maxLLMRounds + runawayNudgeInterval - 1
	m.maybeRunawayNudge()
	if n := countSystem(m.history); n != 1 {
		t.Fatalf("inside the interval must not re-fire, got %d system notes", n)
	}
}

// TestRunawayNudgeRefiresPeriodically: past the cap the check must keep firing
// every runawayNudgeInterval rounds. Under the old once per turn latch a turn
// that sailed past the cap ran completely unsupervised for the rest of its life,
// which is exactly the runaway the nudge exists to catch.
func TestRunawayNudgeRefiresPeriodically(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})

	// Overshoot the cap without ever landing on it (the multi call jump).
	m.llmRounds = maxLLMRounds + 3
	m.maybeRunawayNudge()
	if n := countSystem(m.history); n != 1 {
		t.Fatalf("overshooting the cap must still fire once, got %d system notes", n)
	}
	m.llmRounds += runawayNudgeInterval
	m.maybeRunawayNudge()
	if n := countSystem(m.history); n != 2 {
		t.Fatalf("a full interval later must re-fire, got %d system notes", n)
	}
}

// TestEndTurnResetsToolRounds: the runaway counter is per turn, so endTurn must
// zero it or the next turn inherits a head start toward the cap.
func TestEndTurnResetsToolRounds(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	m.installTurnContext()
	m.toolRounds = 42
	m.endTurn()
	if m.toolRounds != 0 {
		t.Fatalf("endTurn must reset toolRounds, got %d", m.toolRounds)
	}
}

// TestVerifyNudgeFiresOnceAtMinRounds: the finish re grounding nudge trips one
// soft system note only once a turn has done real work (llmRounds >=
// verifyNudgeMinRounds, or toolRounds >= verifyNudgeMinCalls), and never below
// it; the latch keeps it to once per turn.
func TestVerifyNudgeFiresOnceAtMinRounds(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})

	m.toolRounds = verifyNudgeMinRounds - 1
	m.llmRounds = verifyNudgeMinRounds - 1
	if m.maybeVerifyNudge() {
		t.Fatal("below the min-rounds gate must not nudge")
	}
	if n := countSystem(m.history); n != 0 {
		t.Fatalf("a trivial turn must not be re-grounded, got %d system notes", n)
	}

	m.toolRounds = verifyNudgeMinRounds
	m.llmRounds = verifyNudgeMinRounds
	m.turnActed = true
	if !m.maybeVerifyNudge() {
		t.Fatal("at the min-rounds gate the nudge must fire and report it nudged")
	}
	if n := countSystem(m.history); n != 1 {
		t.Fatalf("at the gate expected one re grounding note, got %d:\n%+v", n, m.history)
	}
	last := m.history[len(m.history)-1]
	if !strings.Contains(last.Content, "unverified") || !strings.Contains(last.Content, "original request") {
		t.Fatalf("re grounding note must push honest verification against the original request: %q", last.Content)
	}
	// The note is re sent as context on every remaining round of the turn, and
	// it competes for attention with the actual task. The old 1240 char version
	// carried a whole install probe runbook that duplicated the system prompt;
	// that guidance belongs in the always on prompt, not stapled to a finish.
	// Pin the budget so it can't grow back.
	if len(last.Content) > 700 {
		t.Fatalf("re grounding note has grown back to %d chars; keep it a principle, not a runbook: %q", len(last.Content), last.Content)
	}

	// Latched: a later drain in the same turn must not re fire.
	m.toolRounds = verifyNudgeMinRounds + 50
	m.llmRounds = verifyNudgeMinRounds + 50
	if m.maybeVerifyNudge() {
		t.Fatal("latch must prevent a second re grounding nudge in the same turn")
	}
	if n := countSystem(m.history); n != 1 {
		t.Fatalf("latch must hold at one note, got %d", n)
	}
}

// TestVerifyNudgeFiresOnBatchedCalls: the batching instruction compresses a
// substantial build and claim turn below verifyNudgeMinRounds round trips
// (five rounds of three calls each is real work with a real false green
// risk), so the call count gate must trip the nudge on its own. Guards the
// hole where the prompt's biggest lever silently disabled the fourth backstop.
func TestVerifyNudgeFiresOnBatchedCalls(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	m.llmRounds = verifyNudgeMinRounds - 3 // few round trips...
	m.toolRounds = verifyNudgeMinCalls     // ...but many batched calls
	m.turnActed = true
	if !m.maybeVerifyNudge() {
		t.Fatal("a heavily-batched substantial turn must still be re-grounded")
	}
	if n := countSystem(m.history); n != 1 {
		t.Fatalf("expected one re grounding note, got %d", n)
	}
}

// TestVerifyNudgeRePromptsSubstantialCleanFinish: a substantial turn ending with a
// clean, nonempty summary must be reprompted once (phase back to thinking, a
// chat cmd returned, the re grounding note appended) rather than finalized, so
// the model verifies before handing back. This is the false green finish guard.
func TestVerifyNudgeRePromptsSubstantialCleanFinish(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	m.installTurnContext()
	m.phase = phaseStreaming
	m.toolRounds = verifyNudgeMinRounds
	m.llmRounds = verifyNudgeMinRounds
	m.turnActed = true
	m.history = []chmctx.Message{
		{Role: chmctx.RoleUser, Content: "build galaxy.html"},
		{Role: chmctx.RoleAssistant, Content: "Done: built galaxy.html with all features."},
	}
	out, cmd := m.handleStreamClosed()
	mm := out.(Model)
	if cmd == nil {
		t.Fatal("a substantial clean finish must reprompt (non-nil chat cmd)")
	}
	if mm.phase != phaseThinking {
		t.Fatalf("reprompt must leave phase thinking, got %v", mm.phase)
	}
	if !mm.verifyNudged {
		t.Fatal("verifyNudged must latch after the re grounding nudge fires")
	}
	if n := countSystem(mm.history); n != 1 {
		t.Fatalf("expected exactly one re grounding system note, got %d:\n%+v", n, mm.history)
	}
}

// TestVerifyNudgeSkipsTrivialTurn: a turn that did little work (toolRounds below
// the gate) must finish normally (finalized, no reprompt, no system note) so
// quick answers and one line edits aren't nagged.
func TestVerifyNudgeSkipsTrivialTurn(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	m.installTurnContext()
	m.phase = phaseStreaming
	m.toolRounds = verifyNudgeMinRounds - 1
	m.llmRounds = verifyNudgeMinRounds - 1
	m.history = []chmctx.Message{
		{Role: chmctx.RoleUser, Content: "what does this function do?"},
		{Role: chmctx.RoleAssistant, Content: "It hashes the input."},
	}
	out, cmd := m.handleStreamClosed()
	mm := out.(Model)
	if cmd != nil {
		t.Fatal("a trivial turn must finish, not reprompt")
	}
	if mm.phase != phaseIdle {
		t.Fatalf("a finished turn must be idle, got %v", mm.phase)
	}
	if n := countSystem(mm.history); n != 0 {
		t.Fatalf("a trivial turn must not be re-grounded, got %d system notes", n)
	}
}

// TestVerifyNudgeYieldsToEmptyReply: an empty newest assistant message is the
// empty reply nudge's domain even on a substantial turn: the verify nudge must
// not pre empt it (its note re grounds a summary that doesn't exist).
func TestVerifyNudgeYieldsToEmptyReply(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	m.installTurnContext()
	m.phase = phaseStreaming
	m.toolRounds = verifyNudgeMinRounds + 10
	m.llmRounds = verifyNudgeMinRounds + 10
	m.history = []chmctx.Message{
		{Role: chmctx.RoleUser, Content: "build it"},
		{Role: chmctx.RoleAssistant, Content: ""}, // empty: stopped mid task
	}
	out, _ := m.handleStreamClosed()
	mm := out.(Model)
	if mm.verifyNudged {
		t.Fatal("an empty reply belongs to the empty reply nudge; verify nudge must not fire")
	}
	last := mm.history[len(mm.history)-1]
	if last.Role != chmctx.RoleSystem || !strings.Contains(last.Content, "no reply and no tool call") {
		t.Fatalf("expected the empty reply nudge to own this finish, got %+v", last)
	}
}

// TestVerifyNudgeSkipsHonestUnverifiedFinish: a substantial turn whose summary
// already marks something `unverified` has done the honest self assessment the
// nudge exists to elicit (it is the OPPOSITE of a false green) so it must
// finish without a reprompt. Guards the round 5 regression where reprompting an
// honest "unverified: browser runtime" finish produced a confident, caveat free
// "it works" on the next round.
func TestVerifyNudgeSkipsHonestUnverifiedFinish(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	m.installTurnContext()
	m.phase = phaseStreaming
	m.toolRounds = verifyNudgeMinRounds + 20
	m.llmRounds = verifyNudgeMinRounds + 20
	m.history = []chmctx.Message{
		{Role: chmctx.RoleUser, Content: "build galaxy.html"},
		{Role: chmctx.RoleAssistant, Content: "Built galaxy.html. unverified: browser runtime: no browser in this sandbox to load it."},
	}
	out, cmd := m.handleStreamClosed()
	mm := out.(Model)
	if cmd != nil {
		t.Fatal("an honest unverified finish must not be re-prompted")
	}
	if mm.phase != phaseIdle {
		t.Fatalf("an honest unverified finish must finalize, got phase %v", mm.phase)
	}
	if mm.verifyNudged {
		t.Fatal("a suppressed nudge must not latch verifyNudged")
	}
	if n := countSystem(mm.history); n != 0 {
		t.Fatalf("an honest unverified finish must not be re-grounded, got %d system notes", n)
	}
}

// TestVerifyNudgeEndToEndRePromptsThenFinishes drives a full turn that does real
// work (8 bash rounds) then "finishes" with a confident summary. The finish
// re grounding nudge must inject one note and reprompt exactly once, and the
// turn must then complete idle: the false green finish path, end to end.
func TestVerifyNudgeEndToEndRePromptsThenFinishes(t *testing.T) {
	var round int
	handler := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		round++
		if round <= verifyNudgeMinRounds {
			// A real tool call so toolRounds climbs to the gate.
			fmt.Fprintf(w, "data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"function_call\",\"call_id\":\"c%d\",\"name\":\"bash\",\"arguments\":\"{\\\"cmd\\\":\\\"echo step\\\"}\"}}\n\n", round)
			fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"output_tokens\":5}}}\n\n")
			return
		}
		// A confident, toolless summary, what the galaxy runs shipped.
		fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"Done: galaxy.html built with all features.\"}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"output_tokens\":6}}}\n\n")
	}

	m := newTestModel(t, handler)
	final := drainFinal(t, m, "build galaxy.html")

	// 8 tool rounds + a summary that gets reprompted + the final summary = 10.
	if round != verifyNudgeMinRounds+2 {
		t.Fatalf("substantial finish must reprompt exactly once (want %d requests, got %d)", verifyNudgeMinRounds+2, round)
	}
	var nudges int
	for _, msg := range final.history {
		if msg.Role == chmctx.RoleSystem && strings.Contains(msg.Content, "original request") {
			nudges++
		}
	}
	if nudges != 1 {
		t.Fatalf("expected exactly one finish re grounding note in history, got %d", nudges)
	}
	if final.phase != phaseIdle {
		t.Fatalf("turn must end idle after the re-grounded finish, phase=%v", final.phase)
	}
}

// TestEndTurnResetsVerifyNudged: the latch is per turn, so endTurn must clear it
// or a later turn never re grounds.
func TestEndTurnResetsVerifyNudged(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	m.installTurnContext()
	m.verifyNudged = true
	m.endTurn()
	if m.verifyNudged {
		t.Fatal("endTurn must reset verifyNudged")
	}
}

// TestToolCallLeakWarningDetectsStrandedXML: a turn ending with leaked
// tool call XML stranded in the newest assistant message warns the user; clean
// text doesn't, and only the NEWEST assistant message is inspected.
func TestToolCallLeakWarningDetectsStrandedXML(t *testing.T) {
	// XML tool call body.
	coderLeak := "Let me search.\n<tool_call>\n<function=bash>\n<parameter=cmd>ls</parameter>\n</function>\n</tool_call>"
	// General JSON tool call body, the target model class, NO `<function=`.
	denseLeak := "Let me search.\n<tool_call>\n{\"name\": \"bash\", \"arguments\": {\"cmd\": \"ls\"}}\n</tool_call>"
	clean := "Done: built and tested, all green."

	for name, leak := range map[string]string{"coder-xml": coderLeak, "dense-json": denseLeak} {
		if w := toolCallLeakWarning([]chmctx.Message{{Role: chmctx.RoleAssistant, Content: leak}}); w == "" {
			t.Fatalf("%s: leaked tool call must produce a warning", name)
		}
	}
	if w := toolCallLeakWarning([]chmctx.Message{{Role: chmctx.RoleAssistant, Content: clean}}); w != "" {
		t.Fatalf("clean reply must not warn, got %q", w)
	}
	// A message that carried a real structured tool call never leaked, even if
	// its prose quotes the `<tool_call>` tag: the ToolCalls gate keeps it clean.
	withCall := chmctx.Message{
		Role:      chmctx.RoleAssistant,
		Content:   "Running the build via a <tool_call> now.",
		ToolCalls: []chmctx.ToolCall{{ID: "1", Name: "bash", Arguments: map[string]any{"cmd": "go build ./..."}}},
	}
	if w := toolCallLeakWarning([]chmctx.Message{withCall}); w != "" {
		t.Fatalf("a turn with a structured tool call must not warn on incidental prose, got %q", w)
	}
	// An old leak followed by a clean reply must stay silent: only the newest
	// assistant message is the one that just ended the turn.
	hist := []chmctx.Message{
		{Role: chmctx.RoleAssistant, Content: coderLeak},
		{Role: chmctx.RoleUser, Content: "go on"},
		{Role: chmctx.RoleAssistant, Content: clean},
	}
	if w := toolCallLeakWarning(hist); w != "" {
		t.Fatalf("only the newest assistant message should be checked, got %q", w)
	}
}

// TestStatusBarShowsSessionTokens: once the counter is nonzero it appears
// in the status bar; at zero the bar stays quiet.
func TestStatusBarShowsSessionTokens(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	if strings.Contains(m.renderStatusBar(), "tok") {
		t.Fatalf("fresh session (0 tok) should not render counter: %q", m.renderStatusBar())
	}
	m.sessionTokens = 1234
	bar := m.renderStatusBar()
	if !strings.Contains(bar, "1.2k tok") {
		t.Fatalf("status bar should show compact session counter: %q", bar)
	}
}

// TestStatusBarLiveTimerAndFrozenSummary: during an active turn the bar shows
// the phase label plus a ticking wall clock; at idle after a clean finish it
// shows the frozen ✓, the wall clock duration, and the avg rate that divides
// into it (5000 tok ÷ 100s = 50 tok/s).
func TestStatusBarLiveTimerAndFrozenSummary(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})

	m.phase = phaseStreaming
	m.turnStart = time.Now().Add(-90 * time.Second)
	bar := stripANSI(m.renderStatusBar())
	if !strings.Contains(bar, "generating") {
		t.Fatalf("active bar should show the phase label: %q", bar)
	}
	if !strings.Contains(bar, "1m 30s") {
		t.Fatalf("active bar should show the live wall-clock: %q", bar)
	}

	m.phase = phaseIdle
	m.turnStart = time.Time{}
	m.lastOutcome = outcomeDone
	m.lastElapsed = 100 * time.Second
	m.lastTokens = 5000
	bar = stripANSI(m.renderStatusBar())
	if !strings.Contains(bar, "✓ 1m 40s") {
		t.Fatalf("idle bar should show frozen outcome + duration: %q", bar)
	}
	if !strings.Contains(bar, "50 tok/s avg") {
		t.Fatalf("idle bar should show the avg rate: %q", bar)
	}
}

// TestClearResetsSessionTokens: /clear wipes the conversation AND the
// session counter: starting over from zero is the whole point.
func TestClearResetsSessionTokens(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	m.sessionTokens = 999
	out, _ := m.runSlash("/clear")
	if got := out.(Model).sessionTokens; got != 0 {
		t.Fatalf("/clear should reset sessionTokens to 0, got %d", got)
	}
}

// TestWrapRowsMatchesBubblesBehaviour: wrapRows must match the row count of
// bubbles/textarea's internal wrap(): word boundary aware, hard wrap fallback
// for over wide single words, and a trailing cursor anchor row when content
// exactly fills the width.
func TestWrapRowsMatchesBubblesBehaviour(t *testing.T) {
	cases := []struct {
		name     string
		input    string
		width    int
		wantRows int
	}{
		{"empty", "", 15, 1},
		{"short fits", "hello", 15, 1},
		{"short with spaces fits", "hi you", 15, 1},
		{"exactly fills width adds cursor anchor", strings.Repeat("x", 15), 15, 2},
		{"just under width", strings.Repeat("x", 14), 15, 1},
		{"just over width hard-wraps", strings.Repeat("x", 16), 15, 2},
		{"long hard-wrap no spaces", strings.Repeat("x", 45), 15, 4},
		{"word-boundary wastes space", strings.Repeat("hello ", 20), 15, 10},
		{"zero width floors to 1", "anything", 0, 1},
	}
	for _, c := range cases {
		if got := wrapRows(c.input, c.width); got != c.wantRows {
			t.Errorf("wrapRows(%q, %d) = %d, want %d", c.input, c.width, got, c.wantRows)
		}
	}
}

// TestVisualPromptLinesSumsAcrossLogicalLines: visualPromptLines splits on
// newlines and sums wrapRows for each segment: the prompt field grows to
// hold the combined visual height.
func TestVisualPromptLinesSumsAcrossLogicalLines(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	// width=100, effective=96. Two short logical lines = 2 visual rows.
	m.ta.SetValue("first line\nsecond line")
	if got := m.visualPromptLines(); got != 2 {
		t.Errorf("two short lines should produce 2 visual rows, got %d", got)
	}
	// One line that wraps to multiple visual rows.
	m.ta.SetValue(strings.Repeat("x", 200))
	if got := m.visualPromptLines(); got < 2 {
		t.Errorf("200-char line should wrap past 1 row, got %d", got)
	}
}

// TestPromptGrowsOnWrappedLongLine: a long paragraph typed without Enter has
// LineCount()==1, so relying on it sticks the textarea at 1 row while text wraps
// off screen. recomputeLayout must count *visual* rows.
func TestPromptGrowsOnWrappedLongLine(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	// newTestModel sets width=100 → effective text width ~96. 300 runes of
	// text wraps to 4 rows (ceil(300/96)).
	m.ta.SetValue(strings.Repeat("x", 300))
	m.recomputeLayout()
	if got := m.ta.Height(); got < 2 {
		t.Fatalf("long wrapped line should expand textarea past 1 row, got %d", got)
	}
}

// TestPromptAutoGrowsWithContent: the textarea starts at 1 line, grows with
// newlines, and clamps to height: minViewport: 2: popover, so big pastes use
// most of the screen while chat keeps its floor.
func TestPromptAutoGrowsWithContent(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	if m.ta.Height() != 1 {
		t.Fatalf("empty prompt should be 1 line, got %d", m.ta.Height())
	}

	m.ta.SetValue("line1\nline2\nline3\nline4")
	m.recomputeLayout()
	if got := m.ta.Height(); got != 4 {
		t.Fatalf("4 lines in textarea should produce height 4, got %d", got)
	}

	// Far past the cap, height must clamp to leave room for chat. With
	// newTestModel's 30 row terminal, cap = 30: 5: 2: 0 = 23.
	m.ta.SetValue(strings.Repeat("x\n", 40) + "end")
	m.recomputeLayout()
	want := m.height - minViewport - 2
	if got := m.ta.Height(); got != want {
		t.Fatalf("height should clamp to %d (h=%d: minViewport=%d: chrome=2), got %d",
			want, m.height, minViewport, got)
	}
}

// TestPromptShrinksAfterSubmit: after Enter submits a multi line prompt the
// textarea resets to empty and the height snaps back to 1 line.
func TestPromptShrinksAfterSubmit(t *testing.T) {
	m := newTestModel(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "data: "+`{"type":"response.output_text.delta","delta":"ok"}`+"\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{}}\n\n")
	})
	m.ta.SetValue("line1\nline2\nline3\nline4")
	m.recomputeLayout()
	if m.ta.Height() != 4 {
		t.Fatalf("precondition: 4-line draft, got height %d", m.ta.Height())
	}
	// Enter at idle submits the current textarea value and calls ta.Reset;
	// recomputeLayout after handleKey then snaps the height back down.
	out, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	final, _ := drain(out, cmd)
	if got := final.(Model).ta.Height(); got != 1 {
		t.Fatalf("after submit the textarea should collapse to 1 line, got %d", got)
	}
}

// stripANSI removes CSI escape sequences (`\x1b[…m`) so tests can match the
// visible text through glamour's per word styling, which splits "foo bar" into
// `<span>foo</span> <span>bar</span>` and breaks a naive Contains("foo bar").
func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			for j := i + 2; j < len(s); j++ {
				if s[j] >= 0x40 && s[j] <= 0x7e {
					i = j
					break
				}
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// TestStreamContentShowsLiveInViewport: a mid turn content event populates the
// streaming buffer, promotes phase thinking→streaming, and is visible in View()
// before any EventDone: the "tokens stream immediately" promise.
func TestStreamContentShowsLiveInViewport(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	m.phase = phaseThinking
	out, _ := m.handleStream(llm.Event{Kind: llm.EventContent, Content: "hello world"})
	om := out.(Model)
	if om.phase != phaseStreaming {
		t.Fatalf("first content chunk should promote phase to streaming, got %v", om.phase)
	}
	if om.streaming.String() != "hello world" {
		t.Fatalf("streaming buffer should contain raw content, got %q", om.streaming.String())
	}
	if om.scroll.Len() != 0 {
		t.Fatalf("scroll should still be empty before flush, got %q", om.scroll.String())
	}
	if !strings.Contains(om.View(), "hello world") {
		t.Fatalf("View must show live text during stream: %q", om.View())
	}
}

// TestEventDoneFlushesStreamingThroughGlamour: EventDone moves raw streamed
// text into the committed scroll via the Markdown renderer and empties the
// streaming buffer. After Done, scroll contains the rendered block and the
// streaming buffer is empty.
func TestEventDoneFlushesStreamingThroughGlamour(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	m.phase = phaseStreaming
	m.streaming.WriteString("done message")
	out, _ := m.handleStream(llm.Event{Kind: llm.EventDone, Final: &chmctx.Message{Role: chmctx.RoleAssistant, Content: "done message"}})
	om := out.(Model)
	if om.streaming.Len() != 0 {
		t.Fatalf("streaming should be empty after Done, got %q", om.streaming.String())
	}
	if !strings.Contains(stripANSI(om.scroll.String()), "done message") {
		t.Fatalf("rendered content should be in scroll: %q", om.scroll.String())
	}
}

// TestToolCallFlushesStreamedContent: a tool call event ends the content phase.
// Whatever streamed before it is rendered and committed *now*, so the user sees
// styled text *before* the inline tool call status, not all at once at turn end.
func TestToolCallFlushesStreamedContent(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	m.phase = phaseStreaming
	m.streaming.WriteString("I'll run bash.")
	call := chmctx.ToolCall{ID: "c1", Name: "bash"}
	out, _ := m.handleStream(llm.Event{Kind: llm.EventToolCall, ToolCall: &call})
	om := out.(Model)
	if om.streaming.Len() != 0 {
		t.Fatal("tool call must flush streaming buffer into scroll")
	}
	if !strings.Contains(stripANSI(om.scroll.String()), "I'll run bash") {
		t.Fatalf("content should be committed to scroll before tool call: %q", om.scroll.String())
	}
	if len(om.pending) != 1 || om.pending[0].Name != "bash" {
		t.Fatalf("tool call should land in pending: %+v", om.pending)
	}
}

// TestCancelMidStreamPreservesStreamedText: Ctrl+C while content is streaming
// keeps the partial response visible (flushed to scroll) and appends the
// cancelled marker. The user never loses context they've already read.
func TestCancelMidStreamPreservesStreamedText(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	ctx, cancel := context.WithCancel(context.Background())
	m.turnCtx = ctx
	m.cancel = cancel
	m.phase = phaseStreaming
	m.streaming.WriteString("partial response before cancel")

	out, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	om := out.(Model)

	if om.streaming.Len() != 0 {
		t.Fatal("streaming buffer should be flushed by cancel")
	}
	if !strings.Contains(stripANSI(om.scroll.String()), "partial response before cancel") {
		t.Fatalf("streamed text must survive cancel, got: %q", om.scroll.String())
	}
	if !strings.Contains(stripANSI(om.scroll.String()), "cancelled") {
		t.Fatalf("cancelled marker missing from scroll: %q", om.scroll.String())
	}
	if om.phase.active() {
		t.Fatal("phase must return to idle after cancel")
	}
}

// TestHandleStreamDrainsAfterCancel: buffered stream events can arrive after
// Ctrl+C returned phase to idle. Processing them would write ghost tokens, re
// populate m.pending, and credit a cancelled turn's usage to sessionTokens.
// handleStream must drain only.
func TestHandleStreamDrainsAfterCancel(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	m.phase = phaseIdle // post cancel state
	m.sessionTokens = 100

	out, _ := m.handleStream(llm.Event{Kind: llm.EventContent, Content: "ghost"})
	om := out.(Model)
	if om.streaming.Len() != 0 {
		t.Fatalf("stale EventContent wrote to streaming: %q", om.streaming.String())
	}
	out, _ = om.handleStream(llm.Event{Kind: llm.EventToolCall, ToolCall: &chmctx.ToolCall{ID: "c", Name: "bash"}})
	om = out.(Model)
	if len(om.pending) != 0 {
		t.Fatalf("stale EventToolCall re-populated pending: %+v", om.pending)
	}
	out, _ = om.handleStream(llm.Event{Kind: llm.EventDone, Tokens: 50})
	om = out.(Model)
	if om.sessionTokens != 100 {
		t.Fatalf("stale EventDone credited tokens to session: %d", om.sessionTokens)
	}
}

// TestHandleStreamClosedSkipsAdvanceAfterCancel: after Ctrl+C leaves phase=idle,
// the deferred streamClosedMsg must not auto restart a turn (the agent re
// entering chat after a stop would surprise the user). No history mutation.
func TestHandleStreamClosedSkipsAdvanceAfterCancel(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	m.phase = phaseIdle // handleCtrlC already finalised
	m.history = []chmctx.Message{{Role: chmctx.RoleUser, Content: "earlier"}}
	before := len(m.history)

	out, cmd := m.handleStreamClosed()
	om := out.(Model)
	if cmd != nil {
		t.Fatal("stale close after cancel must NOT return a new-turn Cmd")
	}
	if len(om.history) != before {
		t.Fatalf("stale close after cancel must not mutate history, grew by %d", len(om.history)-before)
	}
}

// TestStaleStreamEventDoesNotMutateLiveTurn: after Ctrl+C kills turn 1 and the
// user submits turn 2, turn 1's readEvent Cmd can still fire (buffered channel
// or producer not yet exited). That stale event must not write turn 2's
// streaming buffer or session counters.
func TestStaleStreamEventDoesNotMutateLiveTurn(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	stale := make(chan llm.Event, 1)
	live := make(chan llm.Event, 1)
	m.stream = live
	m.phase = phaseThinking
	m.sessionTokens = 50

	out, _ := m.Update(streamEventMsg{ch: stale, e: llm.Event{Kind: llm.EventContent, Content: "ghost"}})
	om := out.(Model)
	if om.streaming.Len() != 0 {
		t.Fatalf("stale content event leaked into live turn's streaming buffer: %q", om.streaming.String())
	}
	if om.sessionTokens != 50 {
		t.Fatalf("stale event credited tokens to live session: %d", om.sessionTokens)
	}
	if om.stream != live {
		t.Fatal("stale event must not overwrite live m.stream")
	}
}

// TestStaleStreamCloseDoesNotKillLiveTurn: the prior turn's channel closing
// after a fresh submit must NOT run handleStreamClosed against the live turn.
// Doing so would (a) nil out m.stream, breaking the live read loop, and
// (b) finalizeTurn + endTurn the current request out from under the user.
func TestStaleStreamCloseDoesNotKillLiveTurn(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	stale := make(chan llm.Event)
	live := make(chan llm.Event, 1)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	m.turnCtx = ctx
	m.cancel = cancel
	m.stream = live
	m.phase = phaseStreaming

	out, _ := m.Update(streamClosedMsg{ch: stale})
	om := out.(Model)
	if om.stream != live {
		t.Fatal("stale close must not overwrite m.stream: live read loop would die")
	}
	if !om.phase.active() {
		t.Fatalf("stale close finalised the live turn: phase is now %v", om.phase)
	}
	if om.cancel == nil {
		t.Fatal("stale close cancelled the live turn's context")
	}
}

// TestStaleToolResultDoesNotEnterLiveHistory: a bash result from cancelled turn
// N must not append to turn N+1's history (its unmatched tool_call_id would 400
// the next /v1 request) and must not steal the live stream via startChat.
func TestStaleToolResultDoesNotEnterLiveHistory(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})

	// Live turn N+1.
	ctxLive, cancelLive := context.WithCancel(context.Background())
	t.Cleanup(cancelLive)
	m.turnCtx = ctxLive
	m.cancel = cancelLive
	m.stream = make(chan llm.Event, 1)
	m.phase = phaseStreaming
	m.history = []chmctx.Message{{Role: chmctx.RoleUser, Content: "live prompt"}}
	beforeLen := len(m.history)
	beforeStream := m.stream

	// Stale toolResultMsg carrying turn N's (already cancelled) ctx.
	ctxStale, cancelStale := context.WithCancel(context.Background())
	cancelStale()
	stale := toolResultMsg{
		Msg:     chmctx.Message{Role: chmctx.RoleTool, ToolCallID: "stale", Content: "ghost"},
		turnCtx: ctxStale,
	}
	out, cmd := m.Update(stale)
	om := out.(Model)
	if len(om.history) != beforeLen {
		t.Fatalf("stale tool result entered live history: %+v", om.history)
	}
	if om.stream != beforeStream {
		t.Fatal("stale tool result triggered a fresh startChat: live stream replaced")
	}
	if cmd != nil {
		t.Fatalf("stale tool result returned a Cmd; should be no-op: %T", cmd)
	}
}

// TestRunToolCallHonorsBashTimeoutBeyondLegacyCap: bash's own timeout is the only
// ceiling: runToolCall must not wrap the parent context in a shorter cap. Runs a
// fast `echo` with a 1800s tool arg timeout and asserts it completes normally.
func TestRunToolCallHonorsBashTimeoutBeyondLegacyCap(t *testing.T) {
	parent := context.Background() // no outer deadline
	cmd := runToolCall(parent, chmctx.ToolCall{
		ID:   "t-cap",
		Name: "bash",
		Arguments: map[string]any{
			"cmd":             "echo through-the-cap",
			"timeout_seconds": float64(1800), // 10× the old 3 min ceiling
		},
	})
	start := time.Now()
	msg := cmd()
	elapsed := time.Since(start)

	result, ok := msg.(toolResultMsg)
	if !ok {
		t.Fatalf("expected toolResultMsg, got %T", msg)
	}
	if !strings.Contains(result.Msg.Content, "through-the-cap") {
		t.Fatalf("bash output missing: call may have been killed: %q", result.Msg.Content)
	}
	if strings.Contains(result.Msg.Content, "timeout") || strings.Contains(result.Msg.Content, "cancelled") {
		t.Fatalf("bash should not have been timed-out or cancelled: %q", result.Msg.Content)
	}
	// Sanity: a fast echo finishes in ms, not minutes, a canary for runToolCall
	// re introducing a blocking outer wrapper.
	if elapsed > 10*time.Second {
		t.Fatalf("bash took %s: runToolCall is doing more than passing through", elapsed)
	}
}

// TestEventErrorPreservesStreamedText: a stream error mid content flushes the
// partial text before appending the error line, same principle as cancel,
// user keeps the context they were reading.
func TestEventErrorPreservesStreamedText(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	m.phase = phaseStreaming
	m.streaming.WriteString("got this far then")
	out, _ := m.handleStream(llm.Event{Kind: llm.EventError, Err: fmt.Errorf("boom")})
	om := out.(Model)
	if om.streaming.Len() != 0 {
		t.Fatal("streaming should be flushed on error")
	}
	if !strings.Contains(stripANSI(om.scroll.String()), "got this far then") {
		t.Fatalf("pre-error content must survive: %q", om.scroll.String())
	}
	if !strings.Contains(stripANSI(om.scroll.String()), "boom") {
		t.Fatalf("error message should follow the content: %q", om.scroll.String())
	}
	if om.phase.active() {
		t.Fatal("phase must return to idle after error")
	}
}

// drain advances the model until no async commands remain, a synchronous
// bubbletea mini runtime. tea.BatchMsg arrives when Update wraps multiple Cmds
// (e.g. tea.Println from the outbox + the handler's own Cmd); each child runs,
// its result feeds back through Update, and any new Cmd is queued.
func drain(m tea.Model, cmd tea.Cmd) (tea.Model, []tea.Msg) {
	var seen []tea.Msg
	queue := []tea.Cmd{cmd}
	for len(queue) > 0 {
		cmd, queue = queue[0], queue[1:]
		if cmd == nil {
			continue
		}
		msg := cmd()
		if msg == nil {
			continue
		}
		seen = append(seen, msg)
		if batch, ok := msg.(tea.BatchMsg); ok {
			for _, c := range batch {
				if c == nil {
					continue
				}
				bm, bcmd := m.Update(c())
				m = bm
				queue = append(queue, bcmd)
			}
			continue
		}
		var nextCmd tea.Cmd
		m, nextCmd = m.Update(msg)
		queue = append(queue, nextCmd)
	}
	return m, seen
}

// TestPopoverRenderHasNoMarker: no row in the rendered popover is prefixed
// with the old `▸ ` arrow or a 2 space marker. Selection is a colour change,
// not a marker.
func TestPopoverRenderHasNoMarker(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	mm := typeInto(m, "/")
	rendered := stripANSI(mm.renderPopover())
	for line := range strings.SplitSeq(rendered, "\n") {
		if strings.HasPrefix(line, "▸ ") || strings.HasPrefix(line, "  /") {
			t.Fatalf("popover row must not carry a marker prefix: %q", line)
		}
	}
}

// TestPopoverSelectionStaysVisible: with more than popoverCap suggestions the
// popover renders a window; when the selection moves past it, renderPopover must
// scroll so the highlighted row stays shown, else Enter commits an unseen row.
func TestPopoverSelectionStaysVisible(t *testing.T) {
	m := Model{width: 80, suggestOpen: true}
	for i := 0; i < popoverCap+4; i++ {
		m.suggest = append(m.suggest, argOption{value: fmt.Sprintf("profile%d", i)})
	}
	m.suggestIdx = len(m.suggest) - 1 // last row, well past the first window
	rendered := stripANSI(m.renderPopover())
	if !strings.Contains(rendered, m.suggest[m.suggestIdx].value) {
		t.Fatalf("selected row %q not visible in popover:\n%s",
			m.suggest[m.suggestIdx].value, rendered)
	}
}

// TestSplashEmittedOnFirstSize: the splash prints once into scrollback on the
// first WindowSizeMsg, then scrolls up as content arrives. We verify the lines
// reach the outbox (drained via tea.Println by the Update wrapper in production).
func TestSplashEmittedOnFirstSize(t *testing.T) {
	cfg, _, _ := config.Bootstrap(t.TempDir())
	m := New(cfg, llm.New("http://x", cfg.ActiveProfile().LLM, ""), t.TempDir(), "test")
	out, _ := m.update(tea.WindowSizeMsg{Width: 100, Height: 30})
	om := out.(Model)
	joined := stripANSI(strings.Join(om.outbox, "\n"))
	if !strings.Contains(joined, "hamr") {
		t.Fatalf("splash should be queued on first size: %s", joined)
	}
	if !strings.Contains(joined, "codehamr test") {
		t.Fatalf("splash should carry version/profile line: %s", joined)
	}
	if !strings.Contains(joined, "AI systems can make mistakes") {
		t.Fatalf("splash should carry AI safety notice: %s", joined)
	}
	if !strings.Contains(joined, "devcontainer or VM") {
		t.Fatalf("splash should recommend a sandbox: %s", joined)
	}
	// A second size message must not reemit the splash. Clear the captured
	// outbox first, since production drains it but we called update() directly.
	om.outbox = nil
	out2, _ := om.update(tea.WindowSizeMsg{Width: 80, Height: 24})
	om2 := out2.(Model)
	if joined2 := strings.Join(om2.outbox, "\n"); strings.Contains(joined2, "hamr time") {
		t.Fatalf("splash must be a one-shot, got re-emit: %s", joined2)
	}
}

// TestStreamingShownInLiveView: the streaming buffer is rendered above
// the prompt by View() while content is in flight, so the user sees
// tokens immediately even before the block flushes to scrollback.
func TestStreamingShownInLiveView(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	m.streaming.WriteString("live tokens arriving...")
	if !strings.Contains(stripANSI(m.View()), "live tokens arriving") {
		t.Fatalf("View must include streaming buffer: %s", stripANSI(m.View()))
	}
}

// TestFocusBlurMsgsAreInert: terminal focus reports arrive as tea.FocusMsg /
// tea.BlurMsg under tea.WithReportFocus. They must never touch the textarea (no
// inserted chars, no height change), else the UI slides up when another window
// steals focus.
func TestFocusBlurMsgsAreInert(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	beforeVal := m.ta.Value()
	beforeHeight := m.ta.Height()

	out, cmd := m.Update(tea.FocusMsg{})
	om := out.(Model)
	if om.ta.Value() != beforeVal {
		t.Fatalf("FocusMsg altered textarea: %q → %q", beforeVal, om.ta.Value())
	}
	if om.ta.Height() != beforeHeight {
		t.Fatalf("FocusMsg changed textarea height: %d → %d", beforeHeight, om.ta.Height())
	}
	if cmd != nil {
		t.Fatal("FocusMsg must not return a Cmd")
	}

	out, cmd = m.Update(tea.BlurMsg{})
	om = out.(Model)
	if om.ta.Value() != beforeVal || om.ta.Height() != beforeHeight {
		t.Fatalf("BlurMsg altered state: val=%q h=%d",
			om.ta.Value(), om.ta.Height())
	}
	if cmd != nil {
		t.Fatal("BlurMsg must not return a Cmd")
	}
}

// TestEmptyRunesKeyIsDropped: KeyMsg{Type: KeyRunes, Runes: nil} arises when
// bubbletea's parser chokes on a partial escape sequence. Falling through
// inserts nothing but triggers recomputeLayout: a stream of them flickers the
// UI, so drop them at the front door.
func TestEmptyRunesKeyIsDropped(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	beforeVal := m.ta.Value()
	out, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: nil})
	om := out.(Model)
	if om.ta.Value() != beforeVal {
		t.Fatalf("empty-runes key leaked into textarea: %q", om.ta.Value())
	}
}

// TestPopoverRenderRightAligns: each row ends flush with the popover width
// (== m.width after ANSI stripping). The value starts at column 0 and the
// description is right aligned.
func TestPopoverRenderRightAligns(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	mm := typeInto(m, "/")
	rendered := stripANSI(mm.renderPopover())
	for line := range strings.SplitSeq(rendered, "\n") {
		if line == "" {
			continue
		}
		if got := lipgloss.Width(line); got != mm.width {
			t.Fatalf("row width = %d, want %d: %q", got, mm.width, line)
		}
		// value starts at column 0: first rune is a slash
		if !strings.HasPrefix(line, "/") {
			t.Fatalf("row should start with the command name at column 0: %q", line)
		}
	}
}

// TestMultiToolCallRoundExecutesAllBeforeNextChat: when the model emits multiple
// tool calls in one round, EVERY result must be appended to history BEFORE the
// next chat round. OpenAI rejects an `assistant.tool_calls` message followed by
// fewer `tool` messages than calls issued; the captured request body lets us
// assert both results land before the round 2 dispatch.
func TestMultiToolCallRoundExecutesAllBeforeNextChat(t *testing.T) {
	var roundBodies [][]byte
	turn := 0
	m := newTestModel(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		roundBodies = append(roundBodies, body)
		turn++
		w.Header().Set("Content-Type", "text/event-stream")
		switch turn {
		case 1:
			fmt.Fprintf(w, "data: %s\n\n", `{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","call_id":"c1","name":"bash","arguments":"{\"cmd\":\"echo first\"}"}}`)
			fmt.Fprintf(w, "data: %s\n\n", `{"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","call_id":"c2","name":"bash","arguments":"{\"cmd\":\"echo second\"}"}}`)
			fmt.Fprintf(w, "data: %s\n\n", `{"type":"response.completed","response":{"usage":{"output_tokens":5}}}`)
		default:
			// Round 2 ends the turn: a plain content reply with NO tool call.
			// Emitting a nonexistent tool here would loop drain forever: runRaw
			// returns "(unknown tool: ...)" and reenters chat indefinitely.
			fmt.Fprintf(w, "data: %s\n\n", `{"type":"response.output_text.delta","delta":"both echoes finished"}`)
			fmt.Fprintf(w, "data: %s\n\n", `{"type":"response.completed","response":{"usage":{"output_tokens":1}}}`)
		}
	})
	mm, cmd := m.submit("two echoes", "two echoes", promptEntry{display: "two echoes"})
	out, _ := drain(mm, cmd)
	final := out.(Model)

	if turn != 2 {
		t.Fatalf("expected exactly 2 LLM rounds, got %d", turn)
	}
	if !bytes.Contains(roundBodies[1], []byte("first")) {
		t.Fatalf("round 2 request missing tool_result for first call:\n%s", roundBodies[1])
	}
	if !bytes.Contains(roundBodies[1], []byte("second")) {
		t.Fatalf("round 2 request missing tool_result for second call:\n%s", roundBodies[1])
	}
	toolResults := 0
	for _, msg := range final.history {
		if msg.Role == chmctx.RoleTool {
			toolResults++
		}
	}
	if toolResults != 2 {
		t.Fatalf("expected 2 tool results in history, got %d:\n%+v", toolResults, final.history)
	}
	if len(final.pending) != 0 {
		t.Fatalf("pending should be drained after the turn, got %d leftover calls", len(final.pending))
	}
}

// TestEndTurnResetsPendingSoStaleCallsDoNotLeakIntoNextTurn: a turn can end with
// calls still in m.pending. Without resetting pending in endTurn, the next
// submission picks up the stale call, dispatches it against the new turn's
// context, and appends a tool_result whose tool_call_id points at the previous
// turn's assistant message: the orphan shape OpenAI rejects with 400.
func TestEndTurnResetsPendingSoStaleCallsDoNotLeakIntoNextTurn(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	m.installTurnContext()
	m.phase = phaseStreaming
	m.pending = []chmctx.ToolCall{
		{ID: "stale-c1", Name: "bash", Arguments: map[string]any{"cmd": "echo stale"}},
	}
	m.endTurn()
	if len(m.pending) != 0 {
		t.Fatalf("endTurn must drop pending tool calls, got %d leftover", len(m.pending))
	}
}

// TestStaleProbeDoesNotPrintActivationBannerForNonActiveProfile: a probe for a
// profile the user has /models'd away from must not print "✓ active: <profile>":
// the banner would be a lie. Pairs with the connection state guard in
// TestStaleProbeForOldProfileDoesNotOverwriteConnectedFlag.
func TestStaleProbeDoesNotPrintActivationBannerForNonActiveProfile(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	m.cfg.Active = "local"

	out, _ := m.handleProbe(probeMsg{profile: "remote"})
	final := out.(Model)
	if got := stripANSI(final.scroll.String()); strings.Contains(got, "✓ active: remote") {
		t.Fatalf("stale probe must not print activation banner for non-active profile:\n%s", got)
	}

	// Sanity: probe for the live profile with the live client DOES print the banner.
	m2 := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	m2.cfg.Active = "local"
	out2, _ := m2.handleProbe(probeMsg{profile: "local", cli: m2.cli})
	final2 := out2.(Model)
	if got := stripANSI(final2.scroll.String()); !strings.Contains(got, "✓ active: local") {
		t.Fatalf("active-profile probe must print activation banner:\n%s", got)
	}
}

// TestNewestAssistantEmpty pins the anomaly detector behind the empty reply
// nudge: only a newest assistant message with neither text nor a structured
// tool call counts as empty. A summary or a tool call is a normal turn.
func TestNewestAssistantEmpty(t *testing.T) {
	cases := []struct {
		name string
		hist []chmctx.Message
		want bool
	}{
		{"empty assistant", []chmctx.Message{
			{Role: chmctx.RoleUser, Content: "go"},
			{Role: chmctx.RoleAssistant, Content: ""},
		}, true},
		{"whitespace-only assistant", []chmctx.Message{
			{Role: chmctx.RoleAssistant, Content: "  \n\t"},
		}, true},
		{"assistant with summary", []chmctx.Message{
			{Role: chmctx.RoleAssistant, Content: "done"},
		}, false},
		{"assistant with tool call", []chmctx.Message{
			{Role: chmctx.RoleAssistant, ToolCalls: []chmctx.ToolCall{{Name: "bash"}}},
		}, false},
		{"newest is tool result, prior assistant empty", []chmctx.Message{
			{Role: chmctx.RoleAssistant, Content: ""},
			{Role: chmctx.RoleTool, Content: "out"},
		}, true},
		{"no assistant at all", []chmctx.Message{
			{Role: chmctx.RoleUser, Content: "go"},
		}, false},
	}
	for _, tc := range cases {
		if got := newestAssistantEmpty(tc.hist); got != tc.want {
			t.Errorf("%s: newestAssistantEmpty = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestEmptyReplyNudgeRePromptsThenRecovers: a turn whose first round comes back
// with no content and no tool call (the dominant silent death: a thinking
// model's tool call swallowed into the reasoning channel) must be reprompted
// once, not ended silently. Here the second round produces a real summary, so
// the run self heals.
func TestEmptyReplyNudgeRePromptsThenRecovers(t *testing.T) {
	var round int
	handler := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		round++
		if round == 1 {
			// Empty assistant message: stop with no content delta, no tool calls.
			fmt.Fprintf(w, "data: %s\n\n", `{"type":"response.completed","response":{"usage":{"output_tokens":1}}}`)
			return
		}
		fmt.Fprintf(w, "data: %s\n\n", `{"type":"response.output_text.delta","delta":"fixed and verified"}`)
		fmt.Fprintf(w, "data: %s\n\n", `{"type":"response.completed","response":{"usage":{"output_tokens":3}}}`)
	}

	m := newTestModel(t, handler)
	final := drainFinal(t, m, "build it")

	if round != 2 {
		t.Fatalf("empty reply must trigger exactly one reprompt (want 2 requests, got %d)", round)
	}
	var sawNudge bool
	for _, msg := range final.history {
		if msg.Role == chmctx.RoleSystem && strings.Contains(msg.Content, "no reply and no tool call") {
			sawNudge = true
		}
	}
	if !sawNudge {
		t.Fatalf("expected an empty reply system nudge in history; got %+v", final.history)
	}
	scroll := stripANSI(final.scroll.String())
	if !strings.Contains(scroll, "fixed and verified") {
		t.Fatalf("recovered summary missing from scroll:\n%s", scroll)
	}
	if strings.Contains(scroll, "dropped the call") {
		t.Fatalf("a recovered turn must not surface the persistent-empty diagnostic:\n%s", scroll)
	}
	if final.phase != phaseIdle {
		t.Fatalf("turn must end idle after recovery, phase=%v", final.phase)
	}
}

// TestEmptyReplyNudgeFiresOnceThenSurfaces: if the model stays empty even after
// the reprompt (e.g. a server that deterministically swallows the call), the
// latch must stop at one retry (no infinite loop) and the failure must be
// surfaced rather than dying silently as before.
func TestEmptyReplyNudgeFiresOnceThenSurfaces(t *testing.T) {
	var round int
	handler := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		round++
		fmt.Fprintf(w, "data: %s\n\n", `{"type":"response.completed","response":{"usage":{"output_tokens":1}}}`)
	}

	m := newTestModel(t, handler)
	final := drainFinal(t, m, "build it")

	if round != 2 {
		t.Fatalf("latch must bound re-prompts to one (want 2 requests total, got %d)", round)
	}
	scroll := stripANSI(final.scroll.String())
	if !strings.Contains(scroll, "dropped the call") {
		t.Fatalf("a persistently-empty turn must surface a diagnostic, not die silently:\n%s", scroll)
	}
	if final.phase != phaseIdle {
		t.Fatalf("turn must end idle, phase=%v", final.phase)
	}
}

// TestEmptyReplyNudgeReArmsAfterProgress pins the galaxy1 fix: once an empty
// reply has nudged this turn, a round that issues a real tool call is genuine
// progress and must rearm the latch, so a LATER transient empty on the same
// long turn earns its own reprompt instead of hitting the leak and die branch.
// A flaky stream that drops the occasional call must not abandon a half built
// file. Two CONSECUTIVE empties (no tool call between) still terminate: that
// path has pending=0 and so never reaches the rearm.
func TestEmptyReplyNudgeReArmsAfterProgress(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	m.installTurnContext()
	m.phase = phaseStreaming
	m.emptyNudged = true // a prior empty already nudged this turn
	m.pending = []chmctx.ToolCall{{ID: "c1", Name: "bash", Arguments: map[string]any{"cmd": "echo hi"}}}
	m.history = []chmctx.Message{
		{Role: chmctx.RoleUser, Content: "build it"},
		{Role: chmctx.RoleAssistant, ToolCalls: []chmctx.ToolCall{{ID: "c1", Name: "bash"}}},
	}
	out, _ := m.handleStreamClosed()
	if out.(Model).emptyNudged {
		t.Fatal("a round that issued a tool call (progress) must re-arm the empty reply latch, not leave it consumed for the rest of the turn")
	}
}

// drainFinal submits a prompt and pumps the whole turn to completion, returning
// the settled model. Centralises the submit+drain dance the empty reply tests share.
func drainFinal(t *testing.T, m Model, prompt string) Model {
	t.Helper()
	mm, cmd := m.submit(prompt, prompt, promptEntry{display: prompt})
	out, _ := drain(mm, cmd)
	return out.(Model)
}

// TestRetryEventDrivesStatusBar: an EventRetry surfaces the backoff hint in
// the status bar (so the wait doesn't read as a frozen turn), and the next
// stream event clears it; content means the retry succeeded, an error means
// the banner takes over.
func TestRetryEventDrivesStatusBar(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	m.phase = phaseThinking

	out, _ := m.handleStream(llm.Event{Kind: llm.EventRetry, Content: "retry 1/3 in 2s", Err: fmt.Errorf("404: not found")})
	m = out.(Model)
	if m.status != "retry 1/3 in 2s" {
		t.Fatalf("retry hint must land in the status bar, got %q", m.status)
	}

	out, _ = m.handleStream(llm.Event{Kind: llm.EventContent, Content: "hi"})
	m = out.(Model)
	if m.status != "" {
		t.Fatalf("status must clear once the stream resumes, got %q", m.status)
	}
}

// TestSubmitRecoversFromTransient404EndToEnd: the full submit → stream loop
// against a backend whose first response is a 404 (the issue #7 involving a proxy and
// LiteLLM hiccup). The turn must retry transparently and finish with the
// assistant's content in scrollback, never an error banner.
func TestSubmitRecoversFromTransient404EndToEnd(t *testing.T) {
	attempts := 0
	m := newTestModel(t, func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		if attempts == 1 {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"detail":"Not Found"}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\n", `{"type":"response.output_text.delta","delta":"recovered"}`)
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{}}\n\n")
	})
	m.cli.RetryBackoff = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}

	mm, cmd := m.submit("ping", "ping", promptEntry{display: "ping"})
	out, _ := drain(mm, cmd)
	final := out.(Model)

	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2 (one 404, one success)", attempts)
	}
	scroll := stripANSI(final.scroll.String())
	if !strings.Contains(scroll, "recovered") {
		t.Fatalf("assistant content missing after retry: %q", scroll)
	}
	if strings.Contains(scroll, "404") {
		t.Fatalf("a recovered turn must not surface the 404 to the user: %q", scroll)
	}
	if final.status != "" {
		t.Fatalf("retry hint must be cleared after recovery, got %q", final.status)
	}
	if final.phase.active() {
		t.Fatalf("turn must end idle, phase=%v", final.phase)
	}
}

// TestToolSchemasFitFixedToolsReservation is the FixedSystem pin's missing
// sibling. The four schemas ride on every request exactly like the prompt does,
// and they grow the same way (read_file gained offset/limit, write_file gained
// append). Unpinned, the next parameter silently over allocates history on
// small ctx profiles. On failure raise ctx.FixedTools; don't loosen this.
func TestToolSchemasFitFixedToolsReservation(t *testing.T) {
	cfg, _, _ := config.Bootstrap(t.TempDir())
	m := New(cfg, llm.New("http://x", cfg.ActiveProfile().LLM, ""), "/workspaces/codehamr", "test")
	wire, err := json.Marshal(m.buildTools())
	if err != nil {
		t.Fatal(err)
	}
	if cost := len(wire) / 4; cost > chmctx.FixedTools {
		t.Fatalf("tool schemas cost %d tokens, FixedTools reserves only %d: "+
			"raise ctx.FixedTools so Budget() doesn't over-allocate to history",
			cost, chmctx.FixedTools)
	}
}

// TestFailureStreakSurvivesBatchedSuccess: the loop breaker must count a target
// that keeps failing even when the model batches a succeeding call alongside it.
// Resetting the streak on ANY success is defeated by the most natural batch
// there is: a failing edit_file paired with a succeeding read_file: which
// would leave the first supervision to the runaway cap, dozens of round trips
// later.
func TestFailureStreakSurvivesBatchedSuccess(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	for range maxToolFailStreak {
		m.lastToolKey = "edit_file|src/app.js"
		m.recordToolOutcome(tools.EditFileName, "(not found: old_string does not appear in src/app.js)")
		m.lastToolKey = "read_file|src/app.js"
		m.recordToolOutcome(tools.ReadFileName, "some file content")
	}
	if m.failStreak < maxToolFailStreak {
		t.Fatalf("interleaved READ success reset the streak: got %d, want >= %d", m.failStreak, maxToolFailStreak)
	}
	m.maybeFailureNudge()
	if n := countSystem(m.history); n != 1 {
		t.Fatalf("a same target failure streak must nudge, got %d system notes", n)
	}
	// A success on the FAILING target still clears it: that is real recovery.
	m.lastToolKey = "edit_file|src/app.js"
	m.failKey, m.failStreak = "edit_file|src/app.js", 3
	m.recordToolOutcome(tools.EditFileName, "edited src/app.js")
	if m.failStreak != 0 {
		t.Fatalf("a success on the failing target must clear the streak, got %d", m.failStreak)
	}
}

// TestVerifyNudgeSkipsReadOnlyTurn: a turn that only read files produced no
// artifact that could be falsely called green, so re grounding it is a wasted
// round trip on every research question.
func TestVerifyNudgeSkipsReadOnlyTurn(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	m.toolRounds = verifyNudgeMinRounds + 20
	m.llmRounds = verifyNudgeMinRounds + 20
	m.turnActed = false
	m.history = []chmctx.Message{
		{Role: chmctx.RoleUser, Content: "how does packing work?"},
		{Role: chmctx.RoleAssistant, Content: "Pack walks newest-first into Budget()."},
	}
	if m.maybeVerifyNudge() {
		t.Fatal("a read-only turn must not be re-grounded")
	}
	// The same turn, once it acts, is back in scope.
	m.turnActed = true
	if !m.maybeVerifyNudge() {
		t.Fatal("a turn that acted must still be re-grounded")
	}
}

// TestMidStreamDropReplaysWithoutDuplicating: a socket that dies mid answer
// must be retried transparently instead of ending the turn. History is written
// only on EventDone, so the replayed round must leave exactly one assistant
// message: not the partial text plus the retry.
func TestMidStreamDropReplaysWithoutDuplicating(t *testing.T) {
	var round int
	handler := func(w http.ResponseWriter, _ *http.Request) {
		round++
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"half an ans\"}\n\n")
		w.(http.Flusher).Flush()
		if round == 1 {
			conn, _, _ := w.(http.Hijacker).Hijack()
			conn.Close()
			return
		}
		fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"wer.\"}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"output_tokens\":4}}}\n\n")
	}
	m := newTestModel(t, handler)
	final := drainFinal(t, m, "answer me")

	if round != 2 {
		t.Fatalf("a mid stream drop must be replayed once (want 2 requests, got %d)", round)
	}
	var assistants []string
	for _, msg := range final.history {
		if msg.Role == chmctx.RoleAssistant {
			assistants = append(assistants, msg.Content)
		}
	}
	if len(assistants) != 1 {
		t.Fatalf("replay must not duplicate the assistant message, got %d: %q", len(assistants), assistants)
	}
	if assistants[0] != "half an answer." {
		t.Fatalf("replayed round should carry the complete answer, got %q", assistants[0])
	}
	if final.phase != phaseIdle {
		t.Fatalf("turn must end idle after a replayed round, phase=%v", final.phase)
	}
}

// TestFailureStreakDecaysOnRealProgress is the counterweight to
// TestFailureStreakSurvivesBatchedSuccess. A read can't fix anything, so it must
// not clear the streak: but every other success is progress and must. The
// canonical healthy loop is a succeeding edit_file batched with a `go test` that
// still fails, on a different error each round. Letting that build a streak
// hands a converging model a note saying "stop repeating it ... or tell the user
// what's blocking you" five errors into ten, which ctx.Pack then demotes to the
// USER role on the wire: so a 30B reads its own user telling it to give up.
func TestFailureStreakDecaysOnRealProgress(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	for i := range maxToolFailStreak + 3 {
		m.lastToolKey = fmt.Sprintf("edit_file|src/file%d.go", i)
		m.recordToolOutcome(tools.EditFileName, "edited src/file.go")
		m.lastToolKey = "bash|go test ./..."
		m.recordToolOutcome(tools.BashName, fmt.Sprintf("undefined: thing%d\n(exit: exit status 1)", i))
		if m.failStreak > 1 {
			t.Fatalf("round %d: a succeeding edit must reset the streak, got %d", i, m.failStreak)
		}
	}
	m.maybeFailureNudge()
	if n := countSystem(m.history); n != 0 {
		t.Fatalf("a converging build loop must not be told to stop, got %d system notes:\n%+v", n, m.history)
	}
}

func TestCommandRegistry(t *testing.T) {
	if len(commands) != 2 || commandByName("/clear") == nil || commandByName("/models") == nil {
		t.Fatal("expected exactly /clear and /models")
	}
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	out, cmd := m.runSlash("/unknown-command")
	if cmd != nil || !strings.Contains(stripANSI(out.(Model).scroll.String()), "unknown command") {
		t.Fatal("unknown commands must show help without starting an action")
	}
}

func TestProbeSuccessUsesConfiguredContext(t *testing.T) {
	m := newTestModel(t, func(http.ResponseWriter, *http.Request) {})
	m.cfg.ActiveProfile().ContextSize = 65536
	m.connected = false
	out, _ := m.handleProbe(probeMsg{profile: m.cfg.Active, cli: m.cli})
	final := out.(Model)
	if !final.connected || final.activeContextSize() != 65536 || !strings.Contains(stripANSI(final.scroll.String()), "✓ active: local") {
		t.Fatal("successful activation must retain the configured context and print confirmation")
	}
}
