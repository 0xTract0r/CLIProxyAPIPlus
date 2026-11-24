package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/copilot"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/httptransport"
)

func TestOrdinaryCopilotRefreshRejectsInvalidExplicitProxy(t *testing.T) {
	auth := copilotOrdinaryFixtureAuth(t, t.Name(), "gpt-5-codex", copilot.Chat)
	auth.Metadata["refresh_token"] = "fake-grant"
	auth.ProxyURL = "invalid://fixture"
	calls := 0
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", copilotOrdinaryFixtureTransport(func(*http.Request) (*http.Response, error) {
		calls++
		t.Fatal("invalid explicit proxy fell back to another transport")
		return nil, nil
	}))
	if _, err := NewCopilotExecutor(nil).Refresh(ctx, auth); err == nil || calls != 0 {
		t.Fatalf("invalid proxy error=%v; fallback calls=%d", err, calls)
	}
}

func TestOrdinaryCopilotPausedRefreshReturnsSelectedCredential(t *testing.T) {
	for _, tc := range []struct {
		name  string
		pause func(*coreauth.Auth)
	}{
		{"explicit pause", func(a *coreauth.Auth) { a.Metadata["account_settings"] = map[string]any{"refresh_enabled": false} }},
		{"disabled flag", func(a *coreauth.Auth) { a.Disabled = true }},
		{"disabled status", func(a *coreauth.Auth) { a.Status = coreauth.StatusDisabled }},
		{"quarantined", func(a *coreauth.Auth) { a.AutoQuarantined = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			auth := copilotOrdinaryFixtureAuth(t, t.Name(), "claude-opus-5", copilot.Chat)
			tc.pause(auth)
			calls := 0
			ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", copilotOrdinaryFixtureTransport(func(*http.Request) (*http.Response, error) {
				calls++
				return nil, context.Canceled
			}))
			for _, grant := range []string{"", "fixture-grant"} {
				auth.Metadata["refresh_token"] = grant
				got, err := NewCopilotExecutor(nil).Refresh(ctx, auth)
				if err != nil || got != auth || calls != 0 {
					t.Fatalf("got selected=%t calls=%d err=%v", got == auth, calls, err)
				}
			}
		})
	}
}

func TestOrdinaryCopilotRefreshUsesSelectedProxy(t *testing.T) {
	auth := copilotOrdinaryFixtureAuth(t, t.Name(), "claude-opus-5", copilot.Chat)
	auth.Metadata["refresh_token"] = "fixture-grant"
	auth.ProxyURL = "http://selected-proxy.invalid:8080"
	cfg := &config.Config{SDKConfig: config.SDKConfig{ProxyURL: "http://global-proxy.invalid:8080"}}
	calls := 0
	ctx := httptransport.WithDecorator(t.Context(), func(next http.RoundTripper) http.RoundTripper {
		return copilotOrdinaryFixtureTransport(func(r *http.Request) (*http.Response, error) {
			calls++
			transport, ok := next.(*http.Transport)
			if !ok || transport.Proxy == nil {
				t.Fatal("expected a configured HTTP proxy transport")
			}
			selected, err := transport.Proxy(r)
			if err != nil || selected == nil || selected.String() != auth.ProxyURL {
				t.Fatalf("wrong selected proxy: %v (%v)", selected, err)
			}
			if r.URL.Host != "api.github.com" || r.Header.Get("Authorization") != "Bearer fixture-grant" {
				t.Fatal("refresh changed the selected grant or exchange origin")
			}
			body := fmt.Sprintf(`{"token":"replacement","expires_at":%d,"endpoints":{"api":"https://api.individual.githubcopilot.com"}}`, time.Now().Add(time.Hour).Unix())
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
		})
	})
	got, err := NewCopilotExecutor(cfg).Refresh(ctx, auth)
	if err != nil || got == nil || got.Metadata["access_token"] != "replacement" || calls != 1 {
		t.Fatalf("exchange calls=%d err=%v", calls, err)
	}
}
