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

// TestKiroExecutor_Refresh_UsesAccountProxyURL verifies that
// KiroExecutor.Refresh routes the OAuth/social-auth refresh request through
// auth.ProxyURL rather than falling back to the global cfg.ProxyURL.
//
// Regression coverage for the bug where the per-account proxy was silently
// ignored at refresh time, causing Kiro accounts with a SOCKS5 account proxy
// to auto-refresh via the global proxy.
func TestKiroExecutor_Refresh_UsesAccountProxyURL(t *testing.T) {
	var accountProxyHits int32

	accountProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			atomic.AddInt32(&accountProxyHits, 1)
		}
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer accountProxy.Close()

	cfg := &config.Config{SDKConfig: config.SDKConfig{ProxyURL: "http://bogus-global-proxy.invalid:1"}}
	exec := NewKiroExecutor(cfg)

	// No client_id/client_secret means refresh falls into the default branch
	// (Kiro social-auth refresh endpoint at prod.us-east-1.auth.desktop.kiro.dev),
	// which is HTTPS and therefore triggers CONNECT through any configured
	// HTTP proxy. The synthetic auth omits last_refresh / expires_at so the
	// short-circuit "still valid" / "recently refreshed" branches do not fire.
	auth := &cliproxyauth.Auth{
		Provider: "kiro",
		ProxyURL: accountProxy.URL,
		Metadata: map[string]any{
			"refresh_token": "fake_refresh_token",
		},
	}

	_, _ = exec.Refresh(context.Background(), auth)

	if got := atomic.LoadInt32(&accountProxyHits); got == 0 {
		t.Fatalf("expected KiroExecutor.Refresh to route through account proxy %s, but proxy received no CONNECT", accountProxy.URL)
	}
}

// TestKiroExecutor_Refresh_IDC_UsesAccountProxyURL verifies the SSO OIDC
// (IDC) refresh path also honors the per-account proxy URL.
func TestKiroExecutor_Refresh_IDC_UsesAccountProxyURL(t *testing.T) {
	var accountProxyHits int32

	accountProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			atomic.AddInt32(&accountProxyHits, 1)
		}
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer accountProxy.Close()

	cfg := &config.Config{SDKConfig: config.SDKConfig{ProxyURL: "http://bogus-global-proxy.invalid:1"}}
	exec := NewKiroExecutor(cfg)

	auth := &cliproxyauth.Auth{
		Provider: "kiro",
		ProxyURL: accountProxy.URL,
		Metadata: map[string]any{
			"refresh_token": "fake_refresh_token",
			"client_id":     "fake_client_id",
			"client_secret": "fake_client_secret",
			"auth_method":   "idc",
			"region":        "us-east-1",
			"start_url":     "https://view.awsapps.com/start",
		},
	}

	_, _ = exec.Refresh(context.Background(), auth)

	if got := atomic.LoadInt32(&accountProxyHits); got == 0 {
		t.Fatalf("expected KiroExecutor.Refresh (IDC) to route through account proxy %s, but proxy received no CONNECT", accountProxy.URL)
	}
}
