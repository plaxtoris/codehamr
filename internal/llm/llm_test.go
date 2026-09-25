package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	chmctx "github.com/codehamr/codehamr/internal/ctx"
)

func collect(ch <-chan Event) []Event {
	var evs []Event
	for e := range ch {
		evs = append(evs, e)
	}
	return evs
}

// sseOK writes a Responses stream: each event as `event:`+`data:` the way
// OpenAI and vLLM emit it, closed by response.completed carrying the usage.
func sseOK(w http.ResponseWriter, events []string) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, e := range events {
		writeEvent(w, e)
	}
}

func writeEvent(w io.Writer, data string) {
	var ev struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal([]byte(data), &ev)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, data)
}

// Shorthands for the events the tests replay.
func textDelta(s string) string {
	return fmt.Sprintf(`{"type":"response.output_text.delta","output_index":0,"delta":%q}`, s)
}

func completed(outputTokens, inputTokens int) string {
	return fmt.Sprintf(`{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":%d,"output_tokens":%d}}}`, inputTokens, outputTokens)
}

func callAdded(idx int, callID, name string) string {
	return fmt.Sprintf(`{"type":"response.output_item.added","output_index":%d,"item":{"type":"function_call","id":"fc_%s","call_id":%q,"name":%q,"arguments":""}}`, idx, callID, callID, name)
}

func argsDelta(idx int, s string) string {
	return fmt.Sprintf(`{"type":"response.function_call_arguments.delta","output_index":%d,"delta":%q}`, idx, s)
}

func callDone(idx int, callID, name, args string) string {
	return fmt.Sprintf(`{"type":"response.output_item.done","output_index":%d,"item":{"type":"function_call","id":"fc_%s","call_id":%q,"name":%q,"arguments":%q}}`, idx, callID, callID, name, args)
}

// TestChatStreamsContent: text deltas merge into one final string; the request
// carries model, medium reasoning and stateless mode.
func TestChatStreamsContent(t *testing.T) {
	var gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		sseOK(w, []string{
			`{"type":"response.created","response":{"status":"in_progress"}}`,
			textDelta("Hel"),
			textDelta("lo"),
			completed(7, 0),
		})
	}))
	defer srv.Close()

	c := New(srv.URL, "test-model", "sk-xyz")
	events := collect(c.Chat(context.Background(),
		[]chmctx.Message{{Role: chmctx.RoleUser, Content: "hi"}}, nil))

	if gotAuth != "Bearer sk-xyz" {
		t.Fatalf("auth header missing: %q", gotAuth)
	}
	for _, want := range []string{`"model":"test-model"`, `"reasoning":{"effort":"medium"}`, `"store":false`, `"stream":true`,
		`{"type":"message","role":"user","content":"hi"}`} {
		if !strings.Contains(gotBody, want) {
			t.Fatalf("request missing %s: %s", want, gotBody)
		}
	}

	var content strings.Builder
	var sawDone bool
	for _, e := range events {
		switch e.Kind {
		case EventContent:
			content.WriteString(e.Content)
		case EventDone:
			sawDone = true
			if e.Final == nil || e.Final.Content != "Hello" {
				t.Errorf("final content wrong: %+v", e.Final)
			}
			if e.Tokens != 7 {
				t.Errorf("tokens = %d, want 7", e.Tokens)
			}
		}
	}
	if content.String() != "Hello" {
		t.Fatalf("content = %q", content.String())
	}
	if !sawDone {
		t.Fatal("no done event")
	}
}

// TestChatPostsToResponses pins the one endpoint the client speaks.
func TestChatPostsToResponses(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		sseOK(w, []string{completed(0, 0)})
	}))
	defer srv.Close()
	collect(New(srv.URL+"/", "m", "").Chat(context.Background(), nil, nil))
	if gotPath != "/v1/responses" {
		t.Fatalf("path = %q, want /v1/responses", gotPath)
	}
}

// TestChatToolCall: the canonical OpenAI shape: output_item.added, argument
// deltas, arguments.done, output_item.done: resolves to one EventToolCall and
// rides along in EventDone.Final.ToolCalls so the next round can replay it.
func TestChatToolCall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sseOK(w, []string{
			callAdded(0, "call_1", "bash"),
			argsDelta(0, `{"cmd"`),
			argsDelta(0, `:"ls"}`),
			`{"type":"response.function_call_arguments.done","output_index":0,"arguments":"{\"cmd\":\"ls\"}"}`,
			callDone(0, "call_1", "bash", `{"cmd":"ls"}`),
			completed(5, 0),
		})
	}))
	defer srv.Close()
	c := New(srv.URL, "m", "")
	var calls []chmctx.ToolCall
	var final *chmctx.Message
	for _, e := range collect(c.Chat(context.Background(), nil, nil)) {
		switch e.Kind {
		case EventToolCall:
			calls = append(calls, *e.ToolCall)
		case EventDone:
			final = e.Final
		}
	}
	if len(calls) != 1 || calls[0].Name != "bash" || calls[0].ID != "call_1" {
		t.Fatalf("want one bash call with call_id, got %+v", calls)
	}
	if cmd, _ := calls[0].Arguments["cmd"].(string); cmd != "ls" {
		t.Fatalf("tool args wrong (done must replace, not append to, the deltas): %+v", calls[0].Arguments)
	}
	if final == nil || len(final.ToolCalls) != 1 || final.ToolCalls[0].Name != "bash" {
		t.Fatalf("Final.ToolCalls should carry the bash call: %+v", final)
	}
}

