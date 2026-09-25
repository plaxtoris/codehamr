// Package ctx owns conversation messages, tool output truncation, and
// newest first packing.
package ctx

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"
)

type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

type ToolCall struct {
	ID        string         `json:"id,omitempty"`
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

type Message struct {
	Role       Role       `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	ToolName   string     `json:"name,omitempty"`
	// Reasoning holds the assistant round's reasoning items exactly as the
	// server emitted them, replayed verbatim on every later request. Opaque on
	// purpose: OpenAI's are encrypted, a local server's are plain text, and
	// neither is ever read here. They live on the message so the newest first
	// budget walk keeps a round's reasoning, calls and results together.
	Reasoning []json.RawMessage `json:"reasoning,omitempty"`
}

// Tokens estimates tokens as one per four bytes, rounded up.
func Tokens(s string) int { return (len(s) + 3) / 4 }

func (m Message) Tokens() int {
	n := Tokens(m.Content)
	for _, tc := range m.ToolCalls {
		n += Tokens(tc.Name)
		for k, v := range tc.Arguments {
			n += Tokens(k) + Tokens(fmt.Sprint(v))
		}
	}
	// Charge raw reasoning bytes to the estimate as well. Encrypted payload
	// length need not match the provider token count.
	for _, r := range m.Reasoning {
		n += Tokens(string(r))
	}
	return n + 8
}

const (
	ToolOutputCap = 6000
	// Head/tail kept when a result overflows the cap. Tail heavy on purpose: in
	// practice only bash output ever overflows (read_file windows itself under
	// the cap), and the failing assertion, stack trace, or compiler error of a
	// long command sits at the END of its output; the spill file already covers
	// the dropped middle, so the free window should hold the part that names
	// the failure.
	ToolTruncHead = 1000
	ToolTruncTail = 3000
	// FixedSystem reserves the embedded prompt and working directory anchor.
	// FixedTools reserves the four tool schemas. Tests compare both reservations
	// to the current content so prompt changes cannot silently consume history.
	FixedSystem = 1600
	FixedTools  = 1100
)

// budgetHeadroomDivisor leaves ten percent of the history budget unused.
// The byte estimate can differ from provider tokenization, so this margin
// reduces the chance of overflowing the served context window.
const budgetHeadroomDivisor = 10

// ResponseReserve is the slice Budget keeps free for the model's response.
// Scales as ctxSize/8 so reasoning models get room (262k→32k, 1M→125k),
// floored at 8k so small ctx profiles don't collapse history to nothing.
func ResponseReserve(ctxSize int) int {
	if r := ctxSize / 8; r > 8000 {
		return r
	}
	return 8000
}

// Truncate collapses oversized tool outputs to first 1k + last 3k tokens;
// inputs at or under 6k pass through unchanged. Head/tail can't overlap:
// >6k tokens means >24k bytes, well over the 16k kept. Boundaries snap to a
// valid UTF 8 rune start so non ASCII output never breaks mid sequence.
// The marker defers to the spill file tools.Execute names right below it, so
// the model never reads "rerun the command" and "don't rerun it" in one
// result; rerunning narrower is only the fallback when no spill happened.
func Truncate(out string) string {
	total := Tokens(out)
	if total <= ToolOutputCap {
		return out
	}
	head := runeBoundaryDown(out, ToolTruncHead*4)
	tail := runeBoundaryUp(out, len(out)-ToolTruncTail*4)
	marker := fmt.Sprintf("\n───── truncated: %d tokens total, first %d + last %d shown, the middle is OMITTED. This is a PARTIAL view; you can't review or conclude from what you can't see here. If a line below names a full output file, grep or read that file for the omitted span; otherwise rerun narrower (grep/sed/head/tail). ─────\n",
		total, ToolTruncHead, ToolTruncTail)
	return out[:head] + marker + out[tail:]
}

// runeBoundaryDown walks i left to a rune start so out[:i] never ends
// mid sequence. Safe for i == len(out).
func runeBoundaryDown(out string, i int) int {
	if i >= len(out) {
		return len(out)
	}
	for i > 0 && !utf8.RuneStart(out[i]) {
		i--
	}
	return i
}

// runeBoundaryUp walks i right to a rune start so out[i:] never starts
// mid sequence. Safe for i <= 0.
func runeBoundaryUp(out string, i int) int {
	if i <= 0 {
		return 0
	}
	for i < len(out) && !utf8.RuneStart(out[i]) {
		i++
	}
	return i
}

// PackResult records what Pack kept: the packed messages.
type PackResult struct {
	Messages []Message
}

// Pack keeps recent whole messages and returns them in conversation order.
// The newest message is kept even if it exceeds the budget. Cleanup drops
// incomplete tool exchanges, preserves a user message, and converts later
// system notes to user messages for backends requiring one leading system
// message. The embedded system prompt is added separately by the TUI.
func Pack(history []Message, budget int) PackResult {
	kept := make([]Message, 0, len(history))
	used := 0
	// walk newest to oldest
	for i := len(history) - 1; i >= 0; i-- {
		cost := history[i].Tokens()
		if len(kept) > 0 && used+cost > budget {
			break
		}
		kept = append(kept, history[i])
		used += cost
	}
	slices.Reverse(kept)
	// Dangling assistant first: dropping it can orphan its partial tool results,
	// which the following dropOrphanTools pass then cleans up.
	kept = dropDanglingToolCalls(kept)
	kept = dropOrphanTools(kept)
	// The cleanup passes can lose the current turn's tool exchange when the
	// newest tool result's owning assistant fell just past the budget cut: the
	// budget walk keeps the lone tool result (plus any trailing system nudge),
	// then the orphan drop removes it, so the next request would silently lose
	// the whole conversation mid turn (reachable on small ctx profiles after a
	// big tool output). Keyed on "nothing substantive survived", NOT on
	// len(kept)==0: a failure/runaway nudge (or the empty assistant reply the
	// empty reply nudge answers) survives the cleanup as the sole keeper and
	// would otherwise mask exactly this loss: and nothing substantive
	// surviving already implies the newest tool result didn't. A surviving
	// user message or real assistant reply instead means the conversation
	// moved past the exchange, ordinary budget trimming, no over budget
	// resurrection. Recover the newest assistant+tool results group whole,
	// over budget if need be, with the same deliberately over budget
	// guarantee a newest user message already gets.
	if i := newestToolIndex(history); i >= 0 && onlyNonSubstantive(kept) {
		// Recover the group over budget, then rerun the same two passes the
		// normal path uses: a partially answered parallel set (owner issued c1,c2
		// but only c1 came back before an abort) would otherwise reach the wire as
		// a dangling assistant and 400 every backend. Fully answered groups pass
		// through untouched; an unpairable partial empties to nothing. Survivors
		// in kept are all newer than the recovered group (the budget walk keeps a
		// suffix), so prepending keeps the order chronological.
		group := newestToolGroup(history[:i+1])
		group = dropDanglingToolCalls(group)
		group = dropOrphanTools(group)
		kept = append(group, kept...)
	}
	kept = anchorUserMessage(kept, history)
	kept = demoteSystemMessages(kept)
	return PackResult{Messages: kept}
}

// anchorUserMessage guarantees the packed window carries a user role message
// whenever history has one. The newest first walk drops oldest first, so a long
// single turn (one task message, then dozens of assistant+tool rounds that fill
// the budget) evicts the sole user task and hands the backend a userless window,
// which 400s every OpenAI compatible server ("no user query found in messages").
// When no user survived, recover the FIRST user message (the original task, the
// agent's anchor against drift), prepended chronologically over budget: the same
// deliberately over budget guarantee newestToolGroup and the always keep newest
// path already make. A lone user message carries no tool call pairing, so this is
// safe after the dangling/orphan passes. No operation when a recent user already
// survived (normal multi turn) or history has no user message at all.
func anchorUserMessage(kept, history []Message) []Message {
	for _, m := range kept {
		if m.Role == RoleUser {
			return kept
		}
	}
	for i := range history {
		if history[i].Role == RoleUser {
			return append([]Message{history[i]}, kept...)
		}
	}
	return kept
}

// demoteSystemMessages rewrites every system role message in the packed history
// to a user message. buildMessages always prepends the embedded system prompt as
// wire element 0, so any system message Pack returns is a SECOND, non leading
// system message, which strict OpenAI compat backends reject outright ("System
// message must be at the beginning"; observed on strict backends like Ollama
// and llama.cpp), the same class of wire shape 400 the dangling/orphan/userless passes
// guard against. The only system content reaching history is a soft nudge note
// (the embedded prompt is never stored there), so demoting to user keeps that note,
// automated check prefix and all, in front of the model while keeping the wire
// legal everywhere. Must run AFTER anchorUserMessage: a demoted nudge would
// otherwise masquerade as a surviving user message and suppress the original task
// anchor. Mutates only the copied kept slice, never history.
func demoteSystemMessages(kept []Message) []Message {
	for i := range kept {
		if kept[i].Role == RoleSystem {
			kept[i].Role = RoleUser
		}
	}
	return kept
}

// newestToolGroup returns the assistant that issued the newest tool result
// together with every tool result answering it, chronologically: the minimal
// well formed unit that honours "always keep the newest" when the newest
// history message is a tool result. nil when the newest message isn't an
// identifiable tool result (the budget walk already keeps non tool newests) or
// no owning assistant exists (an unpairable tool can't be kept anyway).
func newestToolGroup(history []Message) []Message {
	if len(history) == 0 {
		return nil
	}
	last := history[len(history)-1]
	if last.Role != RoleTool || last.ToolCallID == "" {
		return nil
	}
	owner := -1
search:
	for i := len(history) - 2; i >= 0; i-- {
		if history[i].Role != RoleAssistant {
			continue
		}
		for _, tc := range history[i].ToolCalls {
			if tc.ID == last.ToolCallID {
				owner = i
				break search
			}
		}
	}
	if owner < 0 {
		return nil
	}
	// Parallel tool calls put [assistant(c1,c2), tool(c1), tool(c2)] at the tail,
	// so collect every tool result whose id the owning assistant issued, not
	// just the immediately preceding one.
	ids := map[string]bool{}
	for _, tc := range history[owner].ToolCalls {
		if tc.ID != "" {
			ids[tc.ID] = true
		}
	}
	group := []Message{history[owner]}
	for i := owner + 1; i < len(history); i++ {
		if history[i].Role == RoleTool && ids[history[i].ToolCallID] {
			group = append(group, history[i])
		}
	}
	return group
}

// newestToolIndex returns the index of the newest tool result message in
// history, or -1. Everything after it can only be non tool (a system nudge, an
// assistant summary), so it marks the current turn's newest tool exchange.
func newestToolIndex(history []Message) int {
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role == RoleTool {
			return i
		}
	}
	return -1
}

// onlyNonSubstantive reports whether kept carries no substantive conversation:
// only system role notes (soft nudges) and empty assistant messages (no text,
// no tool calls: the stall shape the empty reply nudge answers, appended
// right before that nudge). TrimSpace matches tui.newestAssistantEmpty's
// definition of "empty", so a whitespace only stall can't mask the recovery.
// Vacuously true for an empty slice.
func onlyNonSubstantive(kept []Message) bool {
	for _, m := range kept {
		if m.Role == RoleSystem {
			continue
		}
		if m.Role == RoleAssistant && strings.TrimSpace(m.Content) == "" && len(m.ToolCalls) == 0 {
			continue
		}
		return false
	}
	return true
}

// dropOrphanTools removes tool messages that are not part of the contiguous
// tool run answering the assistant directly before them: sending one alone
// 400s on every OpenAI compatible backend ("tool message without preceding
// tool_calls"). Positional like dropDanglingToolCalls, and for the same
// reason: a "was this ID ever issued" lookup would let a reused index derived
// ID ("call_0" on local backends) from an OLDER turn vouch for a stray tool
// result whose own assistant was dropped, leaving a tool message right after
// a user message on the wire.
//
// Empty IDs are orphans on both ends: an unidentifiable tool message has no
// valid pairing.
func dropOrphanTools(kept []Message) []Message {
	out := kept[:0]
	// IDs issued by the assistant whose contiguous tool run we're inside;
	// nil once any non tool message ends the run.
	var current map[string]bool
	for _, m := range kept {
		switch m.Role {
		case RoleAssistant:
			current = map[string]bool{}
			for _, tc := range m.ToolCalls {
				if tc.ID != "" {
					current[tc.ID] = true
				}
			}
		case RoleTool:
			if m.ToolCallID == "" || !current[m.ToolCallID] {
				continue
			}
		default:
			current = nil
		}
		out = append(out, m)
	}
	return out
}

// dropDanglingToolCalls removes any assistant message whose tool_calls include
// an id with no answering tool message in the kept slice: the mirror of
// dropOrphanTools. An assistant.tool_calls followed by fewer tool results than
// calls issued 400s every OpenAI compatible backend with "missing tool
// response". This shape is produced whenever a turn is aborted mid tool: the
// TUI appends the assistant.tool_calls as soon as the round closes, but a Ctrl+C
// / stream error / idle stall then drops the pending calls so their tool results
// never arrive (see tui.endTurn). On the user's next request that dangling
// assistant would otherwise reach the wire and wedge the conversation until
// /clear. Empty ids count as unanswered: an unidentifiable call can't be paired.
func dropDanglingToolCalls(kept []Message) []Message {
	out := kept[:0]
	for i, m := range kept {
		if m.Role == RoleAssistant && len(m.ToolCalls) > 0 {
			// Answers must come from the contiguous run of tool messages that
			// follows THIS assistant, the only shape the wire accepts. A global
			// ID lookup would let a later turn's reused ID (index derived
			// "call_0"-style IDs are common on local backends) vouch for an
			// aborted call here, sending the dangling assistant to the wire and
			// wedging the conversation with 400s until /clear.
			answered := map[string]bool{}
			for j := i + 1; j < len(kept) && kept[j].Role == RoleTool; j++ {
				if kept[j].ToolCallID != "" {
					answered[kept[j].ToolCallID] = true
				}
			}
			dangling := false
			for _, tc := range m.ToolCalls {
				if !answered[tc.ID] {
					dangling = true
					break
				}
			}
			if dangling {
				continue
			}
		}
		out = append(out, m)
	}
	return out
}

// Budget subtracts the fixed reservations from the total context size, then
// leaves a headroom margin (see budgetHeadroomDivisor) so a bytes/4 undercount
// can't push the real prompt past the declared window.
func Budget(ctxSize int) int {
	b := ctxSize - FixedSystem - FixedTools - ResponseReserve(ctxSize)
	if b < 0 {
		return 0
	}
	return b - b/budgetHeadroomDivisor
}
