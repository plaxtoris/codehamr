package ctx

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/codehamr/codehamr/internal/config"
)

// TestEmbeddedPromptFitsFixedSystem guards the invariant the packer relies on:
// FixedSystem must reserve enough budget for the embedded system prompt PLUS the
// "\n\nWorking directory: <path>" anchor buildSystem appends. If a prompt edit
// pushes the embedded text past this, the reservation under counts and packing
// silently over fills the real context, a bug with no other test to catch it.
// anchorAllowance covers a generously long working directory path (~384 chars).
func TestEmbeddedPromptFitsFixedSystem(t *testing.T) {
	const anchorAllowance = 96 // tokens; "\n\nWorking directory: " + a deep container path
	used := Tokens(config.DefaultSystemPrompt) + anchorAllowance
	if used > FixedSystem {
		t.Fatalf("embedded prompt (%d tok) + anchor allowance (%d) = %d exceeds FixedSystem=%d; trim the prompt or bump FixedSystem",
			Tokens(config.DefaultSystemPrompt), anchorAllowance, used, FixedSystem)
	}
}

func TestTokensHeuristic(t *testing.T) {
	cases := map[string]int{
		"":         0,
		"a":        1,
		"abcd":     1,
		"abcde":    2,
		"12345678": 2,
	}
	for s, want := range cases {
		if got := Tokens(s); got != want {
			t.Errorf("Tokens(%q) = %d, want %d", s, got, want)
		}
	}
}

// TestMessageTokensCountsToolCallArguments pins ToolCall.Arguments accounting
// in Message.Tokens: it feeds the budget on every tool using turn, yet no
// other test populates Arguments. Asserts the delta so the +8 per message
// overhead can't mask a dropped Arguments loop.
func TestMessageTokensCountsToolCallArguments(t *testing.T) {
	base := Message{Role: RoleAssistant, ToolCalls: []ToolCall{{Name: "bash"}}}.Tokens()
	withArgs := Message{Role: RoleAssistant, ToolCalls: []ToolCall{
		{Name: "bash", Arguments: map[string]any{"cmd": "echo hello world"}},
	}}.Tokens()
	// args add Tokens("cmd")=1 + Tokens(fmt.Sprint("echo hello world"))=4 = 5.
	if got := withArgs - base; got != 5 {
		t.Fatalf("argument cost = %d, want 5 (Message.Tokens must account for ToolCall.Arguments)", got)
	}
}

// TestMessageTokensCountsReasoning: replayed reasoning items ride on every
// later request, so they must cost budget like any other bytes on the wire.
func TestMessageTokensCountsReasoning(t *testing.T) {
	item := json.RawMessage(`{"type":"reasoning","encrypted_content":"` + strings.Repeat("A", 40) + `"}`)
	base := Message{Role: RoleAssistant, Content: "ok"}.Tokens()
	with := Message{Role: RoleAssistant, Content: "ok", Reasoning: []json.RawMessage{item}}.Tokens()
	if got, want := with-base, Tokens(string(item)); got != want {
		t.Fatalf("reasoning cost = %d tokens, want %d (Message.Tokens must account for Reasoning)", got, want)
	}
}

func TestTruncateSmallUntouched(t *testing.T) {
	in := strings.Repeat("x", 20000) // 5000 tokens, under 6k cap
	if out := Truncate(in); out != in {
		t.Fatalf("expected no change for small output")
	}
}

func TestTruncateLargeCollapses(t *testing.T) {
	in := strings.Repeat("abcd", 8000) // 32000 chars ~= 8000 tokens
	out := Truncate(in)
	if !strings.Contains(out, "truncated") {
		t.Fatalf("expected truncation marker, got %q", out)
	}
	if Tokens(out) > ToolTruncHead+ToolTruncTail+200 {
		t.Fatalf("truncated output too large: %d tokens", Tokens(out))
	}
	if !strings.HasPrefix(out, in[:100]) {
		t.Fatal("expected head preserved")
	}
	if !strings.HasSuffix(out, in[len(in)-100:]) {
		t.Fatal("expected tail preserved")
	}
	// Tail bias: errors sit at the end of long output, so the kept tail must be
	// larger than the kept head (the spill file covers the dropped middle).
	if ToolTruncTail <= ToolTruncHead {
		t.Fatalf("truncation must keep more tail than head, got head=%d tail=%d", ToolTruncHead, ToolTruncTail)
	}
}

