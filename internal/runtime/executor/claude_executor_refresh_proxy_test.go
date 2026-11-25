package executor

import (
	"io"
	"net/http"
	"strings"
	"testing"

	claudeauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/claude"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/httptransport"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestOrdinaryClaudeRefreshUsesExplicitProxy(t *testing.T) {
	const accountProxy = "http://fixture-account-proxy.invalid:8080"
	a := &cliproxyauth.Auth{
		Provider: "claude", ProxyURL: accountProxy,
		Metadata: map[string]any{"refresh_token": "fixture-refresh", "access_token": "fixture-older"},
	}
	calls := 0
	ctx := httptransport.WithDecorator(t.Context(), func(base http.RoundTripper) http.RoundTripper {
		return ordinaryClaudeTestRoundTripper(func(req *http.Request) (*http.Response, error) {
			calls++
			transport, ok := base.(*http.Transport)
			if !ok || transport.Proxy == nil {
				t.Fatal("explicit OAuth refresh route did not select a standard proxy transport")
			}
			proxy, err := transport.Proxy(req)
			if err != nil || proxy.String() != accountProxy {
				t.Error("OAuth refresh selected the wrong proxy")
			}
			if req.URL.String() != claudeauth.RefreshTokenURL || req.Method != http.MethodPost {
				t.Fatal("OAuth refresh escaped its fixed issuer and method")
			}
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}},
				Body: io.NopCloser(strings.NewReader(`{"access_token":"fixture-fresh","refresh_token":"fixture-rotated","expires_in":600}`)), Request: req}, nil
		})
	})
	e := NewClaudeExecutor(&config.Config{SDKConfig: config.SDKConfig{ProxyURL: "http://fixture-global-proxy.invalid:8080"}})
	got, err := e.Refresh(ctx, a)
	if err != nil || got == nil || calls != 1 || got.Metadata["access_token"] != "fixture-fresh" ||
		got.Metadata["refresh_token"] != "fixture-rotated" || a.Metadata["access_token"] != "fixture-older" {
		t.Fatalf("refresh results invalid: attempts=%d err=%v", calls, err)
	}
}
