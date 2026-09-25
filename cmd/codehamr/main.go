// Command codehamr is the lightweight, fast coding agent for the terminal.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/codehamr/codehamr/internal/config"
	"github.com/codehamr/codehamr/internal/llm"
	"github.com/codehamr/codehamr/internal/tui"
	"github.com/codehamr/codehamr/internal/update"
)

// updateBudget caps the pre launch auto update (checksum fetch + download +
// rename): enough for the real ~16MB binaries on a slow (few Mbps) link. A
// too tight budget is a permanent every launch degradation, not a one off:
// the binary on disk never changes, so each start repeats the stall, the
// failure banner, and a wasted partial download. Generous is safe because an
// offline user never gets here (Check's 2s fetchTimeout fails first) and the
// wait stays Ctrl+C escapable.
const updateBudget = 90 * time.Second

// version is injected via -ldflags at build time; "dev" when running `go run`.
var version = "dev"

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "-v", "--version", "version":
			fmt.Println("codehamr", version)
			return
		case "-h", "--help", "help":
			printHelp()
			return
		}
	}

	// Wipe last session's superseded binary. Apply renames the running exe
	// to <path>.old before promoting the new one; on Windows that old file
	// stays locked until the prior process exits, so deleting it at launch
	// (not at Apply time) always wins.
	if exe, err := os.Executable(); err == nil {
		update.CleanupOld(exe)
	}

	// Pre launch auto update; all failures are non fatal and fall through to
	// the old binary.
	maybeSelfUpdate()

	cwd := mustCwd()
	// created is ignored: any first run notice printed here is wiped milliseconds
	// later by the unconditional screen+scrollback clear below, before the TUI
	// draws, so there's nothing to announce.
	cfg, _, err := config.Bootstrap(cwd)
	if err != nil {
		log.Fatalf("codehamr: %v", err)
	}
	applyEnvOverrides(cfg)

	// Opt in debug log (`logging: true`): truncates .codehamr/log.txt and
	// records every chat exchange. See tui.OpenDebugLog / dbgWrite.
	if cfg.Logging {
		tui.OpenDebugLog(cfg.Dir)
		defer tui.CloseDebugLog()
	}

	p := cfg.ActiveProfile()
	client := llm.New(cfg.ActiveURL(), p.LLM, p.ResolvedKey())

	abs, _ := filepath.Abs(cwd)
	m := tui.New(cfg, client, abs, version)

	// Hard clear before the TUI takes over: \x1b[2J viewport, \x1b[3J
	// scrollback, \x1b[H cursor home, a clean canvas free of prior shell
	// history. Inline mode safe: the session's own scrollback still
	// accumulates via tea.Println.
	os.Stdout.WriteString("\x1b[2J\x1b[3J\x1b[H")

	// Inline mode (no AltScreen, no mouse capture): only the prompt + status
	// bar render live at the bottom; everything else goes to native
	// scrollback via tea.Println, leaving scrolling/selection/copy to the
	// terminal.
	//
	// WithReportFocus types raw focus in and out sequences (\x1b[I / \x1b[O) as
	// tea.FocusMsg / tea.BlurMsg so Update can swallow them; otherwise
	// xterm.js hosts (VS Code) leak those bytes as runes into the textarea
	// on every window switch, inflating prompt height with invisible chars.
	if _, err := tea.NewProgram(m, tea.WithReportFocus()).Run(); err != nil {
		log.Fatalf("codehamr: %v", err)
	}
}

func printHelp() {
	fmt.Println(strings.TrimSpace(`
codehamr, a lightweight, fast coding agent for the terminal.

Usage:
  codehamr             start interactive TUI
  codehamr --version   print version

Slash commands (inside TUI):`))
	tui.PrintHelp(os.Stdout)
	fmt.Println(strings.TrimSpace(`
Keys (inside TUI):
  ctrl+l   clear the prompt and redraw (keeps conversation)
  ctrl+c   cancel running op · press again to quit
  ctrl+d   quit (on empty input while idle)

Config:
  .codehamr/config.yaml   project settings

Env:
  CODEHAMR_URL            override the active profile's URL at runtime
  CODEHAMR_NO_UPDATE_CHECK  set to 1 to disable GitHub update checks
  CODEHAMR_IDLE_TIMEOUT   stream idle timeout, e.g. 90m or 1h (default 1h)`))
}