// TestTruncateSnapsToRuneBoundary: Truncate's byte offset cut must not slice
// multi byte runes mid sequence: output stays valid UTF 8.
func TestTruncateSnapsToRuneBoundary(t *testing.T) {
	in := strings.Repeat("ä", 20000) // 2 bytes each, 40000 bytes total = 10000 tokens
	out := Truncate(in)
	if !strings.Contains(out, "truncated") {
		t.Fatalf("expected truncation marker, got %q", out[:80])
	}
	if !utf8.ValidString(out) {
		t.Fatal("Truncate produced invalid UTF-8: slice landed mid-rune")
	}
}

func TestPackNewestFirstWhole(t *testing.T) {
	big := strings.Repeat("a", 4*1000) // 1000 tokens
	history := []Message{
		{Role: RoleUser, Content: big},
		{Role: RoleAssistant, Content: big},
		{Role: RoleUser, Content: big},
		{Role: RoleAssistant, Content: big},
	}
	r := Pack(history, 2500)
	// Each message costs Tokens(4000 bytes)+8 = 1008. Budget 2500 keeps newest
	// (1008) and msg3 (2016 <= 2500), breaks before msg2 (3024 > 2500), so
	// exactly 2, pinning the `used+cost > budget` break against off by one.
	if len(r.Messages) != 2 {
		t.Fatalf("kept=%d want exactly 2", len(r.Messages))
	}
	// last message must always be kept
	if r.Messages[len(r.Messages)-1].Content != big {
		t.Fatal("newest message not preserved")
	}
}

func TestPackAlwaysKeepsNewest(t *testing.T) {
	massive := strings.Repeat("z", 4*10000)
	history := []Message{
		{Role: RoleUser, Content: "small"},
		{Role: RoleUser, Content: massive},
	}
	r := Pack(history, 100)
	if len(r.Messages) != 1 {
		t.Fatalf("expected only newest kept, got %d", len(r.Messages))
	}
	if r.Messages[0].Content != massive {
		t.Fatal("newest should have been kept even if over budget")
	}
}

// TestPackDropsOrphanToolMessage: if budget trimming cuts the assistant whose
// tool_calls spawned a tool message, that orphan must be dropped, else
// OpenAI compat servers 400 with "tool message without preceding tool_calls".
func TestPackDropsOrphanToolMessage(t *testing.T) {
	fortyX := strings.Repeat("x", 40)
	history := []Message{
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "c1", Name: "bash"}}},
		{Role: RoleTool, ToolCallID: "c1", Content: fortyX},
		{Role: RoleAssistant, Content: "reply"},
	}
	// Tight enough to drop the first assistant, loose enough that the tool
	// message would otherwise survive. Tuned against the +8 per message overhead.
	r := Pack(history, 30)
	for _, m := range r.Messages {
		if m.Role == RoleTool {
			t.Fatalf("orphan tool message survived pack: %+v", r.Messages)
		}
	}
	if len(r.Messages) == 0 {
		t.Fatal("newest assistant should have survived")
	}
}

// TestPackDropsEmptyIDToolMessages: an empty ID tool_call must not let
// subsequent empty ToolCallID tool messages ride through as "paired". An
// unidentifiable tool message can never be paired, so it's always dropped,
// else OpenAI compat backends 400 on the bare tool message.
func TestPackDropsEmptyIDToolMessages(t *testing.T) {
	history := []Message{
		// Malformed assistant with a missing tool_call id (server bug).
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "", Name: "bash"}}},
		// Looks paired via the empty ID; must be dropped anyway.
		{Role: RoleTool, ToolCallID: "", Content: "from empty1"},
		// Clearly orphan, nothing to pair with.
		{Role: RoleTool, ToolCallID: "", Content: "TRULY ORPHAN"},
		{Role: RoleAssistant, Content: "final"},
	}
	r := Pack(history, 100000)
	for _, m := range r.Messages {
		if m.Role == RoleTool {
			t.Fatalf("empty-ID tool message survived pack: %+v (full kept set: %+v)", m, r.Messages)
		}
	}
}

