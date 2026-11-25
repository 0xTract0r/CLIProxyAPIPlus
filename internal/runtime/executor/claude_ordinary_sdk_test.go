package executor

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	claudeauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/claude"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestOrdinaryClaudeSDKRequest(t *testing.T) {
	for _, n := range []string{"api key", "bearer", "origin", "host header", "redirect"} {
		t.Run(n, func(t *testing.T) {
			svc := NewClaudeExecutor(&config.Config{})
			a := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": "https://configured.invalid", "api_key": "sdk-fixture"}}
			if n == "bearer" {
				delete(a.Attributes, "api_key")
				a.Metadata = map[string]any{"access_token": "sdk-fixture"}
			}
			url := "https://configured.invalid/v1/messages"
			if n == "origin" {
				url = "https://other.invalid/v1/messages"
			}
			r, _ := http.NewRequest(http.MethodGet, url, nil)
			r.Header["authorization"] = []string{"stale"}
			if n == "host header" {
				r.Host = "other.invalid"
			}
			calls := 0
			rt := ordinarySyncTransport(func(got *http.Request) (*http.Response, error) {
				calls++
				if got.URL.Host != "configured.invalid" || len(got.Header["authorization"]) != 0 || got.Header.Get("Anthropic-Version") != "2023-06-01" {
					t.Fatalf("unexpected outgoing origin/headers: %v", got.URL)
				}
				if n == "bearer" {
					if got.Header.Get("Authorization") != "Bearer sdk-fixture" {
						t.Fatal("missing bearer")
					}
				} else if got.Header.Get("X-Api-Key") != "sdk-fixture" {
					t.Fatal("missing API key")
				}
				code := 200
				head := make(http.Header)
				if n == "redirect" {
					code = 307
					head.Set("Location", "https://other.invalid/")
				}
				return &http.Response{StatusCode: code, Header: head, Body: http.NoBody, Request: got}, nil
			})
			resp, err := svc.HttpRequest(context.WithValue(context.Background(), "cliproxy.roundtripper", rt), a, r)
			if r.Header["authorization"][0] != "stale" || r.Header.Get("X-Api-Key") != "" {
				t.Fatal("original request was mutated")
			}
			if n == "origin" || n == "host header" {
				reason, ok := err.(statusErr)
				if calls != 0 || !ok || reason.StatusCode() != 403 {
					t.Fatalf("calls=%d err=%v", calls, err)
				}
				return
			}
			if err != nil || calls != 1 {
				t.Fatalf("calls=%d err=%v", calls, err)
			}
			defer resp.Body.Close()
			code := 200
			if n == "redirect" {
				code = 307
			}
			if resp.StatusCode != code {
				t.Fatalf("status=%d", resp.StatusCode)
			}
		})
	}
}

func TestOrdinaryClaudeSDKRefresh(t *testing.T) {
	for _, n := range []string{"rotate", "deny", "cancel"} {
		t.Run(n, func(t *testing.T) {
			svc := NewClaudeExecutor(&config.Config{})
			a := &cliproxyauth.Auth{Metadata: map[string]any{"access_token": "old-fixture", "refreshToken": "sdk-refresh-" + n, "email": "example@invalid", "account_uuid": "fixture-account", "expired": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)}}
			if !svc.ShouldPrepareRequestAuth(a) {
				t.Fatal("expired credential was not selected")
			}
			base, cancel := context.WithCancel(context.Background())
			defer cancel()
			if n == "cancel" {
				cancel()
			}
			calls := 0
			rt := ordinarySyncTransport(func(got *http.Request) (*http.Response, error) {
				calls++
				if got.URL.String() != claudeauth.RefreshTokenURL || got.Method != "POST" || got.Header.Get("Content-Type") != "application/json" {
					t.Fatalf("unexpected refresh request %s %s", got.Method, got.URL)
				}
				code := 200
				body := `{"access_token":"updated-fixture","refresh_token":"rotated-fixture","expires_in":600}`
				if n == "deny" {
					code = 403
					body = `{"error":"access_denied"}`
				}
				return &http.Response{StatusCode: code, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: got}, nil
			})
			next, err := svc.PrepareRequestAuth(context.WithValue(base, "cliproxy.roundtripper", rt), a)
			if a.Metadata["access_token"] != "old-fixture" {
				t.Fatal("original credential was overwritten")
			}
			switch n {
			case "cancel":
				if err != context.Canceled || calls != 0 {
					t.Fatalf("calls=%d err=%v", calls, err)
				}
			case "deny":
				if err == nil || next != nil || calls != 1 {
					t.Fatalf("credential=%t calls=%d err=%v", next != nil, calls, err)
				}
			default:
				if err != nil || calls != 1 || next.Metadata["access_token"] != "updated-fixture" || next.Metadata["refresh_token"] != "rotated-fixture" || next.Metadata["email"] != "example@invalid" || next.Metadata["account_uuid"] != "fixture-account" {
					t.Fatalf("credential=%t calls=%d err=%v", next != nil, calls, err)
				}
			}
		})
	}
}
