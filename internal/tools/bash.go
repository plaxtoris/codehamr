// Package tools holds the local executors (bash, read_file, write_file,
// edit_file) and the router that dispatches assistant tool calls by name.
package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	chmctx "github.com/codehamr/codehamr/internal/ctx"
)

// Wire format tool names. One source so schema, router, and inline status
// switch can't drift apart.
const (
	BashName      = "bash"
	WriteFileName = "write_file"
	EditFileName  = "edit_file"
	ReadFileName  = "read_file"
)

// maxBashTimeoutSeconds caps the per call timeout_seconds the model can
// request, a backstop against runaway loops (`sleep 99999`, `while true`)
// that would otherwise tie up the turn until Ctrl+C.
const maxBashTimeoutSeconds = 3600

// Bash runs one shell command through /bin/sh -c and returns combined
// stdout+stderr. Nonzero exit is not an error; the model sees the failure
// and reacts.
//
// A pre cancelled parent (Ctrl+C raced the dispatch) returns "(cancelled)"
// before the blank command check, so a trivially cancelled call isn't
// mistaken for a valid empty args invocation.
func Bash(parent context.Context, command string, timeout time.Duration) string {
	if parent.Err() != nil {
		return "(cancelled)"
	}
	if strings.TrimSpace(command) == "" {
		return "(empty command)"
	}
	ctxT, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctxT, "/bin/sh", "-c", command)
	// Shell gets its own process group + a Cancel that kills the whole group
	// on cancel/timeout (Unix; no operation on Windows). Without it, backgrounded
	// children (`cmd &`) outlive the parent shell and leak.
	setProcessGroup(cmd)
	// Cap the wait for stdout/stderr pipes to close after /bin/sh exits.
	// Backgrounded children inherit those pipe fds, so without this Run's
	// pipe copy goroutine blocks for the full timeout even though the shell
	// is gone.
	cmd.WaitDelay = 100 * time.Millisecond
	// Bounded combined output instead of CombinedOutput's unbounded buffer: a
	// high throughput command (`cat big.iso`, `grep -r "" /`) can emit hundreds
	// of MB/s and OOM kill the whole TUI well before the timeout or Ctrl+C
	// react. ctx.Truncate keeps only head+tail anyway, so nothing the model
	// would see is lost. Stdout and Stderr get the SAME writer value, which
	// os/exec detects and funnels through one pipe: no locking needed.
	buf := &headTailBuffer{}
	cmd.Stdout = buf
	cmd.Stderr = buf
	err := cmd.Run()
	s := buf.String()
	// Name the capture drop at the END of the output, where ctx.Truncate's
	// tail keep guarantees the model sees it: the in band seam marker sits at
	// the ~1MB offset, always inside Truncate's dropped middle, and Truncate's
	// own "total" would count the collapsed string, under reporting the real
	// size by orders of magnitude.
	if d := buf.droppedBytes(); d > 0 {
		s += fmt.Sprintf("\n(output capped at capture: %d bytes total, %d bytes dropped mid stream)", buf.totalBytes(), d)
	}
	if err != nil {
		switch {
		case ctxT.Err() == context.DeadlineExceeded:
			// Name the recovery in the result string rather than the system
			// prompt: a foreground server is the common cause and it never
			// exits on its own, so re firing it with a bigger timeout just
			// burns the turn again.
			return s + fmt.Sprintf("\n(timeout after %s: if this command never exits on its own it is a server or watcher: rerun it backgrounded, `cmd >/tmp/x.log 2>&1 & echo $! >/tmp/x.pid`, then poll the log. If it was legitimately slow, reissue it once with a larger timeout_seconds.)", timeout)
		case parent.Err() == context.Canceled || ctxT.Err() == context.Canceled:
			// User Ctrl+C; name it rather than leak "signal: killed" noise.
			return s + "\n(cancelled)"
		case errors.Is(err, exec.ErrWaitDelay):
			// Shell exited 0; err is nonnil only because a backgrounded child
			// held the pipes past WaitDelay, not a failure. Return output as is
			// so it isn't mislabeled with a spurious (exit: ...). After the
			// cancel/timeout cases so those signals win over a coincident delay.
			return s
		default:
			// Exit errors go into the output, exactly what the model needs.
			s += fmt.Sprintf("\n(exit: %v)", err)
		}
	}
	return s
}

// BashSchema is the OpenAI tool definition for bash, exposed by every profile.
func BashSchema() map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        BashName,
			"description": "Run a shell command in the user's environment. Combined stdout+stderr is returned; a nonzero exit comes back as `(exit: N)`, not an error. Use targeted commands (grep, head, tail) to keep output small. Independent commands belong in one message together with any other independent calls.",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"cmd": map[string]any{
						"type":        "string",
						"description": "The shell command to execute.",
					},
					"timeout_seconds": map[string]any{
						"type":        "integer",
						"description": "Optional per call timeout in seconds. Default 600, hard capped at 3600. Raise for commands you expect to run longer (large test suites, docker build, DB migrations). If a call comes back `(timeout after ...)` and the command was legitimately slow, reissue it once with a larger value: raising the timeout is a different call, not a repeat.",
					},
				},
				"required": []string{"cmd"},
			},
		},
	}
}