// TestPackKeepsPairedToolMessage: a healthy assistant+tool pair that fits the
// budget stays intact: don't regress into dropping good pairs.
func TestPackKeepsPairedToolMessage(t *testing.T) {
	history := []Message{
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "c1", Name: "bash"}}},
		{Role: RoleTool, ToolCallID: "c1", Content: "ok"},
		{Role: RoleAssistant, Content: "done"},
	}
	r := Pack(history, 10000)
	if len(r.Messages) != 3 {
		t.Fatalf("all 3 messages should be kept, got %d: %+v", len(r.Messages), r.Messages)
	}
}

// TestPackKeepsNewestToolPairOverBudget: when the newest message is a tool
// result and its owning assistant won't fit the budget, Pack must keep the pair
// whole rather than drop the lone orphan and collapse to nothing, otherwise the
// next request carries only the system prompt and silently loses the whole
// conversation. Reachable on small ctx profiles after a big tool output.
func TestPackKeepsNewestToolPairOverBudget(t *testing.T) {
	bigArgs := strings.Repeat("x", 4*5000) // ~5000 token write_file content
	history := []Message{
		{Role: RoleUser, Content: "do the thing"},
		{Role: RoleAssistant, ToolCalls: []ToolCall{
			{ID: "c1", Name: "write_file", Arguments: map[string]any{"content": bigArgs}},
		}},
		{Role: RoleTool, ToolCallID: "c1", Content: "wrote 20000 bytes"},
	}
	r := Pack(history, 500) // far below the assistant's cost
	if len(r.Messages) == 0 {
		t.Fatal("Pack collapsed to zero messages: newest tool pair was dropped")
	}
	var sawAssistant, sawTool bool
	for _, m := range r.Messages {
		if m.Role == RoleAssistant && len(m.ToolCalls) > 0 {
			sawAssistant = true
		}
		if m.Role == RoleTool {
			sawTool = true
		}
	}
	if !sawAssistant || !sawTool {
		t.Fatalf("expected the assistant+tool pair to survive, got %+v", r.Messages)
	}
}

// TestPackKeepsNewestParallelToolGroupOverBudget: with parallel tool calls the
// tail is [assistant(c1,c2), tool(c1), tool(c2)], so the newest tool's owner is
// not the immediately preceding message. Pack must recover the whole group, and
// no tool may survive without the assistant that issued its id.
func TestPackKeepsNewestParallelToolGroupOverBudget(t *testing.T) {
	bigArgs := strings.Repeat("y", 4*5000)
	history := []Message{
		{Role: RoleUser, Content: "do two things"},
		{Role: RoleAssistant, ToolCalls: []ToolCall{
			{ID: "c1", Name: "bash", Arguments: map[string]any{"cmd": bigArgs}},
			{ID: "c2", Name: "bash"},
		}},
		{Role: RoleTool, ToolCallID: "c1", Content: "out1"},
		{Role: RoleTool, ToolCallID: "c2", Content: "out2"},
	}
	r := Pack(history, 300)
	if len(r.Messages) == 0 {
		t.Fatal("Pack collapsed to zero messages: parallel tool group was dropped")
	}
	ids := map[string]bool{}
	for _, m := range r.Messages {
		if m.Role == RoleAssistant {
			for _, tc := range m.ToolCalls {
				ids[tc.ID] = true
			}
		}
	}
	for _, m := range r.Messages {
		if m.Role == RoleTool && !ids[m.ToolCallID] {
			t.Fatalf("orphan tool %q survived without its assistant: %+v", m.ToolCallID, r.Messages)
		}
	}
}

// TestPackRecoveryDropsPartialParallelGroup: the empty recovery path (newest is
// a tool result whose owner fell past a tight budget) must not resurrect a
// partially answered parallel group as a dangling assistant. owner issued c1,c2
// but only c1 came back; newestToolGroup recovers [assistant(c1,c2), tool(c1)],
// which must then be stripped to nothing rather than reaching the wire and 400ing.
func TestPackRecoveryDropsPartialParallelGroup(t *testing.T) {
	bigArgs := strings.Repeat("y", 4*5000) // owner far over the tight budget below
	history := []Message{
		{Role: RoleUser, Content: "do two things"},
		{Role: RoleAssistant, ToolCalls: []ToolCall{
			{ID: "c1", Name: "bash", Arguments: map[string]any{"cmd": bigArgs}},
			{ID: "c2", Name: "bash"},
		}},
		{Role: RoleTool, ToolCallID: "c1", Content: "out1"}, // c2 result never arrived
	}
	r := Pack(history, 300) // forces the budget walk + drops to empty, then recovery
	for _, m := range r.Messages {
		if m.Role == RoleAssistant && len(m.ToolCalls) > 0 {
			t.Fatalf("dangling partial-parallel assistant reached the wire: %+v", r.Messages)
		}
		if m.Role == RoleTool {
			t.Fatalf("orphaned tool survived after its assistant was dropped: %+v", r.Messages)
		}
	}
}

