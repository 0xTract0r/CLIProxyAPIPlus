package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// TestGitLabExecutor_Refresh_UsesAccountProxyURL verifies that
// GitLabExecutor.Refresh routes the OAuth refresh / direct-access token
// exchange through auth.ProxyURL rather than falling back to the global
// cfg.ProxyURL.
//
// Regression coverage for the bug where the per-account proxy was silently
// ignored at refresh time, causing GitLab Duo accounts with a SOCKS5 account
// proxy to auto-refresh via the global proxy.
func TestGitLabExecutor_Refresh_UsesAccountProxyURL(t *testing.T) {
	var accountProxyHits int32

	// Stand up a local HTTP proxy server. CONNECT means the executor's HTTP
	// client is honoring the account proxy URL when reaching the HTTPS
	// gitlab.example.com endpoint. We do not need to actually tunnel the
	// request; observing the CONNECT method on this server is sufficient
	// evidence.
	accountProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			atomic.AddInt32(&accountProxyHits, 1)
		}
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer accountProxy.Close()

	cfg := &config.Config{SDKConfig: config.SDKConfig{ProxyURL: "http://bogus-global-proxy.invalid:1"}}
	exec := NewGitLabExecutor(cfg)

	auth := &cliproxyauth.Auth{
		Provider: "gitlab",
		ProxyURL: accountProxy.URL,
		Metadata: map[string]any{
			"base_url":      "https://gitlab.example.com",
			"access_token":  "fake_access_token",
			"refresh_token": "fake_refresh_token",
			"auth_method":   "oauth",
		},
	}

	_, _ = exec.Refresh(context.Background(), auth)

	if got := atomic.LoadInt32(&accountProxyHits); got == 0 {
		t.Fatalf("expected GitLabExecutor.Refresh to route through account proxy %s, but proxy received no CONNECT", accountProxy.URL)
	}
}
