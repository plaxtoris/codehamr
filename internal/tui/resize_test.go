package tui

import (
	"reflect"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/codehamr/codehamr/internal/config"
	"github.com/codehamr/codehamr/internal/llm"
)

// TestResizeKeepsPromptInsideTerminal: across a resize sequence View must
// never claim more rows than the terminal, else the textarea pushes the
// status bar off screen or wraps mid prompt.
func TestResizeKeepsPromptInsideTerminal(t *testing.T) {
	cfg, _, _ := config.Bootstrap(t.TempDir())
	m := New(cfg, llm.New("http://x", cfg.ActiveProfile().LLM, ""), t.TempDir(), "test")
	var mm tea.Model = m

	mx := m
	mx.ta.ta.SetValue(strings.Repeat("abc def ghi jkl mno pqr stu vwx yz ", 20))
	mm = mx

	sizes := [][2]int{
		{120, 40}, {60, 20}, {40, 15}, {20, 10},
		{21, 10}, {20, 10}, {21, 10}, {20, 10},
		{20, 8}, {30, 6}, {120, 40},
	}
	for _, s := range sizes {
		w, h := s[0], s[1]
		mm2, _ := mm.Update(tea.WindowSizeMsg{Width: w, Height: h})
		mm = mm2
		got := strings.Count(mm.View(), "\n") + 1
		if got > h {
			t.Errorf("size %dx%d: View has %d rows, must not exceed %d", w, h, got, h)
		}
	}
}

// TestFirstResizeDoesNotClearScreen: the first WindowSizeMsg must not
// ClearScreen; the terminal still holds the user's pre launch shell
// output; wiping it would feel destructive. Only the splash is printed.
func TestFirstResizeDoesNotClearScreen(t *testing.T) {
	cfg, _, _ := config.Bootstrap(t.TempDir())
	m := New(cfg, llm.New("http://x", cfg.ActiveProfile().LLM, ""), t.TempDir(), "test")

	_, cmd := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	if cmdYieldsClearScreen(cmd) {
		t.Error("first WindowSizeMsg must not request ClearScreen")
	}
}

// TestWidthChangeSuppressesView: any width change (narrow OR widen) flips
// suppressView so nothing commits between SIGWINCH and the settle. View()
// returns "" while suppressed; bubbletea expands that to one blank row +
// EraseScreenBelow, leaving nothing for soft wrap reflow to orphan. The
// streaming buffer survives (re wrapped on resume).
func TestWidthChangeSuppressesView(t *testing.T) {
	cfg, _, _ := config.Bootstrap(t.TempDir())
	m := New(cfg, llm.New("http://x", cfg.ActiveProfile().LLM, ""), t.TempDir(), "test")
	var mm tea.Model = m

	mm, _ = mm.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	mx := mm.(Model)
	mx.streaming.WriteString("partial reply line one\nline two\nline three\n")
	mx.phase = phaseStreaming
	mm = mx

	for _, next := range []int{60, 100} { // narrow then widen, both must suppress
		mm2, cmd := mm.Update(tea.WindowSizeMsg{Width: next, Height: 24})
		nm := mm2.(Model)

		if !nm.suppressView {
			t.Errorf("width change to %d must enable suppressView", next)
		}
		if got := nm.View(); got != "" {
			t.Errorf("View() must return \"\" while suppressed (width=%d), got %q", next, got)
		}
		if nm.streaming.Len() == 0 {
			t.Errorf("streaming buffer must survive width change to %d", next)
		}
		if cmd == nil {
			t.Errorf("width change to %d must schedule a settle tick, got nil cmd", next)
		}
		mm = nm
	}
}

// TestResizeSettleReplaysScrollbackAtNewWidth: a matching settle returns a
// tea.Sequence that wipes viewport + scrollback and reemits splash and the
// full m.scroll transcript at the new width, so no previous width rows
// soft wrap into stair steps.
func TestResizeSettleReplaysScrollbackAtNewWidth(t *testing.T) {
	cfg, _, _ := config.Bootstrap(t.TempDir())
	m := New(cfg, llm.New("http://x", cfg.ActiveProfile().LLM, ""), t.TempDir(), "test")
	var mm tea.Model = m

	mm, _ = mm.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	// Seed m.scroll so the settle has something to replay.
	mx := mm.(Model)
	mx.scroll.WriteString("▌ explain bitcoin briefly\nBitcoin is a digital currency.\n")
	mm = mx

	mm, _ = mm.Update(tea.WindowSizeMsg{Width: 50, Height: 20})
	nm := mm.(Model)
	if !nm.suppressView {
		t.Fatal("setup precondition: suppressView must be true after width change")
	}

	mm2, settle := nm.Update(resizeSettleMsg{gen: nm.resizeGen})
	nm2 := mm2.(Model)

	if nm2.suppressView {
		t.Error("matching settle must flip suppressView back to false")
	}
	if !cmdYieldsClearScreen(settle) {
		t.Error("settle must include tea.ClearScreen")
	}
	if !cmdYieldsScrollbackErase(settle) {
		t.Error("settle must include eraseScrollback (\\x1b[3J)")
	}
	if !cmdYieldsPrintln(settle) {
		t.Error("settle must include at least one tea.Println: the splash and/or replayed scroll")
	}
	// Two Println leaves: splash + transcript replay (no outbox queued here).
	if n := countPrintlnLeaves(settle); n != 2 {
		t.Errorf("expected 2 tea.Println leaves (splash + scroll replay), got %d", n)
	}
}