// TestPackDropsDanglingAssistantToolCalls: a turn cancelled mid tool leaves an
// assistant.tool_calls in history with no answering tool message (the TUI
// appended it on round close, then endTurn dropped the pending call on Ctrl+C).
// On the next request Pack must strip that dangling assistant, otherwise the
// wire carries an unanswered tool_calls and every OpenAI compat backend 400s.
func TestPackDropsDanglingAssistantToolCalls(t *testing.T) {
	history := []Message{
		{Role: RoleUser, Content: "run a long thing"},
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "c1", Name: "bash"}}},
		// no tool(c1): the user Ctrl+C'd before it finished
		{Role: RoleUser, Content: "actually, do this instead"},
	}
	r := Pack(history, 100000)
	for _, m := range r.Messages {
		if m.Role == RoleAssistant && len(m.ToolCalls) > 0 {
			t.Fatalf("dangling assistant.tool_calls survived pack: %+v", r.Messages)
		}
	}
	// Both user messages must remain, only the unpaired assistant is dropped.
	if len(r.Messages) != 2 {
		t.Fatalf("want 2 user messages kept, got %d: %+v", len(r.Messages), r.Messages)
	}
}

// TestPackDropsDanglingPartialParallelGroup: cancelling a parallel round after
// only some results return leaves assistant(c1,c2)+tool(c1). The assistant has
// an unanswered call (c2), so it must be dropped, and dropOrphanTools must then
// clean up the now orphaned tool(c1).
func TestPackDropsDanglingPartialParallelGroup(t *testing.T) {
	history := []Message{
		{Role: RoleUser, Content: "do two things"},
		{Role: RoleAssistant, ToolCalls: []ToolCall{
			{ID: "c1", Name: "bash"}, {ID: "c2", Name: "bash"},
		}},
		{Role: RoleTool, ToolCallID: "c1", Content: "out1"}, // c2 never answered
		{Role: RoleUser, Content: "next"},
	}
	r := Pack(history, 100000)
	for _, m := range r.Messages {
		if m.Role == RoleAssistant && len(m.ToolCalls) > 0 {
			t.Fatalf("partial parallel assistant survived: %+v", r.Messages)
		}
		if m.Role == RoleTool {
			t.Fatalf("orphaned tool(c1) survived after its assistant was dropped: %+v", r.Messages)
		}
	}
}

// TestPackDropsDanglingAssistantDespiteIDReuse: pairing is positional, not a
// global ID lookup. Local backends commonly reuse index derived IDs ("call_0")
// across turns, so a later turn's answered call must not vouch for an earlier
// turn's aborted (dangling) assistant: that shape reaches the wire and 400s
// every backend ("missing tool response") until /clear.
func TestPackDropsDanglingAssistantDespiteIDReuse(t *testing.T) {
	history := []Message{
		{Role: RoleUser, Content: "task"},
		// Turn 1: aborted mid tool (Ctrl+C); call_0 never answered.
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "call_0", Name: "bash"}}},
		// Turn 2: the backend reuses call_0; this exchange is complete.
		{Role: RoleUser, Content: "again"},
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "call_0", Name: "bash"}}},
		{Role: RoleTool, ToolCallID: "call_0", Content: "ok"},
		{Role: RoleAssistant, Content: "done"},
	}
	r := Pack(history, 100000)
	owners := 0
	for i, m := range r.Messages {
		if m.Role != RoleAssistant || len(m.ToolCalls) == 0 {
			continue
		}
		owners++
		if i+1 >= len(r.Messages) || r.Messages[i+1].Role != RoleTool {
			t.Fatalf("turn-1 dangling assistant survived via the reused ID: %+v", r.Messages)
		}
	}
	if owners != 1 {
		t.Fatalf("exactly the answered assistant must survive, got %d owners: %+v", owners, r.Messages)
	}
}