// Execute runs a tool call and returns the (possibly truncated) result ready
// to be appended to the conversation as a `tool` message.
func Execute(parent context.Context, call chmctx.ToolCall) chmctx.Message {
	raw := runRaw(parent, call)
	content := chmctx.Truncate(raw)
	// Truncate keeps head+tail and drops the middle, which is exactly where a
	// failing assertion or a stack trace sits. Spill the whole result to a file
	// and name it, so recovering the dropped middle is one grep instead of
	// rerunning the command that produced it with narrower flags.
	if len(content) < len(raw) {
		if path := spillOutput(raw); path != "" {
			content += fmt.Sprintf("\n(full %d byte output saved to %s · grep or read that file instead of rerunning the command)", len(raw), path)
		}
	}
	return chmctx.Message{
		Role:       chmctx.RoleTool,
		Content:    content,
		ToolCallID: call.ID,
		ToolName:   call.Name,
	}
}

// spillOutput writes an over budget tool result to a temp file and returns its
// path. Best effort: on failure the model keeps the truncated view it would
// have had anyway. Cleanup belongs to the OS tmp reaper: each file is already
// bounded by bash's own capture ceiling.
func spillOutput(s string) string {
	f, err := os.CreateTemp("", "codehamr-out-*.txt")
	if err != nil {
		return ""
	}
	defer f.Close()
	if _, err := f.WriteString(s); err != nil {
		os.Remove(f.Name())
		return ""
	}
	return f.Name()
}

// intArg coerces a numeric tool argument. Weak local tool call parsers emit
// integers as JSON numbers or as bare strings; both must mean the same thing,
// or read_file's continuation note ("continue with offset=451") silently
// rereads the head forever and the model loops on a file it can never finish.
func intArg(args map[string]any, key string) int {
	switch v := args[key].(type) {
	case float64:
		return int(v)
	case string:
		v = strings.TrimSpace(v)
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
		// "451.0": the same integer wearing a float's clothes. Dropping it to 0
		// hands read_file the head window again, with the same continuation note
		//: a success every time, so no failure streak can ever form.
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return int(f)
		}
	}
	return 0
}

// boolArg coerces a boolean tool argument, for the same reason as intArg. A
// string "true" silently read as false would make write_file OVERWRITE where
// append was asked for, destroying the earlier parts of a chunked write behind
// a success shaped result.
func boolArg(args map[string]any, key string) bool {
	switch v := args[key].(type) {
	case bool:
		return v
	case float64:
		// `"append": 1`. Every JSON number decodes to float64, and a model that
		// has just been told to "send the next part with append true" writing 1
		// is common enough that missing it silently truncated the file.
		return v != 0
	case string:
		return strings.EqualFold(v, "true") || v == "1"
	}
	return false
}

func runRaw(parent context.Context, call chmctx.ToolCall) string {
	// A truncated/oversized tool call leaves llm.resolve()'s _parse_error
	// sentinel where real args should be. Without this guard the call falls
	// through to an empty path/cmd and returns a misleading "(empty path)",
	// hiding that the server cut the arguments at its output token limit, the
	// failure that makes a model reemit the same too large write for minutes.
	// Name the real cause and the recovery so it self corrects in one step.
	if msg, ok := call.Arguments["_parse_error"].(string); ok {
		return fmt.Sprintf("(tool arguments were not valid JSON: %s, most likely the "+
			"content was too large and the server truncated the call at its output token "+
			"limit. Do NOT retry the same whole file write. Send it in parts of at most "+
			"~200 lines: write_file with the first part, then write_file with append true "+
			"for each following part, then verify with `wc -c path`.)", msg)
	}
	switch call.Name {
	case BashName:
		cmd, _ := call.Arguments["cmd"].(string)
		// Default 10m, overridable per call up to 1h. The old 2m default made
		// every legitimately slow command (a real test suite, a docker build,
		// an npm install) run twice: once to hit the deadline, once with a
		// bigger timeout. Clamp seconds BEFORE the Duration multiply: 1e18
		// would overflow int64 into a negative duration, and 0.5 would truncate
		// to 0 and cancel before the shell runs, so floor at 1.
		timeout := 10 * time.Minute
		if secs := intArg(call.Arguments, "timeout_seconds"); secs > 0 {
			timeout = time.Duration(min(max(secs, 1), maxBashTimeoutSeconds)) * time.Second
		}
		return Bash(parent, cmd, timeout)
	case WriteFileName:
		path, _ := call.Arguments["path"].(string)
		// A missing or nonstring content (valid JSON, so no _parse_error; schema
		// `required` is not enforced by open source backends) must not decode
		// to "" and silently truncate an existing file to 0 bytes behind a
		// success shaped result. An explicit `"content": ""` still writes.
		content, ok := call.Arguments["content"].(string)
		if !ok {
			return `(missing content argument: the call carried no string "content", refusing to write; resend with the full content; an intentionally empty file needs an explicit "content": "")`
		}
		return WriteFile(path, content, boolArg(call.Arguments, "append"))
	case EditFileName:
		path, _ := call.Arguments["path"].(string)
		oldString, _ := call.Arguments["old_string"].(string)
		// Same guard as write_file's content: a dropped new_string must not
		// decode to "" and silently delete the matched text. An explicit
		// `"new_string": ""` still deletes.
		newString, ok := call.Arguments["new_string"].(string)
		if !ok {
			return `(missing new_string argument: the call carried no string "new_string", refusing to edit; resend it; deleting the match needs an explicit "new_string": "")`
		}
		return EditFile(path, oldString, newString)
	case ReadFileName:
		path, _ := call.Arguments["path"].(string)
		return ReadFile(path, intArg(call.Arguments, "offset"), intArg(call.Arguments, "limit"))
	default:
		return fmt.Sprintf("(unknown tool: %s)", call.Name)
	}
}