// TestChatToolCallFromDoneOnly: a server that streams no argument deltas and
// no arguments.done (only the finished item) must still resolve the call.
func TestChatToolCallFromDoneOnly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sseOK(w, []string{
			callDone(0, "c1", "bash", `{"cmd":"ls"}`),
			completed(5, 0),
		})
	}))
	defer srv.Close()
	var got *chmctx.ToolCall
	for _, e := range collect(New(srv.URL, "m", "").Chat(context.Background(), nil, nil)) {
		if e.Kind == EventToolCall {
			got = e.ToolCall
		}
	}
	if got == nil || got.Name != "bash" || got.ID != "c1" {
		t.Fatalf("tool call missing: %+v", got)
	}
	if cmd, _ := got.Arguments["cmd"].(string); cmd != "ls" {
		t.Fatalf("args wrong: %+v", got.Arguments)
	}
}

// TestChatToolArgsStreamLive: each arguments delta is forwarded as EventToolArgs
// as it arrives, so the UI can tick its live token estimate while a file
// streams into write_file. Fragments concatenate to the full arguments and all
// precede the resolved EventToolCall.
func TestChatToolArgsStreamLive(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sseOK(w, []string{
			callAdded(0, "c1", "write_file"),
			argsDelta(0, `{"path":"a`),
			argsDelta(0, `.txt","content":"hi`),
			argsDelta(0, `"}`),
			callDone(0, "c1", "write_file", `{"path":"a.txt","content":"hi"}`),
			completed(0, 0),
		})
	}))
	defer srv.Close()
	var args strings.Builder
	sawCall, argsAllBeforeCall := false, true
	for _, e := range collect(New(srv.URL, "m", "").Chat(context.Background(), nil, nil)) {
		switch e.Kind {
		case EventToolArgs:
			args.WriteString(e.Content)
			if sawCall {
				argsAllBeforeCall = false
			}
		case EventToolCall:
			sawCall = true
		}
	}
	if got := args.String(); got != `{"path":"a.txt","content":"hi"}` {
		t.Fatalf("EventToolArgs fragments should concatenate to the full args, got %q", got)
	}
	if !sawCall || !argsAllBeforeCall {
		t.Fatalf("every EventToolArgs must precede the resolved EventToolCall (sawCall=%v)", sawCall)
	}
}

// TestChatParallelToolCallsByOutputIndex: two calls interleaved across frames
// route by output_index and resolve in output order.
func TestChatParallelToolCallsByOutputIndex(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sseOK(w, []string{
			callAdded(0, "c1", "read_file"),
			callAdded(1, "c2", "read_file"),
			argsDelta(0, `{"path":`),
			argsDelta(1, `{"path":`),
			argsDelta(0, `"a.go"}`),
			argsDelta(1, `"b.go"}`),
			callDone(0, "c1", "read_file", `{"path":"a.go"}`),
			callDone(1, "c2", "read_file", `{"path":"b.go"}`),
			completed(9, 0),
		})
	}))
	defer srv.Close()
	var calls []chmctx.ToolCall
	for _, e := range collect(New(srv.URL, "m", "").Chat(context.Background(), nil, nil)) {
		if e.Kind == EventToolCall {
			calls = append(calls, *e.ToolCall)
		}
	}
	if len(calls) != 2 {
		t.Fatalf("want 2 tool call events, got %d: %+v", len(calls), calls)
	}
	for i, want := range []string{"a.go", "b.go"} {
		if got, _ := calls[i].Arguments["path"].(string); got != want {
			t.Fatalf("call %d path = %q, want %q", i, got, want)
		}
	}
	if calls[0].ID != "c1" || calls[1].ID != "c2" {
		t.Fatalf("output order lost: got ids %s, %s", calls[0].ID, calls[1].ID)
	}
}

// TestChatToolCallMalformedArgsPreservesMarker: on invalid `arguments` JSON
// (provider bug), the client surfaces a sentinel key rather than an empty args
// map, so the tool result log names what went wrong.
func TestChatToolCallMalformedArgsPreservesMarker(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sseOK(w, []string{
			callDone(0, "c1", "bash", `{not-json`),
			completed(0, 0),
		})
	}))
	defer srv.Close()
	var got *chmctx.ToolCall
	for _, e := range collect(New(srv.URL, "m", "").Chat(context.Background(), nil, nil)) {
		if e.Kind == EventToolCall {
			got = e.ToolCall
		}
	}
	if got == nil {
		t.Fatal("tool call event missing")
	}
	if _, ok := got.Arguments["_parse_error"]; !ok {
		t.Fatalf("_parse_error sentinel missing: %+v", got.Arguments)
	}
}