// TestPackRecoversNewestToolGroupPastTrailingNudge: the over budget recovery
// for the newest tool exchange must fire even when a failure/runaway nudge was
// appended after the tool result. The nudge survives the budget walk as the
// sole keeper, so a bare emptiness check would see "something survived" and
// silently lose the current turn's exchange from the window.
func TestPackRecoversNewestToolGroupPastTrailingNudge(t *testing.T) {
	bigArgs := strings.Repeat("x", 4*5000) // ~5000 token write_file content
	history := []Message{
		{Role: RoleUser, Content: "do the thing"},
		{Role: RoleAssistant, ToolCalls: []ToolCall{
			{ID: "c1", Name: "write_file", Arguments: map[string]any{"content": bigArgs}},
		}},
		{Role: RoleTool, ToolCallID: "c1", Content: "(write error: boom)"},
		{Role: RoleSystem, Content: "nudge: change approach"},
	}
	r := Pack(history, 2000) // the owning assistant alone exceeds the budget
	owner, tool, nudge := -1, -1, -1
	for i, m := range r.Messages {
		switch {
		case m.Role == RoleAssistant && len(m.ToolCalls) > 0:
			owner = i
		case m.Role == RoleTool && m.ToolCallID == "c1":
			tool = i
		case strings.Contains(m.Content, "change approach"):
			nudge = i
		}
	}
	if owner < 0 || tool < 0 {
		t.Fatalf("newest tool exchange lost despite trailing nudge (owner=%d tool=%d): %+v",
			owner, tool, r.Messages)
	}
	// Order must stay chronological: recovered group before the surviving
	// nudge, or the demoted nudge precedes the exchange it comments on.
	if !(owner < tool && tool < nudge) {
		t.Fatalf("recovered group must precede the nudge (owner=%d tool=%d nudge=%d): %+v",
			owner, tool, nudge, r.Messages)
	}
}

// TestPackRecoversNewestToolGroupPastEmptyAssistantTail: the stall shape the
// empty reply nudge answers appends an EMPTY assistant message, then the
// nudge, after the tool result. Both are non substantive; if the cheap empty
// assistant (8 tokens) survives the walk it must not mask the recovery, or
// the wire tells the model to "reissue the call" it can no longer see.
func TestPackRecoversNewestToolGroupPastEmptyAssistantTail(t *testing.T) {
	bigArgs := strings.Repeat("x", 4*5000)
	history := []Message{
		{Role: RoleUser, Content: "do the thing"},
		{Role: RoleAssistant, ToolCalls: []ToolCall{
			{ID: "c1", Name: "write_file", Arguments: map[string]any{"content": bigArgs}},
		}},
		{Role: RoleTool, ToolCallID: "c1", Content: strings.Repeat("y", 4*3000)},
		{Role: RoleAssistant, Content: ""}, // the stall
		{Role: RoleSystem, Content: "nudge: reissue the call"},
	}
	r := Pack(history, 2000)
	var hasOwner, hasTool bool
	for _, m := range r.Messages {
		if m.Role == RoleAssistant && len(m.ToolCalls) > 0 {
			hasOwner = true
		}
		if m.Role == RoleTool && m.ToolCallID == "c1" {
			hasTool = true
		}
	}
	if !hasOwner || !hasTool {
		t.Fatalf("empty-assistant tail masked the recovery (owner=%v tool=%v): %+v",
			hasOwner, hasTool, r.Messages)
	}
}

