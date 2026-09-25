// Debug instrumentation, self contained so it can be ripped out cleanly.
// Activated by `logging: true` in config.yaml; log.txt is truncated on
// every start so a session never appends onto a stale run.
package tui

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	chmctx "github.com/codehamr/codehamr/internal/ctx"
)

var (
	dbgMu   sync.Mutex
	dbgFile *os.File
)

// OpenDebugLog truncates <dir>/log.txt and opens it for writing. On failure
// it reports once on stderr and disables logging: the log must never block
// the TUI from starting.
//
// Prompts and tool arguments can contain secrets. Only the owner should
// have permission to read the log.
func OpenDebugLog(dir string) {
	if dir == "" {
		return
	}
	path := filepath.Join(dir, "log.txt")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		fmt.Fprintln(os.Stderr, "⚠ debuglog:", err)
		return
	}
	dbgMu.Lock()
	dbgFile = f
	dbgMu.Unlock()
	dbgWritef("session", "codehamr started · project=%s", dir)
}

// CloseDebugLog flushes and closes the log. Idempotent.
func CloseDebugLog() {
	dbgMu.Lock()
	defer dbgMu.Unlock()
	if dbgFile != nil {
		_ = dbgFile.Close()
		dbgFile = nil
	}
}

// dbgEnabled reports whether logging is on. Callers use it to skip building
// expensive log payloads (e.g. accumulating a round's reasoning) when the log
// is off. dbgWritef itself is already a no operation, but the work feeding it isn't.
func dbgEnabled() bool {
	dbgMu.Lock()
	defer dbgMu.Unlock()
	return dbgFile != nil
}

// dbgWritef appends one timestamped record. No operation when logging is off. The
// timestamp carries the date too (not just the clock) so a shared log is
// unambiguous across day boundaries and correlatable with other tooling.
func dbgWritef(category, format string, args ...any) {
	dbgMu.Lock()
	defer dbgMu.Unlock()
	if dbgFile == nil {
		return
	}
	ts := time.Now().Format("2006-01-02 15:04:05.000")
	body := fmt.Sprintf(format, args...)
	fmt.Fprintf(dbgFile, "[%s] %s\n%s\n\n", ts, category, body)
}

// dbgWriteSession records the active backend and context budget once at startup.
// Behaviour differs sharply by model (different model families fail in
// different ways) and by context window, so a shared log must name exactly what
// produced it. The system prompt itself isn't dumped: it's the embedded
// PROMPT_SYS.md plus the working dir anchor, both reconstructable from the repo;
// only its size (which feeds the packing budget) is worth recording.
func dbgWriteSession(version, profile, model, url string, ctxSize, sysTokens int, tools []string) {
	dbgWritef("session",
		"codehamr %s · profile=%s · model=%s @ %s\ncontext_size=%d tokens · system_prompt≈%d tokens · tools=[%s]",
		version, profile, model, url, ctxSize, sysTokens, strings.Join(tools, ", "))
}

// dbgWriteRequest records, per LLM round, what newest first packing actually
// sent: how much of history survived the budget and how many tool outputs are
// truncated. The message bodies are already captured as user/assistant/
// tool_result records, so this logs only the packing decisions those per message
// records cannot show: what the model saw versus what was dropped. packed
// includes the prepended system message; historyLen is the pre pack history.
func dbgWriteRequest(model string, ctxSize, budget, historyLen int, packed []chmctx.Message) {
	if !dbgEnabled() {
		return
	}
	// packed[0] is the prepended system message (buildMessages always prepends
	// it). Count AND sum over history messages only (packed[1:]) so the token
	// figure matches the message count and is directly comparable to budget,
	// which is the *history* budget (ctx.Budget already subtracts the system and
	// tool reservations). Summing the ~3k token system prompt into a "packed=1
	// msgs" line would read as if that one history message were 3k tokens.
	tokens, truncated := 0, 0
	for _, msg := range packed[1:] {
		tokens += msg.Tokens()
		if strings.Contains(msg.Content, "───── truncated:") {
			truncated++
		}
	}
	note := ""
	if truncated > 0 {
		note = fmt.Sprintf(" · %d tool output(s) truncated", truncated)
	}
	// kept = packed minus the system message; dropped covers both budget
	// eviction and orphan tool drops.
	kept := len(packed) - 1
	dbgWritef("request",
		"model=%s · ctx=%d (history budget=%d) · history=%d msgs → packed=%d msgs (~%d tokens) · dropped=%d oldest%s",
		model, ctxSize, budget, historyLen, kept, tokens, historyLen-kept, note)
}

// dbgWriteMessage records a chmctx.Message readably: content and tool calls
// each get a labeled section. No operation when logging is off, so callers needn't guard.
func dbgWriteMessage(category string, msg chmctx.Message) {
	if !dbgEnabled() {
		return
	}
	var b strings.Builder
	if msg.Content != "" {
		b.WriteString("CONTENT:\n")
		b.WriteString(msg.Content)
		b.WriteString("\n")
	}
	for _, tc := range msg.ToolCalls {
		args, _ := json.Marshal(tc.Arguments)
		fmt.Fprintf(&b, "TOOL_CALL %s id=%s args=%s\n", tc.Name, tc.ID, args)
	}
	if msg.ToolCallID != "" {
		fmt.Fprintf(&b, "tool=%s id=%s\n", msg.ToolName, msg.ToolCallID)
	}
	dbgWritef(category, "%s", strings.TrimRight(b.String(), "\n"))
}
