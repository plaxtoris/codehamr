package llm

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
)

// ErrUnauthorized identifies an HTTP 401 response.
var ErrUnauthorized = errors.New("invalid or expired API key")

// ErrUnreachable distinguishes transport failures from API responses.
type ErrUnreachable struct{ Err error }

func (e ErrUnreachable) Error() string { return "backend unreachable: " + e.Err.Error() }
func (e ErrUnreachable) Unwrap() error { return e.Err }

// apiURL accepts a server root or an API base ending in /v1. Keeping the
// prefix supports providers such as OpenRouter at /api/v1.
func apiURL(base, resource string) string {
	base = strings.TrimRight(base, "/")
	if !strings.HasSuffix(base, "/v1") {
		base += "/v1"
	}
	return base + "/" + resource
}

// Reachable checks transport connectivity without starting model inference.
// Any HTTP response counts as reachable, including authentication failures.
func Reachable(ctx context.Context, base string) error {
	req, err := http.NewRequestWithContext(ctx, "GET", apiURL(base, "models"), nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}