// isLocalBuild reports whether the binary came from a working tree rather
// than an official release. `go run` leaves version "dev"; `make install`
// injects `git describe --tags --always --dirty`, so a dirty tree carries a
// "-dirty" suffix, a clean tree past the last tag the describe shape
// (v0.3.0-5-g5290930), and a tag less clone a bare short sha. All are local:
// only an exact release tag (v1.2.3) may self update, or the updater would
// silently swap unreleased work for the last published release (its hash
// never matches the manifest, so it always reads as "stale") on first launch.
func isLocalBuild(version string) bool {
	if version == "dev" || strings.HasSuffix(version, "-dirty") {
		return true
	}
	// describe with commits: anything carrying a "-g<hex>" suffix.
	if i := strings.LastIndex(version, "-g"); i >= 0 && isHex(version[i+2:]) {
		return true
	}
	// bare `--always` short sha (tag less clone): all hex, no tag structure.
	return len(version) >= 7 && isHex(version)
}

// isHex reports whether s is nonempty lowercase hex, the shape of a git
// abbreviated commit hash.
func isHex(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// maybeSelfUpdate runs the pre launch auto update. No operation for local builds,
// an already current hash, an unsupported platform (see update.assetName),
// or any network/filesystem refusal. On success it swaps the binary and
// re execs via reExec (which only returns on failure). Any failure past
// "update available" prints one stderr line and proceeds with the old binary.
func maybeSelfUpdate() {
	// Skip local builds: hashing a `go run` temp binary against the
	// published checksum would otherwise swap in the last release and hide
	// unreleased work behind an "update applied" banner.
	if isLocalBuild(version) {
		return
	}
	exe, err := os.Executable()
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), updateBudget)
	defer cancel()
	if !update.Check(ctx, exe) {
		return
	}
	fmt.Fprintln(os.Stderr, "◉ applying codehamr update...")
	if err := update.Apply(ctx, exe); err != nil {
		fmt.Fprintf(os.Stderr, "⚠ update failed: %v\n", err)
		if os.IsPermission(err) {
			fmt.Fprintln(os.Stderr, "  tip: rerun with sudo, or reinstall with PREFIX=$HOME/.local")
		}
		return
	}
	// Relaunch the new binary. reExec is platform split: unix execve (same
	// PID) vs. Windows spawn and wait. CODEHAMR_NO_UPDATE_CHECK=1 stops the
	// replacement run from re checking its own freshly written hash. On
	// reExec failure we fall through to the old in memory binary.
	if err := reExec(exe, os.Args, reexecEnv()); err != nil {
		fmt.Fprintf(os.Stderr, "⚠ re exec failed: %v (continuing with previous version)\n", err)
	}
}

// reexecEnv arms the update loop guard and returns the environment for the
// re exec'd child. os.Setenv overwrites in place so os.Environ() carries
// exactly one entry; append(os.Environ(), …) would leave a pre existing
// user set value first, and Unix execve resolves os.Getenv to the FIRST
// match, silently defeating the guard if someone exported
// CODEHAMR_NO_UPDATE_CHECK to a non-"1" value.
func reexecEnv() []string {
	os.Setenv("CODEHAMR_NO_UPDATE_CHECK", "1")
	return os.Environ()
}

// mustCwd returns the working directory or exits 1, called only where
// there's nothing sensible to recover to.
func mustCwd() string {
	cwd, err := os.Getwd()
	if err != nil {
		log.Fatalf("codehamr: %v", err)
	}
	return cwd
}

// applyEnvOverrides folds runtime env vars into cfg. CODEHAMR_URL overrides
// the active profile's URL (devcontainers / CI), held on a non serialised
// field so it never round trips into config.yaml on Save.
func applyEnvOverrides(cfg *config.Config) {
	if envURL := os.Getenv("CODEHAMR_URL"); envURL != "" {
		cfg.URLOverride = envURL
	}
}
