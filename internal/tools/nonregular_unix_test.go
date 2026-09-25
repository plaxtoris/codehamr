//go:build unix

package tools

import (
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// timeoutC bounds the hang these tests exist to catch, well under go test's
// own package timeout so a regression fails fast with a named cause.
func timeoutC(t *testing.T) <-chan time.Time {
	t.Helper()
	return time.After(5 * time.Second)
}

// mkfifo creates a FIFO in a temp dir; opening it for read OR write would
// block forever with no peer, which is exactly what the guards must prevent.
func mkfifo(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pipe")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	return path
}

// TestReadFileRefusesNonRegular: read_file on a FIFO would block forever in
// open(2) waiting for a writer, leaking the tool goroutine past Ctrl+C (which
// cancels the turn but cannot unblock the read); an endless device file would
// grow the buffer without bound. The Stat guard must refuse without opening.
// If the guard is missing, this test hangs rather than fails: the timeout is
// the assertion.
func TestReadFileRefusesNonRegular(t *testing.T) {
	path := mkfifo(t)
	done := make(chan string, 1)
	go func() { done <- ReadFile(path, 0, 0) }()
	select {
	case got := <-done:
		if !strings.Contains(got, "not a regular file") {
			t.Fatalf("want a not-a-regular-file refusal, got %q", got)
		}
	case <-timeoutC(t):
		t.Fatal("ReadFile blocked on the FIFO instead of refusing")
	}
}

// TestWriteFileRefusesNonRegular: write_file to an existing FIFO with no
// reader blocks identically in open(2) O_WRONLY; same guard, same contract.
func TestWriteFileRefusesNonRegular(t *testing.T) {
	path := mkfifo(t)
	done := make(chan string, 1)
	go func() { done <- WriteFile(path, "data", false) }()
	select {
	case got := <-done:
		if !strings.Contains(got, "not a regular file") {
			t.Fatalf("want a not-a-regular-file refusal, got %q", got)
		}
	case <-timeoutC(t):
		t.Fatal("WriteFile blocked on the FIFO instead of refusing")
	}
}

// TestEditFileRefusesNonRegular: edit_file reads the target first, so it needs
// the same guard as read_file.
func TestEditFileRefusesNonRegular(t *testing.T) {
	path := mkfifo(t)
	done := make(chan string, 1)
	go func() { done <- EditFile(path, "a", "b") }()
	select {
	case got := <-done:
		if !strings.Contains(got, "not a regular file") {
			t.Fatalf("want a not-a-regular-file refusal, got %q", got)
		}
	case <-timeoutC(t):
		t.Fatal("EditFile blocked on the FIFO instead of refusing")
	}
}