// TestReasoningDeltasAreEmittedAndItemsKept: reasoning text streams as
// EventReasoning (else the UI freezes for the whole thinking phase) and must
// NOT fold into content; the finished reasoning ITEM is kept raw on the final
// message so the next request can replay it verbatim.
func TestReasoningDeltasAreEmittedAndItemsKept(t *testing.T) {
	item := `{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"gAAAA=="}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sseOK(w, []string{
			`{"type":"response.output_item.added","output_index":0,"item":` + item + `}`,
			`{"type":"response.reasoning_text.delta","output_index":0,"delta":"Hmm"}`,
			`{"type":"response.reasoning_summary_text.delta","output_index":0,"delta":" OK"}`,
			`{"type":"response.output_item.done","output_index":0,"item":` + item + `}`,
			textDelta("hi"),
			completed(3, 0),
		})
	}))
	defer srv.Close()

	var reasoning, content string
	var done Event
	for _, e := range collect(New(srv.URL, "m", "").Chat(context.Background(), nil, nil)) {
		switch e.Kind {
		case EventReasoning:
			reasoning += e.Content
		case EventContent:
			content += e.Content
		case EventDone:
			done = e
		}
	}
	if reasoning != "Hmm OK" {
		t.Fatalf("want reasoning %q, got %q", "Hmm OK", reasoning)
	}
	if content != "hi" || done.Final == nil || done.Final.Content != "hi" {
		t.Fatalf("reasoning must not leak into content: %q / %+v", content, done.Final)
	}
	if len(done.Final.Reasoning) != 1 || string(done.Final.Reasoning[0]) != item {
		t.Fatalf("finished reasoning item must be kept verbatim (from output_item.done only, once): %s", done.Final.Reasoning)
	}
	if done.Tokens != 3 {
		t.Fatalf("want 3 tokens, got %d", done.Tokens)
	}
}

// TestToInputShapes pins the wire mapping: system/user messages, an assistant
// round as reasoning items + text + function_call items, tool results as
// function_call_output with `output` always present (a silent bash command
// yields an empty string, and the field may not be omitted).
func TestToInputShapes(t *testing.T) {
	msgs := []chmctx.Message{
		{Role: chmctx.RoleSystem, Content: "be terse"},
		{Role: chmctx.RoleUser, Content: "run it"},
		{Role: chmctx.RoleAssistant, Content: "Running.", Reasoning: []json.RawMessage{json.RawMessage(`{"type":"reasoning","id":"rs_1"}`)},
			ToolCalls: []chmctx.ToolCall{{ID: "c1", Name: "bash", Arguments: map[string]any{"cmd": "true"}}}},
		{Role: chmctx.RoleTool, Content: "", ToolCallID: "c1", ToolName: "bash"},
	}
	buf, err := json.Marshal(toInput(msgs))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `[{"type":"message","role":"system","content":"be terse"},` +
		`{"type":"message","role":"user","content":"run it"},` +
		`{"type":"reasoning","id":"rs_1"},` +
		`{"type":"message","role":"assistant","content":"Running."},` +
		`{"type":"function_call","call_id":"c1","name":"bash","arguments":"{\"cmd\":\"true\"}"},` +
		`{"type":"function_call_output","call_id":"c1","output":""}]`
	if string(buf) != want {
		t.Fatalf("toInput =\n%s\nwant\n%s", buf, want)
	}
}

// TestToInputSkipsEmptyTextBesideCallsAndOrphanReasoning: an assistant round
// that only called tools sends no empty text item, and a round that produced
// nothing at all sends its (now orphaned) reasoning nowhere: a reasoning item
// with no following item is the one replay shape OpenAI rejects. The empty
// message itself still goes, so history stays truthful.
func TestToInputSkipsEmptyTextBesideCallsAndOrphanReasoning(t *testing.T) {
	r := []json.RawMessage{json.RawMessage(`{"type":"reasoning","id":"rs_1"}`)}
	buf, _ := json.Marshal(toInput([]chmctx.Message{
		{Role: chmctx.RoleAssistant, Reasoning: r, ToolCalls: []chmctx.ToolCall{{ID: "c1", Name: "bash", Arguments: map[string]any{}}}},
		{Role: chmctx.RoleAssistant, Reasoning: r},
	}))
	want := `[{"type":"reasoning","id":"rs_1"},` +
		`{"type":"function_call","call_id":"c1","name":"bash","arguments":"{}"},` +
		`{"type":"message","role":"assistant","content":""}]`
	if string(buf) != want {
		t.Fatalf("toInput =\n%s\nwant\n%s", buf, want)
	}
}

// TestToInputParseErrorArgsStayValidJSON: when resolve() stamps _parse_error
// for a truncated tool call and that assistant message round trips into the
// next request, the arguments must still be VALID JSON. Otherwise every later
// turn re sends corrupt JSON and the backend 400s forever (session poisoning).
func TestToInputParseErrorArgsStayValidJSON(t *testing.T) {
	items := toInput([]chmctx.Message{{
		Role: chmctx.RoleAssistant,
		ToolCalls: []chmctx.ToolCall{{
			ID:        "c1",
			Name:      "write_file",
			Arguments: map[string]any{"_parse_error": "unexpected end of JSON input"},
		}},
	}})
	args := items[0].(functionCallItem).Arguments
	if !json.Valid([]byte(args)) {
		t.Fatalf("arguments must stay valid JSON to avoid poisoning the session: %q", args)
	}
}

// TestChatToolsAreFlat: the Responses tool declaration has no `function`
// wrapper; the old chat completions nesting 400s here.
func TestChatToolsAreFlat(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		sseOK(w, []string{completed(0, 0)})
	}))
	defer srv.Close()
	collect(New(srv.URL, "m", "").Chat(context.Background(), nil, []Tool{{
		Type: "function", Name: "bash", Description: "run", Parameters: map[string]any{"type": "object"},
	}}))
	if !strings.Contains(gotBody, `"tools":[{"type":"function","name":"bash","description":"run","parameters":{"type":"object"}}]`) {
		t.Fatalf("tools must be flat: %s", gotBody)
	}
}

// TestChatMidStreamErrorFrameSurfacesAsError: OpenAI compatible proxies report
// a post 200 provider failure as a bare `data: {"error":{...}}` frame followed
// by connection close. That frame must surface as EventError; left undecoded
// the close reads as clean EOF and a truncated turn would be replayed or, worse,
// finalized.
func TestChatMidStreamErrorFrameSurfacesAsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeEvent(w, textDelta("partial answer"))
		fmt.Fprint(w, "data: {\"error\":{\"code\":502,\"message\":\"Provider returned error\"}}\n\n")
	}))
	defer srv.Close()

	events := collect(New(srv.URL, "m", "").Chat(context.Background(),
		[]chmctx.Message{{Role: chmctx.RoleUser, Content: "hi"}}, nil))

	last := events[len(events)-1]
	if last.Kind != EventError || !strings.Contains(last.Err.Error(), "Provider returned error") {
		t.Fatalf("stream must end in EventError carrying the server's message, got %+v", last)
	}
	if last.MidStream {
		t.Fatal("a server-reported error must never be replayed: it would repeat forever")
	}
	for _, e := range events {
		if e.Kind == EventDone {
			t.Fatal("a stream that died mid-generation must not emit EventDone")
		}
	}
}

// TestChatFailedAndErrorEventsAreServerErrors: the two Responses native
// failure events carry the server's own diagnosis and are not replayable.
func TestChatFailedAndErrorEventsAreServerErrors(t *testing.T) {
	for name, frame := range map[string]string{
		"response.failed": `{"type":"response.failed","response":{"status":"failed","error":{"code":"server_error","message":"context length exceeded"}}}`,
		"error":           `{"type":"error","code":"server_error","message":"context length exceeded"}`,
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				sseOK(w, []string{textDelta("partial"), frame})
			}))
			defer srv.Close()
			var errEvt *Event
			for _, e := range collect(New(srv.URL, "m", "").Chat(context.Background(), nil, nil)) {
				if e.Kind == EventError {
					errEvt = &e
				}
			}
			if errEvt == nil || !strings.Contains(errEvt.Err.Error(), "context length exceeded") {
				t.Fatalf("want EventError with the server's message, got %+v", errEvt)
			}
			if errEvt.MidStream {
				t.Fatal("a server-reported error must never be replayed")
			}
		})
	}
}

// TestChatReadsUsageTokens: tokens come from response.completed's usage
// (output_tokens; input_tokens rides along for the debug log calibration), not
// content length; we trust what the backend reports.
func TestChatReadsUsageTokens(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sseOK(w, []string{textDelta(strings.Repeat("x", 100)), completed(7, 42)})
	}))
	defer srv.Close()
	var tokens, promptTokens int
	for _, e := range collect(New(srv.URL, "m", "").Chat(context.Background(), nil, nil)) {
		if e.Kind == EventDone {
			tokens, promptTokens = e.Tokens, e.PromptTokens
		}
	}
	if tokens != 7 || promptTokens != 42 {
		t.Fatalf("want tokens=7 prompt=42 from usage, got %d/%d", tokens, promptTokens)
	}
}

// TestChatIncompleteCompletesStream: response.incomplete (max_output_tokens,
// content filter) is the model stopping on purpose; the partial text is a real
// answer, not a dropped socket.
func TestChatIncompleteCompletesStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sseOK(w, []string{
			textDelta("cut off"),
			`{"type":"response.incomplete","response":{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"usage":{"output_tokens":2}}}`,
		})
	}))
	defer srv.Close()
	events := collect(New(srv.URL, "m", "").Chat(context.Background(), nil, nil))
	last := events[len(events)-1]
	if last.Kind != EventDone || last.Final == nil || last.Final.Content != "cut off" || last.Tokens != 2 {
		t.Fatalf("incomplete is a completion signal; want EventDone with the text, got %+v", last)
	}
}

// TestSendEventUnblocksOnCancel pins sendEvent's anti wedge invariant: once the
// parent context is cancelled, a send to an undrained channel must abort via the
// <-parent.Done() arm instead of blocking the stream goroutine forever.
func TestSendEventUnblocksOnCancel(t *testing.T) {
	out := make(chan Event) // unbuffered, no reader → the send blocks until cancel
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool, 1)
	go func() {
		done <- sendEvent(ctx, out, Event{Kind: EventContent, Content: "nobody is reading me"})
	}()
	cancel()
	select {
	case ok := <-done:
		if ok {
			t.Fatal("sendEvent returned true for a send nobody drained; it must report false after cancel")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("sendEvent wedged on an undrained channel after cancel: the anti-wedge <-parent.Done() arm is missing")
	}
}

// TestChat401: maps to typed ErrUnauthorized.
func TestChat401(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	evs := collect(New(srv.URL, "m", "").Chat(context.Background(), nil, nil))
	if len(evs) != 1 || !errors.Is(evs[0].Err, ErrUnauthorized) {
		t.Fatalf("want ErrUnauthorized, got %+v", evs)
	}
}

// TestChat401DrainsBodyForConnReuse: a 401 carrying a body must have that body
// drained before close, or Go's transport discards the TCP connection instead
// of returning it to the keep alive pool. Two sequential 401s on one client
// must land on one connection.
func TestChat401DrainsBodyForConnReuse(t *testing.T) {
	var mu sync.Mutex
	conns := map[string]bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		conns[r.RemoteAddr] = true
		mu.Unlock()
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":{"message":"invalid api key"}}`)
	}))
	defer srv.Close()

	c := New(srv.URL, "m", "")
	for i := 0; i < 2; i++ {
		evs := collect(c.Chat(context.Background(), nil, nil))
		if len(evs) != 1 || !errors.Is(evs[0].Err, ErrUnauthorized) {
			t.Fatalf("request %d: want ErrUnauthorized, got %+v", i, evs)
		}
	}
	mu.Lock()
	n := len(conns)
	mu.Unlock()
	if n != 1 {
		t.Fatalf("401 body not drained: server saw %d connections across 2 sequential requests, want 1", n)
	}
}

