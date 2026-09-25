//go:build unix

package tools

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestBashKillsBackgroundedChildOnCancel proves bash's process group kill
// reaches backgrounded grandchildren. A naked `cmd &` outlives /bin/sh; without
// setProcessGroup's Setpgid + negative PID SIGKILL on cancel, it leaks and runs
// to completion after Ctrl+C. Polls a deadline rather than sleeping the full
// 30s, so it stays fast and deterministic.
func TestBashKillsBackgroundedChildOnCancel(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")

	parent, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		// Record the backgrounded sleep's PID, then `wait` so /bin/sh stays alive
		// holding the group together until cancel kills it.
		Bash(parent, "sleep 30 & echo $! > "+pidFile+"; wait", 60*time.Second)
		close(done)
	}()

	// Wait for the PID file to materialise (the child has started).
	pid := waitForPID(t, pidFile)

	// Sanity: child alive before cancel; signal 0 just probes.
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("backgrounded child %d should be alive before cancel: %v", pid, err)
	}

	// Ctrl+C: setProcessGroup's Cancel hook SIGKILLs the whole group, taking the
	// backgrounded sleep with it.
	cancel()

	// Bash should return promptly once the group is killed.
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Bash did not return within 10s after cancel: group kill failed")
	}

	// Poll until the child is gone; the kernel may take a beat to tear the
	// group down.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err == syscall.ESRCH {
			return // child is gone, the group kill worked
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("backgrounded child %d survived parent cancel: process group was not killed", pid)
}

// waitForPID blocks until pidFile contains a parseable PID, then returns it.
// Fails the test if the file never appears within a short deadline.
func waitForPID(t *testing.T, pidFile string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(pidFile)
		if err == nil {
			if pid, perr := strconv.Atoi(strings.TrimSpace(string(raw))); perr == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("child PID file %s never appeared", pidFile)
	return 0
}