// InlineStatus is the short script the TUI prints per tool call.
func InlineStatus(call chmctx.ToolCall) string {
	switch call.Name {
	case BashName:
		cmd, _ := call.Arguments["cmd"].(string)
		return "▶ bash: " + firstLine(cmd)
	case WriteFileName:
		path, _ := call.Arguments["path"].(string)
		return "▶ write_file: " + path
	case EditFileName:
		path, _ := call.Arguments["path"].(string)
		return "▶ edit_file: " + path
	case ReadFileName:
		path, _ := call.Arguments["path"].(string)
		return "▶ read_file: " + path
	default:
		// Fall back to the first nonempty string arg.
		for _, v := range call.Arguments {
			if s, ok := v.(string); ok && s != "" {
				return fmt.Sprintf("▶ %s: %s", call.Name, firstLine(s))
			}
		}
		return "▶ " + call.Name
	}
}

// bashOutputHead / bashOutputTail bound what headTailBuffer retains: first 1MB
// plus last 1MB of combined output. Far above ctx.Truncate's ~24KB relevance
// threshold (nothing the model would ever see is dropped), small enough that a
// firehose command can't OOM the process.
const (
	bashOutputHead = 1 << 20
	bashOutputTail = 1 << 20
)

// headTailBuffer is an io.Writer keeping the first bashOutputHead and the last
// bashOutputTail bytes written, discarding the middle. The tail is a fixed ring
// so a firehose costs a bounded copy, never an allocation per write.
type headTailBuffer struct {
	head      []byte
	ring      []byte
	pos       int   // next write index in ring
	tailBytes int64 // total bytes routed to the ring
}

func (w *headTailBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if room := bashOutputHead - len(w.head); room > 0 {
		take := min(room, len(p))
		w.head = append(w.head, p[:take]...)
		p = p[take:]
	}
	if len(p) == 0 {
		return n, nil
	}
	if w.ring == nil {
		w.ring = make([]byte, bashOutputTail)
	}
	w.tailBytes += int64(len(p))
	if len(p) >= bashOutputTail {
		copy(w.ring, p[len(p)-bashOutputTail:])
		w.pos = 0
		return n, nil
	}
	k := copy(w.ring[w.pos:], p)
	w.pos = (w.pos + k) % bashOutputTail
	if k < len(p) {
		w.pos = copy(w.ring, p[k:])
	}
	return n, nil
}

// droppedBytes is how many middle bytes the ring discarded; 0 while the whole
// output still fits head+tail.
func (w *headTailBuffer) droppedBytes() int64 {
	if d := w.tailBytes - int64(len(w.ring)); d > 0 {
		return d
	}
	return 0
}

// totalBytes is the full combined output size as written, kept or not.
func (w *headTailBuffer) totalBytes() int64 {
	return int64(len(w.head)) + w.tailBytes
}

// String reassembles head + tail. The in band seam marker keeps the two
// halves from reading as contiguous; the accurate size report lives in the
// caller's end of output note (this marker sits at the ~1MB offset, inside
// the middle ctx.Truncate drops, so the model never reads it).
func (w *headTailBuffer) String() string {
	switch {
	case w.tailBytes == 0:
		return string(w.head)
	case w.tailBytes <= int64(len(w.ring)):
		return string(w.head) + string(w.ring[:w.tailBytes])
	default:
		return string(w.head) +
			fmt.Sprintf("\n───── %d bytes OMITTED here (capture cap) ─────\n", w.droppedBytes()) +
			string(w.ring[w.pos:]) + string(w.ring[:w.pos])
	}
}

func firstLine(s string) string {
	// IndexAny over both separators, not just '\n', so a CR or CRLF first line
	// (old mac or pasted command) cuts at the same point instead of leaking a
	// bare '\r' that would yank the status line's cursor back. Mirrors
	// llm.firstLine.
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	if len(s) > 120 {
		// Snap the cut back to a rune boundary, else a multi byte char gets
		// split and the printed status carries invalid UTF 8 to the terminal.
		cut := 117
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut] + "..."
	}
	return s
}