// TestChat402 preserves the provider message and does not retry billing errors.
func TestChat402(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusPaymentRequired)
		fmt.Fprint(w, `{"error":{"message":"Insufficient credits"}}`)
	}))
	defer srv.Close()
	evs := collect(New(srv.URL, "m", "k").Chat(context.Background(), nil, nil))
	if len(evs) != 1 || evs[0].Err == nil || evs[0].Err.Error() != "402: Insufficient credits" {
		t.Fatalf("expected provider billing error, got %+v", evs)
	}

}

// TestChatUnreachable: transport failure surfaces as ErrUnreachable.
func TestChatUnreachable(t *testing.T) {
	c := New("http://127.0.0.1:1", "m", "")
	c.RetryBackoff = nil
	evs := collect(c.Chat(context.Background(), nil, nil))
	if len(evs) != 1 {
		t.Fatalf("want single event, got %d", len(evs))
	}
	var un ErrUnreachable
	if !errors.As(evs[0].Err, &un) {
		t.Fatalf("want ErrUnreachable, got %v", evs[0].Err)
	}
}

// TestChatOtherHTTPError: non 2xx (not 401/402) surfaces as a generic error
// carrying only the first body line.
func TestChatOtherHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintln(w, "engine exploded")
		fmt.Fprintln(w, "see logs")
	}))
	defer srv.Close()
	c := New(srv.URL, "m", "")
	c.RetryBackoff = nil
	evs := collect(c.Chat(context.Background(), nil, nil))
	if len(evs) != 1 || evs[0].Kind != EventError {
		t.Fatalf("want single error event, got %+v", evs)
	}
	if !strings.Contains(evs[0].Err.Error(), "500") || !strings.Contains(evs[0].Err.Error(), "engine exploded") {
		t.Fatalf("error should include status and body excerpt: %v", evs[0].Err)
	}
	if strings.Contains(evs[0].Err.Error(), "see logs") {
		t.Fatalf("error should include only first body line: %v", evs[0].Err)
	}
}