// TestStaleResizeSettleIsDiscarded: a settle whose gen no longer matches
// m.resizeGen (a newer resize bumped it after scheduling) must be a no operation,
// else the chrome flickers back mid drag on each stale tick.
func TestStaleResizeSettleIsDiscarded(t *testing.T) {
	cfg, _, _ := config.Bootstrap(t.TempDir())
	m := New(cfg, llm.New("http://x", cfg.ActiveProfile().LLM, ""), t.TempDir(), "test")
	var mm tea.Model = m

	mm, _ = mm.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	mm, _ = mm.Update(tea.WindowSizeMsg{Width: 80, Height: 30})
	mm, _ = mm.Update(tea.WindowSizeMsg{Width: 60, Height: 30})
	nm := mm.(Model)

	if nm.resizeGen < 2 {
		t.Fatalf("setup precondition: resizeGen must be >= 2 after two narrowings, got %d", nm.resizeGen)
	}

	stale := resizeSettleMsg{gen: nm.resizeGen - 1}
	mm2, cmd := nm.Update(stale)
	nm2 := mm2.(Model)

	if !nm2.suppressView {
		t.Error("stale settle must NOT flip suppressView off")
	}
	if cmd != nil {
		t.Error("stale settle must return nil cmd (no clear, no further work)")
	}
}

// TestRedundantResizeIsNoOp: a WindowSizeMsg with unchanged dimensions
// (some terminals send these on focus) must not fire the hardening path;
// clearing the screen for a non event flickers for no benefit.
func TestRedundantResizeIsNoOp(t *testing.T) {
	cfg, _, _ := config.Bootstrap(t.TempDir())
	m := New(cfg, llm.New("http://x", cfg.ActiveProfile().LLM, ""), t.TempDir(), "test")
	var mm tea.Model = m

	mm, _ = mm.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	_, cmd := mm.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	if cmdYieldsClearScreen(cmd) {
		t.Error("same-size resize must not request ClearScreen")
	}
}

// TestWidenResizeAlsoSuppresses: widening changes the splash layout
// (text→art at the wordmark threshold) and leaves narrow rows misplaced,
// so it takes the same suppress + settle replay path as narrowing. The
// immediate cmd is the debounce tick; the clear lands on settle.
func TestWidenResizeAlsoSuppresses(t *testing.T) {
	cfg, _, _ := config.Bootstrap(t.TempDir())
	m := New(cfg, llm.New("http://x", cfg.ActiveProfile().LLM, ""), t.TempDir(), "test")
	var mm tea.Model = m

	mm, _ = mm.Update(tea.WindowSizeMsg{Width: 60, Height: 24})
	mm, cmd := mm.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	nm := mm.(Model)

	if !nm.suppressView {
		t.Error("widening must enable suppressView so the eventual settle cleanly replays the new layout")
	}
	if cmd == nil {
		t.Error("widening must schedule a settle tick")
	}
}

// TestHeightOnlyResizeDoesNotClear: a height only change can't induce the
// soft wrap that breaks bubbletea's cursor math (no line widens), so the
// hardening path stays reserved for width changes.
func TestHeightOnlyResizeDoesNotClear(t *testing.T) {
	cfg, _, _ := config.Bootstrap(t.TempDir())
	m := New(cfg, llm.New("http://x", cfg.ActiveProfile().LLM, ""), t.TempDir(), "test")
	var mm tea.Model = m

	mm, _ = mm.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	_, cmd := mm.Update(tea.WindowSizeMsg{Width: 80, Height: 10})
	if cmdYieldsClearScreen(cmd) {
		t.Error("height-only resize must not request ClearScreen")
	}
}

// countCmdLeaves runs cmd, recurses into the slice shaped []tea.Cmd payload
// of tea.BatchMsg / tea.sequenceMsg (both unexported), and counts leaves
// where match holds, asserting what a Sequence emits without importing
// bubbletea's internal types.
func countCmdLeaves(cmd tea.Cmd, match func(tea.Cmd, tea.Msg) bool) int {
	if cmd == nil {
		return 0
	}
	msg := cmd()
	if match(cmd, msg) {
		return 1
	}
	rv := reflect.ValueOf(msg)
	if rv.Kind() != reflect.Slice {
		return 0
	}
	n := 0
	for i := 0; i < rv.Len(); i++ {
		c, ok := rv.Index(i).Interface().(tea.Cmd)
		if !ok {
			continue
		}
		n += countCmdLeaves(c, match)
	}
	return n
}

func cmdYieldsClearScreen(cmd tea.Cmd) bool {
	target := reflect.TypeOf(tea.ClearScreen())
	return countCmdLeaves(cmd, func(_ tea.Cmd, msg tea.Msg) bool {
		return reflect.TypeOf(msg) == target
	}) > 0
}

func cmdYieldsScrollbackErase(cmd tea.Cmd) bool {
	needlePtr := reflect.ValueOf(eraseScrollback).Pointer()
	return countCmdLeaves(cmd, func(c tea.Cmd, _ tea.Msg) bool {
		return reflect.ValueOf(c).Pointer() == needlePtr
	}) > 0
}

func cmdYieldsPrintln(cmd tea.Cmd) bool {
	return countPrintlnLeaves(cmd) > 0
}

// printlnMsgType is captured from tea.Println itself, not matched by its
// unexported type *name*: a name string match would silently degrade to a
// no operation (passing every assertion while checking nothing) the day Charm
// renames that internal type. Capturing from the constructor can't drift.
var printlnMsgType = reflect.TypeOf(tea.Println("probe")())

func countPrintlnLeaves(cmd tea.Cmd) int {
	return countCmdLeaves(cmd, func(_ tea.Cmd, msg tea.Msg) bool {
		return msg != nil && reflect.TypeOf(msg) == printlnMsgType
	})
}
