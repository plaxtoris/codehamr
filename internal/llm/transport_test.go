package llm

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	chmctx "github.com/codehamr/codehamr/internal/ctx"
)

func TestAPIBasePaths(t *testing.T) {
	for _, tc := range []struct{ base, prefix string }{
		{"", "/v1"}, {"/", "/v1"}, {"/v1", "/v1"}, {"/v1/", "/v1"},
		{"/api", "/api/v1"}, {"/api/v1", "/api/v1"}, {"/api/v1/", "/api/v1"},
		{"/proxy/v1/", "/proxy/v1"},
	} {
		t.Run(tc.base, func(t *testing.T) {
			var gets, posts int
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					gets++
					if r.URL.Path != tc.prefix+"/models" {
						t.Errorf("models path = %q", r.URL.Path)
					}
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				posts++
				if r.Method != http.MethodPost || r.URL.Path != tc.prefix+"/responses" {
					t.Errorf("request = %s %s", r.Method, r.URL.Path)
				}
				if r.Header.Get("Authorization") != "Bearer testkey" {
					t.Error("missing bearer authentication")
				}
				sseOK(w, []string{textDelta("OK"), completed(1, 2)})
			}))
			defer srv.Close()
			c := New(srv.URL+tc.base, "provider/model", "testkey")
			c.RetryBackoff = nil
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := Reachable(ctx, c.BaseURL); err != nil {
				t.Fatal(err)
			}
			if err := c.Probe(ctx); err != nil {
				t.Fatal(err)
			}
			events := collect(c.Chat(ctx, []chmctx.Message{{Role: chmctx.RoleUser, Content: "hi"}}, nil))
			if len(events) == 0 || events[len(events)-1].Kind != EventDone {
				t.Fatalf("chat did not complete: %+v", events)
			}
			if gets != 1 || posts != 2 {
				t.Fatalf("requests = %d GET, %d POST", gets, posts)
			}
		})
	}
}

func TestProbeValidatesStream(t *testing.T) {
	for _, tc := range []struct{ name, event, want string }{
		{"success", completed(1, 1), ""},
		{"failure after HTTP success", `{"type":"response.failed","response":{"error":{"message":"model unavailable"}}}`, "model unavailable"},
		{"error frame", `{"error":{"message":"route unavailable"}}`, "route unavailable"},
		{"truncated response", textDelta("partial"), "without a completion event"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { sseOK(w, []string{tc.event}) }))
			defer srv.Close()
			err := New(srv.URL, "model", "key").Probe(context.Background())
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestProbeHonorsCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, ": waiting\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := New(srv.URL, "model", "key").Probe(ctx); err == nil {
		t.Fatal("cancelled probe succeeded")
	}
}

func TestReasoningFallbackRequiresBadRequest(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"error":{"message":"reasoning effort service unavailable"}}`)
	}))
	defer srv.Close()
	c := New(srv.URL, "model", "key")
	c.RetryBackoff = nil
	events := collect(c.Chat(context.Background(), nil, nil))
	if calls != 1 || c.noReasoning.Load() || len(events) != 1 || events[0].Kind != EventError {
		t.Fatal("a server failure must not disable reasoning or trigger its fallback")
	}
}