// TestChat404NamesTheRequirement: a route miss is the one misconfiguration the
// body never explains (a chat completions only server says just "not found"),
// so the error names the Responses API and the server versions that ship it.
// vLLM also 404s an unknown model; its message must survive in front.
func TestChat404NamesTheRequirement(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, "{\"error\":{\"message\":\"The model `nope` does not exist.\",\"code\":404}}")
	}))
	defer srv.Close()
	err := New(srv.URL, "nope", "").Probe(context.Background())
	if err == nil {
		t.Fatal("404 must fail the probe")
	}
	for _, want := range []string{"404", "The model `nope` does not exist.", "Responses API", "API base URL"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("404 error should mention %q: %v", want, err)
		}
	}
}

func TestLiteLLM404NamesTheBridge(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":{"message":"litellm.NotFoundError: OpenAIException - 404: Not Found"}}`)
	}))
	defer srv.Close()
	err := New(srv.URL, "local-model", "").Probe(context.Background())
	if err == nil {
		t.Fatal("404 must fail the probe")
	}
	for _, want := range []string{"404", "litellm.NotFoundError", "upstream URL and model", "use_chat_completions_api: true"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "Ollama 0.13.3") {
		t.Errorf("proxy error must not prescribe a local server upgrade: %v", err)
	}
}

// TestChatStructuredErrorFallsBackToMessage: with only `error.message`, surface
// that, not the raw JSON envelope.
func TestChatStructuredErrorFallsBackToMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `{"error":{"message":"upstream unavailable","type":"upstream_unavailable","upstream_status":503}}`)
	}))
	defer srv.Close()
	c := New(srv.URL, "m", "")
	c.RetryBackoff = nil
	evs := collect(c.Chat(context.Background(), nil, nil))
	if len(evs) != 1 || evs[0].Kind != EventError {
		t.Fatalf("want single error event, got %+v", evs)
	}
	msg := evs[0].Err.Error()
	if !strings.Contains(msg, "503") || !strings.Contains(msg, "upstream unavailable") || strings.Contains(msg, `{"error"`) {
		t.Fatalf("error should carry status and message, never the raw envelope: %v", msg)
	}
}

