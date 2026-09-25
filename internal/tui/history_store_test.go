package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPromptHistoryRoundTrip: values with newlines/quotes/unicode survive a
// disk round trip in append order, the on disk format must carry any byte the
// textarea can submit.
func TestPromptHistoryRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if got := loadPromptHistory(dir); len(got) != 0 {
		t.Fatalf("fresh dir should have 0 entries, got %d", len(got))
	}
	want := []string{"first", "second\nwith newline", `third "quoted"`, "café 🐹"}
	for _, v := range want {
		if err := appendPromptHistory(dir, v); err != nil {
			t.Fatal(err)
		}
	}
	got := loadPromptHistory(dir)
	if len(got) != len(want) {
		t.Fatalf("got %d entries, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].display != w {
			t.Errorf("entry %d: got %q, want %q", i, got[i].display, w)
		}
	}
}

// TestPromptHistoryCap: exceeding historyMaxEntries drops the oldest, not the
// newest, otherwise the file grows unbounded.
func TestPromptHistoryCap(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < historyMaxEntries+50; i++ {
		if err := appendPromptHistory(dir, "p"+strings.Repeat("x", i%3)); err != nil {
			t.Fatal(err)
		}
	}
	got := loadPromptHistory(dir)
	if len(got) != historyMaxEntries {
		t.Fatalf("cap not enforced: %d entries on disk, want %d", len(got), historyMaxEntries)
	}
	// Head entry = the 50th submit (oldest 50 dropped): trim is from the head.
	wantHead := "p" + strings.Repeat("x", 50%3)
	if got[0].display != wantHead {
		t.Errorf("trim direction wrong: head=%q want %q", got[0].display, wantHead)
	}
}

// TestPromptHistorySkipEmpty: empty submits never reach the file, so a stray ↵
// can't pollute recall with blanks.
func TestPromptHistorySkipEmpty(t *testing.T) {
	dir := t.TempDir()
	if err := appendPromptHistory(dir, ""); err != nil {
		t.Fatal(err)
	}
	if got := loadPromptHistory(dir); len(got) != 0 {
		t.Errorf("empty prompt should not be saved, got %d", len(got))
	}
	if _, err := os.Stat(filepath.Join(dir, historyFileName)); !os.IsNotExist(err) {
		t.Errorf("history file should not exist after only-empty append, err=%v", err)
	}
}

// TestPromptHistoryClear: clearPromptHistory removes the file; a missing file
// is not an error. Mirrors /clear semantics.
func TestPromptHistoryClear(t *testing.T) {
	dir := t.TempDir()
	if err := appendPromptHistory(dir, "x"); err != nil {
		t.Fatal(err)
	}
	if err := clearPromptHistory(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, historyFileName)); !os.IsNotExist(err) {
		t.Fatalf("history file should be gone, err=%v", err)
	}
	// Idempotent: clearing an already clean dir is a no operation.
	if err := clearPromptHistory(dir); err != nil {
		t.Errorf("second clear must be no-op, got %v", err)
	}
}

// TestPromptHistoryCorruptLineSkipped: a malformed line (users may hand edit
// the file) must not poison the valid entries around it.
func TestPromptHistoryCorruptLineSkipped(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, historyFileName)
	body := "not a quoted line\n" + `"valid"` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	got := loadPromptHistory(dir)
	if len(got) != 1 || got[0].display != "valid" {
		t.Errorf("expected 1 valid entry, got %+v", got)
	}
}

// TestPromptHistoryConcurrentAppendsKeepBoth guards the load then rewrite race:
// two instances sharing a project dir would each read N entries and overwrite
// the whole file, dropping the other's submit. O_APPEND writes only the new
// line, so both survive.
func TestPromptHistoryConcurrentAppendsKeepBoth(t *testing.T) {
	dir := t.TempDir()

	const n = 50
	done := make(chan struct{}, 2)
	go func() {
		for i := 0; i < n; i++ {
			_ = appendPromptHistory(dir, fmt.Sprintf("alpha-%d", i))
		}
		done <- struct{}{}
	}()
	go func() {
		for i := 0; i < n; i++ {
			_ = appendPromptHistory(dir, fmt.Sprintf("beta-%d", i))
		}
		done <- struct{}{}
	}()
	<-done
	<-done

	got := loadPromptHistory(dir)
	if len(got) != 2*n {
		t.Fatalf("concurrent appends lost entries: got %d/%d", len(got), 2*n)
	}
	seen := map[string]bool{}
	for _, e := range got {
		seen[e.display] = true
	}
	for i := 0; i < n; i++ {
		if !seen[fmt.Sprintf("alpha-%d", i)] {
			t.Fatalf("alpha-%d missing from history", i)
		}
		if !seen[fmt.Sprintf("beta-%d", i)] {
			t.Fatalf("beta-%d missing from history", i)
		}
	}
}

// TestPromptHistoryQuotedLineStaysLoadable guards the quote expansion gap:
// strconv.Quote expands each control/invalid byte to \xNN (4× growth), so a
// value gated on its *unquoted* length can still write an on disk line past
// loadPromptHistory's scanner buffer. bufio's ErrTooLong then halts the scan,
// so every *newer* entry vanishes from recall too. The append guard must
// decline any line the loader can't read back.
func TestPromptHistoryQuotedLineStaysLoadable(t *testing.T) {
	dir := t.TempDir()
	// Clears the unquoted gate (len == cap) but quotes to ~4× the cap, past the
	// scanner ceiling. Pre fix this reached disk.
	pathological := strings.Repeat("\x01", historyMaxEntryBytes)
	if err := appendPromptHistory(dir, pathological); err != nil {
		t.Fatal(err)
	}
	// A later normal prompt must survive recall, not become collateral damage
	// of an unreadable line earlier in the file.
	if err := appendPromptHistory(dir, "survivor"); err != nil {
		t.Fatal(err)
	}
	got := loadPromptHistory(dir)
	for _, e := range got {
		if e.display == "survivor" {
			return // invariant held: later entries stay loadable
		}
	}
	t.Fatalf("a later entry was lost: an oversized quoted line halted the load scan; got %d entries", len(got))
}

// TestPromptHistoryRejectsHugeEntry: a multi MiB paste isn't stored, the load
// scanner would silently drop it anyway, so declining to write is consistent.
// Anything sane (a code paragraph, a stack trace) still survives.
func TestPromptHistoryRejectsHugeEntry(t *testing.T) {
	dir := t.TempDir()
	huge := strings.Repeat("x", historyMaxEntryBytes+1)
	if err := appendPromptHistory(dir, huge); err != nil {
		t.Fatal(err)
	}
	got := loadPromptHistory(dir)
	if len(got) != 0 {
		t.Fatalf("oversized entry should not be saved, got %d", len(got))
	}
	// At the cap entry still saves.
	atCap := strings.Repeat("y", historyMaxEntryBytes)
	if err := appendPromptHistory(dir, atCap); err != nil {
		t.Fatal(err)
	}
	got = loadPromptHistory(dir)
	if len(got) != 1 || got[0].display != atCap {
		t.Fatalf("at-cap entry should round-trip, got %d entries", len(got))
	}
}
