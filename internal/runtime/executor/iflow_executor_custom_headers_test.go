package executor

import (
	"net/http"
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// TestApplyIFlowHeaders_AppliesCustomHeaders verifies that
// applyIFlowHeaders honors account-defined custom headers stored under the
// `header:` attribute namespace, mirroring the behavior already in place
// for Codex / Claude / Kimi / Kilo executors via
// util.ApplyCustomHeadersFromAttrs.
func TestApplyIFlowHeaders_AppliesCustomHeaders(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://iflow.example/chat/completions", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	auth := &cliproxyauth.Auth{
		Provider: "iflow",
		Attributes: map[string]string{
			"header:X-Custom-Foo":         "bar",
			"header:X-Forwarded-Identity": "alice@example.com",
		},
	}

	applyIFlowHeaders(req, "fake_api_key", false, auth)

	if got := req.Header.Get("X-Custom-Foo"); got != "bar" {
		t.Fatalf("expected X-Custom-Foo=bar, got %q", got)
	}
	if got := req.Header.Get("X-Forwarded-Identity"); got != "alice@example.com" {
		t.Fatalf("expected X-Forwarded-Identity=alice@example.com, got %q", got)
	}
}

// TestApplyIFlowHeaders_NilAuthDoesNotPanic guards the legacy code path
// where auth context is unavailable (e.g. preflight builds) and ensures
// the helper keeps producing the hard-coded provider headers.
func TestApplyIFlowHeaders_NilAuthDoesNotPanic(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://iflow.example/chat/completions", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	applyIFlowHeaders(req, "fake_api_key", true, nil)
	if got := req.Header.Get("Authorization"); got != "Bearer fake_api_key" {
		t.Fatalf("expected Authorization=Bearer fake_api_key, got %q", got)
	}
	if got := req.Header.Get("Accept"); got != "text/event-stream" {
		t.Fatalf("expected Accept=text/event-stream, got %q", got)
	}
}