// TestChatFallsBackWhenReasoningRejected: each dialect's refusal of the
// reasoning effort: Ollama's non thinking model, vLLM's scale without our
// value, OpenAI's non reasoning model: drops the field, retries once, and
// stays sticky for the Client's life so later turns don't burn a 400 each.
func TestChatFallsBackWhenReasoningRejected(t *testing.T) {
	for name, body := range map[string]string{
		"ollama": `{"error":"\"test-model:latest\" does not support thinking"}`,
		"vllm":   `{"error":{"message":"Unexpected reasoning effort medium. Supported types are xhigh (default), high, and low.","type":"BadRequestError","param":null,"code":400}}`,
		"openai": `{"error":{"message":"Unsupported parameter: 'reasoning.effort' is not supported with this model.","type":"invalid_request_error","param":"reasoning.effort","code":"unsupported_parameter"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			var bodies []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				bodies = append(bodies, string(b))
				if strings.Contains(string(b), `"reasoning"`) {
					w.WriteHeader(400)
					fmt.Fprintln(w, body)
					return
				}
				sseOK(w, []string{textDelta("ok"), completed(1, 0)})
			}))
			defer srv.Close()

			c := New(srv.URL, "test-model", "")
			for _, e := range collect(c.Chat(context.Background(), nil, nil)) {
				if e.Kind == EventError {
					t.Fatalf("first turn must succeed via fallback, got error: %v", e.Err)
				}
			}
			if len(bodies) != 2 || !strings.Contains(bodies[0], `"reasoning"`) || strings.Contains(bodies[1], `"reasoning"`) {
				t.Fatalf("first turn should send reasoning, then retry without it: %v", bodies)
			}
			bodies = nil
			for _, e := range collect(c.Chat(context.Background(), nil, nil)) {
				if e.Kind == EventError {
					t.Fatalf("second turn must not error: %v", e.Err)
				}
			}
			if len(bodies) != 1 || strings.Contains(bodies[0], `"reasoning"`) {
				t.Fatalf("second turn should make exactly 1 request without reasoning: %v", bodies)
			}
		})
	}
}

// TestChatDoesNotFallBackOnUnrelatedThinking: a 400 that is NOT about reasoning
// but happens to mention "thinking" must not trip the fallback. Otherwise the
// match burns a wasted retry and latches reasoning off for the Client's whole
// life on an error that had nothing to do with it.
func TestChatDoesNotFallBackOnUnrelatedThinking(t *testing.T) {
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		w.WriteHeader(400)
		fmt.Fprintln(w, `{"error":{"message":"the requested tool format is not supported","details":"thinking about it differently won't help"}}`)
	}))
	defer srv.Close()

	c := New(srv.URL, "some-model", "")
	collect(c.Chat(context.Background(), nil, nil))
	if len(bodies) != 1 {
		t.Fatalf("unrelated 400 must NOT trigger a fallback retry; got %d requests", len(bodies))
	}
	if c.noReasoning.Load() {
		t.Fatal("reasoning must not latch off on a 400 unrelated to reasoning")
	}
}

// TestProbeChatNoReasoningIsRaceFree pins the atomic guard on
// Client.noReasoning. The startup probe and the first chat can run on the same
// *Client concurrently: both read the flag via post, and a 400 fallback writes
// it. A plain bool would be a data race; this must run clean under -race.
func TestProbeChatNoReasoningIsRaceFree(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if strings.Contains(string(b), `"reasoning"`) {
			w.WriteHeader(400)
			fmt.Fprint(w, `{"error":{"message":"Unsupported parameter: 'reasoning.effort' is not supported with this model."}}`)
			return
		}
		sseOK(w, []string{textDelta("ok"), completed(1, 0)})
	}))
	defer srv.Close()

	c := New(srv.URL, "m", "")
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = c.Probe(context.Background())
		}()
		go func() {
			defer wg.Done()
			collect(c.Chat(context.Background(), nil, nil))
		}()
	}
	wg.Wait()
}

// TestProbeSendsMinimalRequest: the probe is a hello with no tools and no
// reasoning, and its output is capped to keep activation inexpensive.
func TestProbeSendsMinimalRequest(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		sseOK(w, []string{completed(0, 0)})
	}))
	defer srv.Close()
	err := New(srv.URL, "m", "k").Probe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(gotBody, `"reasoning"`) || strings.Contains(gotBody, `"tools"`) {
		t.Fatalf("probe must send neither reasoning nor tools: %s", gotBody)
	}
	if !strings.Contains(gotBody, `"max_output_tokens":16`) {
		t.Fatal("probe must cap output tokens")
	}

}

// TestNewHasNoHTTPTimeout pins that the streaming Client must NOT set
// http.Client.Timeout: that field is end to end (it covers body reads) and would
// abort a legitimately slow SSE stream. Per turn context cancellation governs
// request lifetime.
func TestNewHasNoHTTPTimeout(t *testing.T) {
	c := New("http://example.test", "model", "token")
	if c.HTTP.Timeout != 0 {
		t.Fatalf("http.Client.Timeout must be 0 so per-turn context governs SSE lifetime; got %v", c.HTTP.Timeout)
	}
}

// TestIdleTimeoutFromEnv pins the CODEHAMR_IDLE_TIMEOUT contract: a Go duration
// or bare seconds string wins, anything else (unset, garbage, nonpositive)
// falls back to the default.
func TestIdleTimeoutFromEnv(t *testing.T) {
	cases := []struct {
		val  string
		set  bool
		want time.Duration
	}{
		{set: false, want: streamIdleTimeout},
		{val: "", set: true, want: streamIdleTimeout},
		{val: "90m", set: true, want: 90 * time.Minute},
		{val: "1h30m", set: true, want: 90 * time.Minute},
		{val: "300", set: true, want: 300 * time.Second},
		{val: "garbage", set: true, want: streamIdleTimeout},
		{val: "0", set: true, want: streamIdleTimeout},
		{val: "-5m", set: true, want: streamIdleTimeout},
		// Bare seconds large enough to wrap the ×time.Second multiply to a
		// small POSITIVE duration must fall back, not silently kill every
		// live but slow stream mid prefill.
		{val: "18446744074", set: true, want: streamIdleTimeout},
		{val: "9000000000", set: true, want: 9_000_000_000 * time.Second},
	}
	for _, tc := range cases {
		if tc.set {
			t.Setenv("CODEHAMR_IDLE_TIMEOUT", tc.val)
		} else {
			os.Unsetenv("CODEHAMR_IDLE_TIMEOUT")
		}
		if got := idleTimeoutFromEnv(); got != tc.want {
			t.Errorf("idleTimeoutFromEnv(%q, set=%v) = %v, want %v", tc.val, tc.set, got, tc.want)
		}
	}
}

// TestChatIdleTimeoutAbortsStalledStream reproduces the exact hang: the server
// returns 200 OK then sends nothing. Without the idle watchdog scanner.Scan()
// blocks forever; with it, the body is closed and the turn ends in an EventError
// naming the stall.
func TestChatIdleTimeoutAbortsStalledStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()

	c := New(srv.URL, "m", "")
	c.IdleTimeout = 60 * time.Millisecond

	start := time.Now()
	var gotErr error
	for _, e := range collect(c.Chat(context.Background(), nil, nil)) {
		if e.Kind == EventError {
			gotErr = e.Err
		}
	}
	if gotErr == nil || !strings.Contains(gotErr.Error(), "stopped sending") {
		t.Fatalf("expected an EventError naming the stall, got %v", gotErr)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("watchdog fired too late (%v): should be ~IdleTimeout", elapsed)
	}
}

// TestChatIdleWatchdogResetByFrames pins that an alive but slow stream is NOT
// aborted: frames spaced under the idle window each reset the watchdog, so a
// stream whose total span exceeds one window still completes.
func TestChatIdleWatchdogResetByFrames(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flush := w.(http.Flusher)
		for _, s := range []string{"A", "B"} {
			writeEvent(w, textDelta(s))
			flush.Flush()
			time.Sleep(250 * time.Millisecond) // < IdleTimeout, so the watchdog resets
		}
		writeEvent(w, completed(0, 0))
		flush.Flush()
	}))
	defer srv.Close()

	c := New(srv.URL, "m", "")
	c.IdleTimeout = 400 * time.Millisecond // > each 250ms gap, < ~500ms total span

	var content strings.Builder
	for _, e := range collect(c.Chat(context.Background(), nil, nil)) {
		switch e.Kind {
		case EventContent:
			content.WriteString(e.Content)
		case EventError:
			t.Fatalf("watchdog aborted a live stream: %v", e.Err)
		}
	}
	if content.String() != "AB" {
		t.Fatalf("content = %q, want AB (slow-but-alive stream must complete)", content.String())
	}
}

// TestChatRetriesTransient404: a proxy that hiccups (two 404s, then a clean
// stream) is retried transparently. The user sees one EventRetry per wait,
// then normal content: never an EventError.
func TestChatRetriesTransient404(t *testing.T) {
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		if attempts <= 2 {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"detail":"Not Found"}`)
			return
		}
		sseOK(w, []string{textDelta("ok"), completed(0, 0)})
	}))
	defer srv.Close()

	c := New(srv.URL, "m", "")
	c.RetryBackoff = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	evs := collect(c.Chat(context.Background(),
		[]chmctx.Message{{Role: chmctx.RoleUser, Content: "hi"}}, nil))

	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3 (two failures + one success)", attempts)
	}
	var retries int
	var sawDone bool
	for _, e := range evs {
		switch e.Kind {
		case EventRetry:
			retries++
			if e.Content == "" || e.Err == nil {
				t.Errorf("retry event must carry a status hint and the trigger error: %+v", e)
			}
		case EventDone:
			sawDone = true
		case EventError:
			t.Fatalf("recovered turn must not surface an error: %v", e.Err)
		}
	}
	if retries != 2 || !sawDone {
		t.Fatalf("retry events = %d (want 2), done = %v", retries, sawDone)
	}
}

