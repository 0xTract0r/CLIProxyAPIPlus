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

// TestGitHubCopilotExecutor_Refresh_UsesAccountProxyURL verifies that
// GitHubCopilotExecutor.Refresh routes the GitHub-token validation /
// Copilot API token exchange through auth.ProxyURL rather than falling back
// to the global cfg.ProxyURL.
//
// Regression coverage for the bug where the per-account proxy was silently
// ignored both at Refresh time AND on every business request (via
// ensureAPIToken), causing GitHub Copilot accounts with a per-account proxy
// to persistently leak through the global proxy.
func TestGitHubCopilotExecutor_Refresh_UsesAccountProxyURL(t *testing.T) {
	var accountProxyHits int32

	accountProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			atomic.AddInt32(&accountProxyHits, 1)
		}
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer accountProxy.Close()

	cfg := &config.Config{SDKConfig: config.SDKConfig{ProxyURL: "http://bogus-global-proxy.invalid:1"}}
	exec := NewGitHubCopilotExecutor(cfg)

	auth := &cliproxyauth.Auth{
		Provider: "github-copilot",
		ProxyURL: accountProxy.URL,
		Metadata: map[string]any{
			"access_token": "fake_github_access_token",
		},
	}

	_, _ = exec.Refresh(context.Background(), auth)

	if got := atomic.LoadInt32(&accountProxyHits); got == 0 {
		t.Fatalf("expected GitHubCopilotExecutor.Refresh to route through account proxy %s, but proxy received no CONNECT", accountProxy.URL)
	}
}