// TestPackDropsStrayToolResultDespiteIDReuse: dropOrphanTools is positional
// too. A stray tool result whose own (dangling, dropped) assistant reused an
// older turn's ID must not survive via that older assistant's issuance: it
// would land right after a user message and 400 strict backends.
func TestPackDropsStrayToolResultDespiteIDReuse(t *testing.T) {
	history := []Message{
		{Role: RoleUser, Content: "task"},
		// Turn 1 completes with call_0.
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "call_0", Name: "bash"}}},
		{Role: RoleTool, ToolCallID: "call_0", Content: "ok"},
		{Role: RoleAssistant, Content: "done"},
		{Role: RoleUser, Content: "again"},
		// Turn 2 reuses call_0 in a parallel set and is aborted after one result.
		{Role: RoleAssistant, ToolCalls: []ToolCall{
			{ID: "call_0", Name: "bash"}, {ID: "call_1", Name: "bash"},
		}},
		{Role: RoleTool, ToolCallID: "call_0", Content: "partial"},
		{Role: RoleUser, Content: "next"},
	}
	r := Pack(history, 100000)
	for i, m := range r.Messages {
		if m.Role != RoleTool {
			continue
		}
		if i == 0 || r.Messages[i-1].Role == RoleUser || m.Content == "partial" {
			t.Fatalf("stray tool result survived via reused ID: %+v", r.Messages)
		}
	}
}

// TestPackKeepsFullyAnsweredToolCalls: don't over reach. An assistant whose
// every tool_call is answered (including parallel) must survive intact.
func TestPackKeepsFullyAnsweredToolCalls(t *testing.T) {
	history := []Message{
		{Role: RoleAssistant, ToolCalls: []ToolCall{
			{ID: "c1", Name: "bash"}, {ID: "c2", Name: "bash"},
		}},
		{Role: RoleTool, ToolCallID: "c1", Content: "out1"},
		{Role: RoleTool, ToolCallID: "c2", Content: "out2"},
		{Role: RoleAssistant, Content: "done"},
	}
	r := Pack(history, 100000)
	if len(r.Messages) != 4 {
		t.Fatalf("fully-answered group must survive, got %d: %+v", len(r.Messages), r.Messages)
	}
}

// TestPackAnchorsUserTaskOverBudget: a long single turn (one small task message,
// then many large assistant rounds that fill the budget) would let the
// newest first walk evict the sole user task, leaving a userless window that
// 400s every OpenAI compat backend ("no user query found in messages"). Pack
// must re anchor the original task over budget so the wire always carries it.
func TestPackAnchorsUserTaskOverBudget(t *testing.T) {
	big := strings.Repeat("x", 4*3000) // ~3000 token assistant chunk
	history := []Message{
		{Role: RoleUser, Content: "BUILD THE GALAXY"},
		{Role: RoleAssistant, Content: big},
		{Role: RoleAssistant, Content: big},
		{Role: RoleAssistant, Content: big},
	}
	r := Pack(history, 5000) // holds ~1 assistant chunk; the task gets evicted
	var sawTask bool
	for _, m := range r.Messages {
		if m.Role == RoleUser && m.Content == "BUILD THE GALAXY" {
			sawTask = true
		}
	}
	if !sawTask {
		t.Fatalf("original user task was evicted, leaving a userless window: %+v", r.Messages)
	}
	// Chronological order: the anchored task must lead the window.
	if r.Messages[0].Role != RoleUser {
		t.Fatalf("anchored task not first: %+v", r.Messages)
	}
}

// TestPackNoStaleAnchorWhenRecentUserSurvives: in a normal multi turn chat a
// recent user message already survives the walk, so Pack must NOT also drag the
// stale first message back in: anchoring is a userless window fallback, not an
// always pin. Guards the anchor against over reach.
func TestPackNoStaleAnchorWhenRecentUserSurvives(t *testing.T) {
	bigStale := strings.Repeat("y", 4*2000) // ~2000 token first task, evicted by the walk
	history := []Message{
		{Role: RoleUser, Content: "STALE FIRST TASK" + bigStale},
		{Role: RoleAssistant, Content: strings.Repeat("z", 4*500)},
		{Role: RoleUser, Content: "fresh recent task"},
		{Role: RoleAssistant, Content: "ok"},
	}
	// Budget holds the recent user + its surrounding small messages but not the
	// large stale first task, which the walk evicts and the anchor must NOT
	// drag back, because a recent user message already survives.
	r := Pack(history, 1000)
	for _, m := range r.Messages {
		if m.Content == "STALE FIRST TASK" {
			t.Fatalf("stale first task was anchored despite a recent user surviving: %+v", r.Messages)
		}
	}
	var sawRecent bool
	for _, m := range r.Messages {
		if m.Role == RoleUser && m.Content == "fresh recent task" {
			sawRecent = true
		}
	}
	if !sawRecent {
		t.Fatalf("recent user message missing: %+v", r.Messages)
	}
}

