package tools

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	chmctx "github.com/codehamr/codehamr/internal/ctx"
)

func TestBashEchoesStdout(t *testing.T) {
	out := Bash(context.Background(), "echo hammer && echo time >&2", 5*time.Second)
	if !strings.Contains(out, "hammer") || !strings.Contains(out, "time") {
		t.Fatalf("combined output missing: %q", out)
	}
}

func TestBashNonZeroExitNotFatal(t *testing.T) {
	out := Bash(context.Background(), "false", 5*time.Second)
	if !strings.Contains(out, "exit") {
		t.Fatalf("expected exit marker, got %q", out)
	}
}

// TestBashExactExitMarkerFormat pins the exact non zero exit shape: output,
// then a single "\n(exit: ...)" marker. The model parses these, and the other
// tests only Contains("exit"); a dropped "\n" or doubled marker would pass
// silently. Also proves real output survives alongside the exit (the `false`
// test emits nothing).
func TestBashExactExitMarkerFormat(t *testing.T) {
	out := Bash(context.Background(), "echo out; exit 3", 5*time.Second)
	want := "out\n\n(exit: exit status 3)"
	if out != want {
		t.Fatalf("exit marker format wrong:\n got %q\nwant %q", out, want)
	}
}

func TestBashEmptyCommand(t *testing.T) {
	if Bash(context.Background(), " ", time.Second) != "(empty command)" {
		t.Fatal("empty command handling wrong")
	}
}

// TestBashBoundsRunawayOutput: a firehose command must not grow an unbounded
// buffer (the old CombinedOutput OOM killed the whole TUI on `cat big.iso`
// well before the timeout could react). The capture keeps head+tail with an
// OMITTED marker between, mirroring ctx.Truncate's framing, and preserves the
// very first and very last bytes so the model still sees both ends.
func TestBashBoundsRunawayOutput(t *testing.T) {
	// ~3x the tail cap so the ring provably wraps.
	n := 3 * bashOutputTail
	out := Bash(context.Background(),
		"printf 'START'; head -c "+strconv.Itoa(n)+" /dev/zero | tr '\\0' 'a'; printf 'END'",
		30*time.Second)
	if len(out) > bashOutputHead+bashOutputTail+300 {
		t.Fatalf("output not bounded: %d bytes", len(out))
	}
	if !strings.HasPrefix(out, "START") {
		t.Fatalf("head lost, output starts %q", out[:16])
	}
	if !strings.Contains(out, "END\n(output capped at capture:") {
		t.Fatalf("tail or capture note lost, output ends %q", out[len(out)-80:])
	}
	if !strings.Contains(out, "OMITTED") {
		t.Fatal("dropped middle must carry the seam marker")
	}
}

// TestHeadTailBufferSmallOutputUntouched: output under the head cap must come
// back byte identical: the bounding must be invisible in the normal case.
func TestHeadTailBufferSmallOutputUntouched(t *testing.T) {
	var b headTailBuffer
	b.Write([]byte("hello "))
	b.Write([]byte("world"))
	if got := b.String(); got != "hello world" {
		t.Fatalf("small output mangled: %q", got)
	}
}

// TestHeadTailBufferKeepsExactHeadAndTail: once the middle is dropped, the
// reassembly must carry exactly the first head cap bytes and the last tail cap
// bytes in order, across chunked writes that wrap the ring several times.
func TestHeadTailBufferKeepsExactHeadAndTail(t *testing.T) {
	var b headTailBuffer
	total := bashOutputHead + 5*bashOutputTail
	// Deterministic byte stream in awkward chunk sizes to exercise ring wraps.
	buf := make([]byte, 0, total)
	for i := 0; len(buf) < total; i++ {
		buf = append(buf, byte('a'+i%26))
	}
	for i := 0; i < total; i += 3333 {
		b.Write(buf[i:min(i+3333, total)])
	}
	got := b.String()
	if !strings.HasPrefix(got, string(buf[:bashOutputHead])) {
		t.Fatal("head bytes wrong")
	}
	if !strings.HasSuffix(got, string(buf[total-bashOutputTail:])) {
		t.Fatal("tail bytes wrong after ring wraps")
	}
}

// TestBashHonorsAlreadyCancelledParent: a pre cancelled parent (Ctrl+C raced
// the dispatch) must report "(cancelled)", not "(empty command)" or, worse,
// spawn a fresh /bin/sh. The cancel must win even on the empty cmd fast path.
func TestBashHonorsAlreadyCancelledParent(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	cancel()
	if got := Bash(parent, "", time.Second); got != "(cancelled)" {
		t.Fatalf("pre-cancelled bash returned %q, want (cancelled)", got)
	}
	if got := Bash(parent, "echo nope", time.Second); got != "(cancelled)" {
		t.Fatalf("pre-cancelled bash with command returned %q, want (cancelled)", got)
	}
}

