package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// TestQwenExecutor_Refresh_UsesAccountProxyURL verifies that
// QwenExecutor.Refresh routes the OAuth refresh request through
// auth.ProxyURL rather than falling back to the global cfg.ProxyURL.
//
// Regression coverage for the bug where the per-account proxy was silently
// ignored at refresh time.
func TestQwenExecutor_Refresh_UsesAccountProxyURL(t *testing.T) {
	var accountProxyHits int32

	accountProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			atomic.AddInt32(&accountProxyHits, 1)
		}
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer accountProxy.Close()

	cfg := &config.Config{SDKConfig: config.SDKConfig{ProxyURL: "http://bogus-global-proxy.invalid:1"}}
	exec := NewQwenExecutor(cfg)

	auth := &cliproxyauth.Auth{
		Provider: "qwen",
		ProxyURL: accountProxy.URL,
		Metadata: map[string]any{
			"refresh_token": "fake_refresh_token",
		},
	}

	_, _ = exec.Refresh(context.Background(), auth)

	if got := atomic.LoadInt32(&accountProxyHits); got == 0 {
		t.Fatalf("expected QwenExecutor.Refresh to route through account proxy %s, but proxy received no CONNECT", accountProxy.URL)
	}
}