// TestPackDemotesSystemNudgeToUser: the four soft nudge backstops append a
// system role note to history. buildMessages prepends the embedded system prompt
// as wire element 0, so any system message Pack returns reaches the wire as a
// SECOND, non leading system message, which strict OpenAI compat backends reject
// outright ("System message must be at the beginning"; observed on strict
// backends like Ollama and llama.cpp). The only system content in history is a nudge, so Pack
// must demote it to a user message: the note (automated check prefix and all)
// stays in front of the model and the wire stays legal on every backend.
func TestPackDemotesSystemNudgeToUser(t *testing.T) {
	history := []Message{
		{Role: RoleUser, Content: "build it"},
		{Role: RoleAssistant, Content: "working"},
		{Role: RoleSystem, Content: "[Automated codehamr check: not a message from your user.] runaway self-check"},
	}
	r := Pack(history, 100000)
	for _, m := range r.Messages {
		if m.Role == RoleSystem {
			t.Fatalf("system message survived Pack: would 400 a strict backend: %+v", r.Messages)
		}
	}
	last := r.Messages[len(r.Messages)-1]
	if last.Role != RoleUser || !strings.Contains(last.Content, "runaway self-check") {
		t.Fatalf("nudge must be demoted to a user message with its content intact, got %+v", last)
	}
}

// TestPackAnchorsTaskEvenWithSystemNudge: a long single turn evicts the sole user
// task, and a nudge (system) is the newest message. The system→user demotion must
// run AFTER anchorUserMessage, otherwise the demoted nudge masquerades as a
// surviving user message, anchorUserMessage no operations, and the wire loses the
// original task. The real first user task must still be re anchored.
func TestPackAnchorsTaskEvenWithSystemNudge(t *testing.T) {
	big := strings.Repeat("x", 4*3000)
	history := []Message{
		{Role: RoleUser, Content: "BUILD THE GALAXY"},
		{Role: RoleAssistant, Content: big},
		{Role: RoleAssistant, Content: big},
		{Role: RoleSystem, Content: "[Automated codehamr check] runaway self-check"},
	}
	r := Pack(history, 5000)
	var sawTask bool
	for _, m := range r.Messages {
		if m.Role == RoleSystem {
			t.Fatalf("system nudge survived Pack: %+v", r.Messages)
		}
		if m.Role == RoleUser && m.Content == "BUILD THE GALAXY" {
			sawTask = true
		}
	}
	if !sawTask {
		t.Fatalf("original task must stay anchored even when a nudge is the newest message: %+v", r.Messages)
	}
}

func TestBudget(t *testing.T) {
	// Reference the constants directly so a future FixedSystem/FixedTools/headroom
	// tweak doesn't trip a magic number mismatch absent a real regression.
	// 65k: ctxSize/8 = 8192, just above the 8k floor.
	if raw := 65536 - FixedSystem - FixedTools - 8192; Budget(65536) != raw-raw/budgetHeadroomDivisor {
		t.Fatalf("budget wrong at 65k: %d", Budget(65536))
	}
	// 262k: ctxSize/8 = 32768, matches a common thinking mode default.
	if raw := 262144 - FixedSystem - FixedTools - 32768; Budget(262144) != raw-raw/budgetHeadroomDivisor {
		t.Fatalf("budget wrong at 262k: %d", Budget(262144))
	}
	if Budget(1000) != 0 {
		t.Fatal("budget must floor at 0")
	}
}

// TestResponseReserveScales pins the reserve curve: floored until ctxSize/8
// crosses 8k, then linear. A divisor tweak lands here loud.
func TestResponseReserveScales(t *testing.T) {
	cases := []struct {
		ctxSize int
		want    int
	}{
		{32_768, 8000},    // floor: ctxSize/8 = 4096 < 8000
		{64_000, 8000},    // floor: ctxSize/8 = 8000, not >
		{65_536, 8192},    // just above the floor
		{128_000, 16_000}, // linear
		{262_144, 32_768}, // common thinking mode default
		{1_000_000, 125_000},
	}
	for _, c := range cases {
		if got := ResponseReserve(c.ctxSize); got != c.want {
			t.Errorf("ResponseReserve(%d) = %d, want %d", c.ctxSize, got, c.want)
		}
	}
}
