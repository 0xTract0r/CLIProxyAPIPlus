package executor

import (
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// TestGitHubCopilotApplyHeaders_AppliesCustomHeaders verifies that the
// GitHub Copilot applyHeaders helper honors account-defined custom headers
// stored under the `header:` attribute namespace, matching the existing
// Codex / Claude / Kimi / Kilo executor pattern via
// util.ApplyCustomHeadersFromAttrs.
func TestGitHubCopilotApplyHeaders_AppliesCustomHeaders(t *testing.T) {
	cfg := &config.Config{}
	exec := NewGitHubCopilotExecutor(cfg)

	req, err := http.NewRequest(http.MethodPost, "https://copilot.example/chat/completions", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	auth := &cliproxyauth.Auth{ProxyURL: "direct",
		Provider: "github-copilot",
		Attributes: map[string]string{
			"header:X-Custom-Foo":         "bar",
			"header:X-Forwarded-Identity": "alice@example.com",
		},
	}

	exec.applyHeaders(req, "fake_api_token", nil, auth)

	if got := req.Header.Get("X-Custom-Foo"); got != "bar" {
		t.Fatalf("expected X-Custom-Foo=bar, got %q", got)
	}
	if got := req.Header.Get("X-Forwarded-Identity"); got != "alice@example.com" {
		t.Fatalf("expected X-Forwarded-Identity=alice@example.com, got %q", got)
	}
}

// TestGitHubCopilotApplyHeaders_NilAuthDoesNotPanic guards the legacy code
// path where auth is nil and ensures the helper keeps producing the
// hard-coded provider headers.
func TestGitHubCopilotApplyHeaders_NilAuthDoesNotPanic(t *testing.T) {
	cfg := &config.Config{}
	exec := NewGitHubCopilotExecutor(cfg)

	req, err := http.NewRequest(http.MethodPost, "https://copilot.example/chat/completions", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	exec.applyHeaders(req, "fake_api_token", nil, nil)
	if got := req.Header.Get("Authorization"); got != "Bearer fake_api_token" {
		t.Fatalf("expected Authorization=Bearer fake_api_token, got %q", got)
	}
}