// TestChatRetryGivesUpAfterBackoffExhausted: a permanently failing backend gets
// len(RetryBackoff) retries, then the last error surfaces.
func TestChatRetryGivesUpAfterBackoffExhausted(t *testing.T) {
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, "down")
	}))
	defer srv.Close()

	c := New(srv.URL, "m", "")
	c.RetryBackoff = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	evs := collect(c.Chat(context.Background(), nil, nil))

	if attempts != 4 {
		t.Fatalf("attempts = %d, want 4 (initial + 3 retries)", attempts)
	}
	last := evs[len(evs)-1]
	if last.Kind != EventError || !strings.Contains(last.Err.Error(), "503") {
		t.Fatalf("want final EventError carrying the 503, got %+v", last)
	}
}

// TestChatDoesNotRetryPermanentErrors: auth (401), forbidden (403), and
// malformed request (400) fail identically on every resend; exactly one
// attempt, straight to EventError.
func TestChatDoesNotRetryPermanentErrors(t *testing.T) {
	for _, status := range []int{400, 401, 403} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			attempts := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				attempts++
				w.WriteHeader(status)
			}))
			defer srv.Close()

			c := New(srv.URL, "m", "")
			c.RetryBackoff = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
			evs := collect(c.Chat(context.Background(), nil, nil))
			if attempts != 1 {
				t.Fatalf("attempts = %d, want 1 (no retry on %d)", attempts, status)
			}
			if len(evs) != 1 || evs[0].Kind != EventError {
				t.Fatalf("want single error event, got %+v", evs)
			}
		})
	}
}

// TestChatRetryWaitCancelable: Ctrl+C during a backoff wait must unwind the
// turn immediately, not after the remaining sleep.
func TestChatRetryWaitCancelable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c := New(srv.URL, "m", "")
	c.RetryBackoff = []time.Duration{time.Minute}
	ctx, cancel := context.WithCancel(context.Background())
	ch := c.Chat(ctx, nil, nil)
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	collect(ch)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("retry wait ignored cancellation: channel closed after %s", elapsed)
	}
}