func TestBashTimeout(t *testing.T) {
	out := Bash(context.Background(), "sleep 2", 100*time.Millisecond)
	if !strings.Contains(out, "timeout") {
		t.Fatalf("expected timeout marker: %q", out)
	}
}

func TestBashCustomTimeoutHonored(t *testing.T) {
	// timeout_seconds of 1 truncates the 3s sleep, and flows through to Bash.
	start := time.Now()
	call := chmctx.ToolCall{
		ID: "t1", Name: "bash",
		Arguments: map[string]any{
			"cmd":             "sleep 3",
			"timeout_seconds": float64(1),
		},
	}
	msg := Execute(context.Background(), call)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("custom timeout ignored; elapsed %s", elapsed)
	}
	if !strings.Contains(msg.Content, "timeout") {
		t.Fatalf("expected timeout marker: %q", msg.Content)
	}
}

func TestBashTimeoutCappedAtOneHour(t *testing.T) {
	// 999999s must clamp to 3600. Can't sleep an hour to prove it, so a short
	// command just has to complete quickly: no overflow, no panic.
	call := chmctx.ToolCall{
		ID: "t2", Name: "bash",
		Arguments: map[string]any{
			"cmd":             "echo clamped",
			"timeout_seconds": float64(999999),
		},
	}
	msg := Execute(context.Background(), call)
	if !strings.Contains(msg.Content, "clamped") {
		t.Fatalf("expected echo output: %q", msg.Content)
	}
}

// TestBashTimeoutOverflowClamped: extreme floats must be clamped BEFORE the
// Duration multiply. `time.Duration(1e18) * time.Second` overflows int64 to a
// negative deadline, firing context.WithTimeout instantly: the command would
// "succeed" without running. Clamp up front to avoid it.
func TestBashTimeoutOverflowClamped(t *testing.T) {
	call := chmctx.ToolCall{
		ID: "t3", Name: "bash",
		Arguments: map[string]any{
			"cmd":             "echo ok",
			"timeout_seconds": float64(1e18),
		},
	}
	msg := Execute(context.Background(), call)
	if !strings.Contains(msg.Content, "ok") {
		t.Fatalf("overflow clamp: expected echo output, got %q", msg.Content)
	}
}

// TestBashParentCancelMidRun: parent cancel mid sleep returns "(cancelled)",
// not a misleading "(timeout after Xs)" or stale exit code. Parent cancel
// wins when it fires before the deadline: it's the user's signal (a latched
// DeadlineExceeded still labels as timeout, since the timeout killed first).
func TestBashParentCancelMidRun(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	out := Bash(parent, "sleep 5", 10*time.Second)
	elapsed := time.Since(start)
	if elapsed > 2*time.Second {
		t.Fatalf("parent cancel didn't propagate; elapsed %s", elapsed)
	}
	if !strings.Contains(out, "cancelled") {
		t.Fatalf("expected (cancelled) marker, got %q", out)
	}
	if strings.Contains(out, "timeout") {
		t.Fatalf("parent-cancel must not be reported as timeout: %q", out)
	}
}

func TestBashBackgroundedChildDoesNotBlock(t *testing.T) {
	// A naked `cmd &` leaves the child holding stdout/stderr pipes. We must not
	// block on them after /bin/sh exits, which caused multi minute stalls
	// before WaitDelay was set.
	start := time.Now()
	Bash(context.Background(), "sleep 3 &", 5*time.Second)
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("bash blocked for %s on backgrounded child's pipes; want <500ms", elapsed)
	}
}

// TestBashBackgroundedChildReturnsCleanOutput: backgrounding a child that
// holds the pipes open trips exec.ErrWaitDelay (shell already exited 0), not
// an exit error. Result must carry real output and no spurious "(exit: ...)"
// marker, else every `cmd &` / `server &` looks like a failure to the model.
func TestBashBackgroundedChildReturnsCleanOutput(t *testing.T) {
	out := Bash(context.Background(), "echo done && sleep 3 &", 10*time.Second)
	if strings.Contains(out, "(exit:") {
		t.Fatalf("backgrounded-child success mislabeled as failure: %q", out)
	}
	if !strings.Contains(out, "done") {
		t.Fatalf("expected backgrounded command output, got %q", out)
	}
}

