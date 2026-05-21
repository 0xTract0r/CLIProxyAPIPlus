package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
)

// TestClaudeExecutor_Refresh_UsesAccountProxyURL verifies that
// ClaudeExecutor.Refresh routes the OAuth refresh request through
// auth.ProxyURL rather than falling back to the global cfg.ProxyURL.
//
// Regression coverage for the bug where the per-account proxy was silently
// ignored at refresh time, causing accounts with a SOCKS account proxy to
// auto-refresh via the global proxy and fail with no explicit error.
func TestClaudeExecutor_Refresh_UsesAccountProxyURL(t *testing.T) {
	var accountProxyHits int32

	// Stand up a local HTTP proxy server. CONNECT means the executor's HTTP
	// client is honoring the account proxy URL when reaching the HTTPS OAuth
	// endpoint. We do not need to actually tunnel the request; observing the
	// CONNECT method on this server is sufficient evidence.
	accountProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			atomic.AddInt32(&accountProxyHits, 1)
		}
		// Force the connection to fail so the executor returns quickly; we are
		// only asserting that the request reached this proxy.
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer accountProxy.Close()

	cfg := &config.Config{SDKConfig: config.SDKConfig{ProxyURL: "http://bogus-global-proxy.invalid:1"}}
	exec := NewClaudeExecutor(cfg)

	auth := &cliproxyauth.Auth{
		Provider: "claude",
		ProxyURL: accountProxy.URL,
		Metadata: map[string]any{
			"refresh_token": "fake_refresh_token",
		},
	}

	_, _ = exec.Refresh(context.Background(), auth)

	if got := atomic.LoadInt32(&accountProxyHits); got == 0 {
		t.Fatalf("expected ClaudeExecutor.Refresh to route through account proxy %s, but proxy received no CONNECT", accountProxy.URL)
	}
}

func TestClaudeExecutor_Execute_TransportErrorIsBadGateway(t *testing.T) {
	disableClaudeTransportRetryBackoff(t)

	exec := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Provider: "claude",
		ProxyURL: "http://127.0.0.1:1",
		Attributes: map[string]string{
			"api_key":  "test-api-key",
			"base_url": "http://example.invalid",
		},
	}

	_, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-haiku-4-5-20251001",
		Payload: []byte(`{"model":"claude-haiku-4-5-20251001","messages":[{"role":"user","content":"hi"}]}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")})
	if err == nil {
		t.Fatal("Execute() error = nil, want proxy transport error")
	}
	statusProvider, ok := err.(interface{ StatusCode() int })
	if !ok {
		t.Fatalf("Execute() error type %T does not expose StatusCode", err)
	}
	if statusProvider.StatusCode() != http.StatusBadGateway {
		t.Fatalf("StatusCode() = %d, want %d", statusProvider.StatusCode(), http.StatusBadGateway)
	}
}