// TestRetryableClassification: network errors and transient statuses retry;
// typed sentinels (401/402), client errors, and unknown errors do not. 404 is
// deliberately in the retry set (LiteLLM proxies emit it transiently, #7).
func TestRetryableClassification(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"unreachable", ErrUnreachable{Err: errors.New("refused")}, true},
		{"404", &httpStatusError{status: 404, msg: "not found"}, true},
		{"408", &httpStatusError{status: 408, msg: "timeout"}, true},
		{"429", &httpStatusError{status: 429, msg: "rate limited"}, true},
		{"500", &httpStatusError{status: 500, msg: "boom"}, true},
		{"503", &httpStatusError{status: 503, msg: "down"}, true},
		{"400", &httpStatusError{status: 400, msg: "bad request"}, false},
		{"403", &httpStatusError{status: 403, msg: "forbidden"}, false},
		{"unauthorized", ErrUnauthorized, false},
		{"payment required", &httpStatusError{status: 402, msg: "Insufficient credits"}, false},
		{"misc", errors.New("something"), false},
	}
	for _, tc := range cases {
		if got := retryable(tc.err); got != tc.want {
			t.Errorf("retryable(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestProbeDoesNotRetry: the startup probe exists for fast feedback on a
// misconfigured URL/model/key; it must fail on the first response.
func TestProbeDoesNotRetry(t *testing.T) {
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := New(srv.URL, "m", "")
	if err := c.Probe(context.Background()); err == nil {
		t.Fatal("probe against a 404 backend must fail")
	}
	if attempts != 1 {
		t.Fatalf("probe attempts = %d, want 1", attempts)
	}
}

// TestChatMidStreamDropIsReplayable: a socket dropped after a delivered frame
// is a transport failure the TUI may replay.
func TestChatMidStreamDropIsReplayable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		writeEvent(w, textDelta("partial"))
		w.(http.Flusher).Flush()
		conn, _, _ := w.(http.Hijacker).Hijack()
		conn.Close() // drop the socket mid stream
	}))
	defer srv.Close()
	var errEvt *Event
	for _, e := range collect(New(srv.URL, "m", "").Chat(context.Background(), nil, nil)) {
		if e.Kind == EventError {
			errEvt = &e
		}
	}
	if errEvt == nil || !errEvt.MidStream {
		t.Fatalf("a drop after a delivered frame must be replayable: %+v", errEvt)
	}
}

// TestKeepaliveDoesNotCollapseThePrefillWindow: a proxy that emits one comment
// or blank line at 200 OK is liveness, not output. Treating it as output would
// swap the long prefill window for the short inter frame one while the model is
// still prefilling, and mark the resulting DETERMINISTIC prefill stall as a
// replayable drop. An `event:` name line without its data is liveness too.
func TestKeepaliveDoesNotCollapseThePrefillWindow(t *testing.T) {
	for _, tc := range []struct {
		name     string
		preamble string
		want     bool
	}{
		{"keepalive comment then silence", ": OPENROUTER PROCESSING\n\n", false},
		{"blank line then silence", "\n", false},
		{"event name then silence", "event: response.created\n", false},
		{"real data frame then silence", "data: " + textDelta("hi") + "\n\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				fmt.Fprint(w, tc.preamble)
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			}))
			defer srv.Close()
			c := New(srv.URL, "m", "")
			c.IdleTimeout = 300 * time.Millisecond
			var errEvt *Event
			for _, e := range collect(c.Chat(context.Background(), nil, nil)) {
				if e.Kind == EventError {
					errEvt = &e
				}
			}
			if errEvt == nil {
				t.Fatal("expected a stall error")
			}
			if errEvt.MidStream != tc.want {
				t.Fatalf("MidStream = %v, want %v (only a data frame means the model started producing): %v",
					errEvt.MidStream, tc.want, errEvt.Err)
			}
		})
	}
}

// TestChatCleanEOFWithoutCompletionIsMidStreamDrop: a proxy/LB that gracefully
// closes the upstream mid generation produces a clean EOF with no
// response.completed. That must surface as a MidStream EventError (so the TUI's
// bounded replay reissues the request), never as EventDone: finalizing it
// hands the turn a mid sentence truncated assistant message as a clean finish.
func TestChatCleanEOFWithoutCompletionIsMidStreamDrop(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeEvent(w, textDelta("partial ans"))
	}))
	defer srv.Close()

	events := collect(New(srv.URL, "m", "").Chat(context.Background(),
		[]chmctx.Message{{Role: chmctx.RoleUser, Content: "hi"}}, nil))

	last := events[len(events)-1]
	if last.Kind != EventError || !last.MidStream {
		t.Fatalf("completion-less EOF must end in a replayable EventError, got %+v", last)
	}
	for _, e := range events {
		if e.Kind == EventDone {
			t.Fatal("a cut stream must not emit EventDone")
		}
	}
}

// TestChatIgnoresLifecycleChatterAndStrayDone: response.created, in_progress,
// content_part.*, reasoning_part.* and a translation proxy's trailing
// `data: [DONE]` carry nothing we act on and must neither error nor leak into
// content.
func TestChatIgnoresLifecycleChatterAndStrayDone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sseOK(w, []string{
			`{"type":"response.created","response":{"status":"in_progress"}}`,
			`{"type":"response.in_progress","response":{"status":"in_progress"}}`,
			`{"type":"response.output_item.added","output_index":0,"item":{"type":"message","role":"assistant","content":[]}}`,
			`{"type":"response.content_part.added","output_index":0,"part":{"type":"output_text","text":""}}`,
			textDelta("ok"),
			`{"type":"response.output_text.done","output_index":0,"text":"ok"}`,
			`{"type":"response.content_part.done","output_index":0}`,
			`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}}`,
			completed(1, 0),
		})
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	events := collect(New(srv.URL, "m", "").Chat(context.Background(), nil, nil))
	last := events[len(events)-1]
	if last.Kind != EventDone || last.Final == nil || last.Final.Content != "ok" || len(last.Final.ToolCalls) != 0 || len(last.Final.Reasoning) != 0 {
		t.Fatalf("want a clean EventDone with content ok and nothing else, got %+v", last)
	}
}