func TestExecuteBashWrapsResult(t *testing.T) {
	call := chmctx.ToolCall{
		ID: "call_1", Name: "bash",
		Arguments: map[string]any{"cmd": "echo hi"},
	}
	msg := Execute(context.Background(), call)
	if msg.Role != chmctx.RoleTool || msg.ToolCallID != "call_1" || msg.ToolName != "bash" {
		t.Fatalf("bad message: %+v", msg)
	}
	if !strings.Contains(msg.Content, "hi") {
		t.Fatalf("content missing: %q", msg.Content)
	}
}

func TestExecuteUnknownTool(t *testing.T) {
	call := chmctx.ToolCall{ID: "x", Name: "nope"}
	msg := Execute(context.Background(), call)
	if !strings.Contains(msg.Content, "unknown tool") {
		t.Fatalf("expected unknown-tool error: %q", msg.Content)
	}
}

func TestInlineStatusBash(t *testing.T) {
	s := InlineStatus(chmctx.ToolCall{Name: "bash",
		Arguments: map[string]any{"cmd": "ls -la\nrm /tmp/x"}})
	if !strings.HasPrefix(s, "▶ bash: ls -la") || strings.Contains(s, "\n") {
		t.Fatalf("bad inline status: %q", s)
	}
}

func TestInlineStatusGeneric(t *testing.T) {
	s := InlineStatus(chmctx.ToolCall{Name: "context7",
		Arguments: map[string]any{"query": "react useEffect"}})
	if !strings.HasPrefix(s, "▶ context7: react") {
		t.Fatalf("bad inline status: %q", s)
	}
}

// TestInlineStatusRuneBoundaryTruncate: truncating a long non ASCII command at
// a fixed byte offset can split a multi byte rune, leaving an orphan
// continuation byte the TUI prints as invalid UTF 8. The cut must snap back to
// a rune boundary, trading a couple chars off the byte budget for valid UTF 8.
func TestInlineStatusRuneBoundaryTruncate(t *testing.T) {
	cmd := strings.Repeat("ä", 100) + " end" // 200+ bytes of 2 byte runes
	s := InlineStatus(chmctx.ToolCall{
		Name:      "bash",
		Arguments: map[string]any{"cmd": cmd},
	})
	if !utf8.ValidString(s) {
		t.Fatalf("InlineStatus produced invalid UTF-8 (cut mid-rune): %q", s)
	}
}

// TestRunRawSurfacesTruncatedToolArgs: the _parse_error sentinel (set by
// llm.resolve when the server truncates an oversized tool call mid JSON) must
// surface as an actionable message naming the cause + chunked recovery, not
// fall through the type assertions to a misleading "(empty path)" / "(empty
// command)". Checked for a file tool and bash to pin that the guard is generic,
// sitting before the per tool switch.
func TestRunRawSurfacesTruncatedToolArgs(t *testing.T) {
	for _, name := range []string{WriteFileName, BashName} {
		call := chmctx.ToolCall{
			Name:      name,
			Arguments: map[string]any{"_parse_error": "unexpected end of JSON input"},
		}
		got := runRaw(context.Background(), call)
		if strings.Contains(got, "empty path") || strings.Contains(got, "empty command") {
			t.Fatalf("%s: truncation masked as empty-arg error: %q", name, got)
		}
		for _, want := range []string{"not valid JSON", "truncated", "append true", "wc -c"} {
			if !strings.Contains(got, want) {
				t.Fatalf("%s: message missing %q: %q", name, want, got)
			}
		}
	}
}

// TestExecuteSpillsOversizeOutput: when Truncate drops the middle of a big
// result, the full bytes must land in a file the model can grep. Without it
// the only way back to the dropped middle is rerunning the command that
// produced it, which is the round trip this exists to remove.
func TestExecuteSpillsOversizeOutput(t *testing.T) {
	msg := Execute(context.Background(), chmctx.ToolCall{
		Name:      BashName,
		Arguments: map[string]any{"cmd": "seq 1 200000"},
	})
	if !strings.Contains(msg.Content, "───── truncated") {
		t.Fatalf("expected a truncated result, got %d bytes", len(msg.Content))
	}
	i := strings.Index(msg.Content, "output saved to ")
	if i < 0 {
		t.Fatalf("truncated result did not name a spill file: %q", msg.Content[len(msg.Content)-200:])
	}
	path := msg.Content[i+len("output saved to "):]
	path = strings.TrimSpace(strings.SplitN(path, " ", 2)[0])
	t.Cleanup(func() { os.Remove(path) })
	spilled, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("spill file unreadable: %v", err)
	}
	// The dropped middle must be recoverable from the spill.
	if !strings.Contains(string(spilled), "\n100000\n") {
		t.Fatalf("spill file is missing the dropped middle (%d bytes)", len(spilled))
	}
}
