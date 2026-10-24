package executor

import (
	"context"
	"fmt"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	auth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	execution "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	translator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestClaudeTokenCountModeUsesConfiguredEndpointOrLocalEstimate(t *testing.T) {
	for _, key := range []string{"synthetic-key", "sk-ant-oat-synthetic", ""} {
		for _, local := range []bool{false, true} {
			t.Run(fmt.Sprintf("key=%s/local=%t", key, local), func(t *testing.T) {
				var calls atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					if r.URL.Path != "/v1/messages/count_tokens" {
						t.Errorf("path=%q", r.URL.Path)
					}
					w.Header().Set("Content-Type", "application/json")
					w.Header().Set("X-Count-Source", "controlled")
					w.Write([]byte(`{"input_tokens":42}`))
				}))
				defer server.Close()
				cfg := &config.Config{Claude: config.ClaudeConfig{LocalTokenCount: local}}
				a := &auth.Auth{ID: "count-mode", Provider: "claude", ProxyURL: "direct", Attributes: map[string]string{"api_key": key, "base_url": server.URL}, Metadata: claudeOAuthTestMetadata()}
				if key == "" {
					delete(a.Metadata, "access_token")
				}
				response, err := NewClaudeExecutor(cfg).CountTokens(context.Background(), a, execution.Request{Model: "claude-sonnet-4-5", Payload: []byte(`{"system":"client system instructions","messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]}`)}, execution.Options{SourceFormat: translator.FormatClaude})
				if err != nil {
					t.Fatal(err)
				}
				wantCalls, wantCount := int32(1), int64(42)
				if local {
					wantCalls = 0
					wantCount = 7
				}
				if calls.Load() != wantCalls || gjson.GetBytes(response.Payload, "input_tokens").Int() != wantCount {
					t.Fatalf("calls=%d count=%s", calls.Load(), response.Payload)
				}
				if !local && response.Headers.Get("X-Count-Source") != "controlled" {
					t.Fatal("lost upstream response headers")
				}
			})
		}
	}
}

func TestClaudeTokenCountCredentialDefaultCannotSilentlyUseLocal(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(500) }))
	defer server.Close()
	for _, endpoint := range []string{"", " \t ", server.URL} {
		for _, local := range []bool{false, true} {
			t.Run(fmt.Sprintf("endpoint=%q/local=%t", endpoint, local), func(t *testing.T) {
				a := &auth.Auth{ID: "count-mode-proxy-guard", Provider: "claude", ProxyURL: "bad-proxy", Attributes: map[string]string{"api_key": "synthetic-key", "base_url": endpoint}}
				cfg := &config.Config{Claude: config.ClaudeConfig{LocalTokenCount: local}}
				response, err := NewClaudeExecutor(cfg).CountTokens(context.Background(), a, execution.Request{Model: "claude-sonnet-4-5", Payload: []byte(`{"messages":[{"role":"user","content":"hello"}]}`)}, execution.Options{SourceFormat: translator.FormatClaude})
				if local {
					if err != nil || gjson.GetBytes(response.Payload, "input_tokens").Int() != 3 {
						t.Fatalf("explicit local failed: err=%v payload=%s", err, response.Payload)
					}
				} else if err == nil || len(response.Payload) != 0 {
					t.Fatal("credential default bypassed upstream error chain with a local success")
				}
				if calls.Load() != 0 {
					t.Fatal("counting bypassed invalid account proxy")
				}
			})
		}
	}
}
