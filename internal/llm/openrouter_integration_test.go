package llm

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/codehamr/codehamr/internal/config"
	chmctx "github.com/codehamr/codehamr/internal/ctx"
)

// This test makes real API requests only when explicitly enabled with a
// project directory containing an openrouter profile. Credentials stay local.
func TestOpenRouterIntegration(t *testing.T) {
	root := os.Getenv("CODEHAMR_TEST_OPENROUTER_PROJECT")
	if root == "" {
		t.Skip("set CODEHAMR_TEST_OPENROUTER_PROJECT to enable live API testing")
	}
	cfg, _, err := config.Bootstrap(root)
	if err != nil {
		t.Fatal("could not load the integration configuration")
	}
	p, ok := cfg.Models["openrouter"]
	if !ok || p.ResolvedKey() == "" {
		t.Fatal("an openrouter profile with an API key is required")
	}
	c := New(p.URL, p.LLM, p.ResolvedKey())
	c.RetryBackoff = nil
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := c.Probe(ctx); err != nil {
		t.Fatalf("OpenRouter probe: %v", err)
	}
	history := []chmctx.Message{
		{Role: chmctx.RoleSystem, Content: "You are testing a connection. Call connection_check exactly once with no arguments, then reply with only the tool result. Keep your answer short."},
		{Role: chmctx.RoleUser, Content: "Run the connection check."},
	}
	tools := []Tool{{Type: "function", Name: "connection_check", Description: "Return the connection check result.", Parameters: map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}}}
	var first *chmctx.Message
	var calls int
	for e := range c.Chat(ctx, history, tools) {
		if e.Err != nil {
			t.Fatalf("OpenRouter tool request: %v", e.Err)
		}
		if e.Kind == EventToolCall {
			calls++
		}
		if e.Kind == EventDone {
			first = e.Final
		}
	}
	if first == nil || len(first.ToolCalls) != 1 || calls != 1 || first.ToolCalls[0].Name != "connection_check" || first.ToolCalls[0].ID == "" || len(first.ToolCalls[0].Arguments) != 0 {
		t.Fatal("OpenRouter did not return the requested structured tool call")
	}
	// Only the tool result contains this marker. Its presence in the answer
	// verifies replay while allowing the model to add ordinary prose.
	marker := fmt.Sprintf("CONNECTION_OK_%d", time.Now().UnixNano())
	history = append(history, *first, chmctx.Message{Role: chmctx.RoleTool, ToolCallID: first.ToolCalls[0].ID, ToolName: "connection_check", Content: marker})
	var final *chmctx.Message
	var streamed bool
	for e := range c.Chat(ctx, history, tools) {
		if e.Err != nil {
			t.Fatalf("OpenRouter tool result replay: %v", e.Err)
		}
		if e.Kind == EventContent && e.Content != "" {
			streamed = true
		}
		if e.Kind == EventDone {
			final = e.Final
		}
	}
	if final == nil || !strings.Contains(final.Content, marker) || len(final.ToolCalls) != 0 || !streamed {
		t.Fatalf("OpenRouter answer mismatch: streamed=%v, final=%+v", streamed, final)
	}
}
