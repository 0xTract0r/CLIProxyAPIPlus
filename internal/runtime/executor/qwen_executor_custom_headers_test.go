package executor

import (
	"net/http"
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// TestApplyQwenHeaders_AppliesCustomHeaders verifies that applyQwenHeaders
// honors account-defined custom headers stored under the `header:`
// attribute namespace, mirroring the Codex / Claude / Kimi / Kilo executor
// pattern via util.ApplyCustomHeadersFromAttrs.
func TestApplyQwenHeaders_AppliesCustomHeaders(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://qwen.example/chat/completions", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	auth := &cliproxyauth.Auth{
		Provider: "qwen",
		Attributes: map[string]string{
			"header:X-Custom-Foo":         "bar",
			"header:X-Forwarded-Identity": "alice@example.com",
		},
	}

	applyQwenHeaders(req, "fake_token", false, auth)

	if got := req.Header.Get("X-Custom-Foo"); got != "bar" {
		t.Fatalf("expected X-Custom-Foo=bar, got %q", got)
	}
	if got := req.Header.Get("X-Forwarded-Identity"); got != "alice@example.com" {
		t.Fatalf("expected X-Forwarded-Identity=alice@example.com, got %q", got)
	}
}

// TestApplyQwenHeaders_NilAuthDoesNotPanic guards the legacy code path
// where auth context is unavailable and ensures the helper still produces
// the hard-coded provider headers.
func TestApplyQwenHeaders_NilAuthDoesNotPanic(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://qwen.example/chat/completions", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	applyQwenHeaders(req, "fake_token", true, nil)
	if got := req.Header.Get("Authorization"); got != "Bearer fake_token" {
		t.Fatalf("expected Authorization=Bearer fake_token, got %q", got)
	}
	if got := req.Header.Get("Accept"); got != "text/event-stream" {
		t.Fatalf("expected Accept=text/event-stream, got %q", got)
	}
}

func TestQwenPrepareRequest_AppliesCustomHeaders(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "https://portal.qwen.ai/v1/models", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	auth := &cliproxyauth.Auth{
		Provider: "qwen",
		Attributes: map[string]string{
			"api_key":              "fake_token",
			"header:X-Custom-Foo":  "bar",
			"header:X-Account-Tag": "qwen-a",
		},
	}

	if err := NewQwenExecutor(nil).PrepareRequest(req, auth); err != nil {
		t.Fatalf("prepare request: %v", err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer fake_token" {
		t.Fatalf("expected Authorization=Bearer fake_token, got %q", got)
	}
	if got := req.Header.Get("X-Custom-Foo"); got != "bar" {
		t.Fatalf("expected X-Custom-Foo=bar, got %q", got)
	}
	if got := req.Header.Get("X-Account-Tag"); got != "qwen-a" {
		t.Fatalf("expected X-Account-Tag=qwen-a, got %q", got)
	}
}
