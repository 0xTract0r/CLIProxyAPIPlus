package executor

import (
	"net/http"
	"testing"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestAntigravityPrepareRequest_AppliesCustomHeaders(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "https://cloudcode-pa.googleapis.com/v1/models", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	auth := &cliproxyauth.Auth{
		Provider: "antigravity",
		Metadata: map[string]any{
			"access_token": "fake_token",
			"expired":      time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		},
		Attributes: map[string]string{
			"header:X-Custom-Foo":  "bar",
			"header:X-Account-Tag": "antigravity-a",
		},
	}

	if err := NewAntigravityExecutor(nil).PrepareRequest(req, auth); err != nil {
		t.Fatalf("prepare request: %v", err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer fake_token" {
		t.Fatalf("expected Authorization=Bearer fake_token, got %q", got)
	}
	if got := req.Header.Get("X-Custom-Foo"); got != "bar" {
		t.Fatalf("expected X-Custom-Foo=bar, got %q", got)
	}
	if got := req.Header.Get("X-Account-Tag"); got != "antigravity-a" {
		t.Fatalf("expected X-Account-Tag=antigravity-a, got %q", got)
	}
}
