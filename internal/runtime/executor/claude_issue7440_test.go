package executor

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestOrdinaryClaudeCallerFallbackPreservesOriginalRateLimit(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		name := "absent"
		extra := ""
		if explicit {
			name = "explicit"
			extra = `,"fallbacks":[{"model":"fixture-fallback"}]`
		}
		t.Run(name, func(t *testing.T) {
			calls := 0
			rt := ordinaryClaudeTestRoundTripper(func(r *http.Request) (*http.Response, error) {
				calls++
				raw, err := io.ReadAll(r.Body)
				field := ojson.GetBytes(raw, "fallbacks")
				if err != nil || field.Exists() != explicit || (explicit && field.Get("0.model").String() != "fixture-fallback") {
					t.Fatalf("fallbacks changed: %s, read=%v", raw, err)
				}
				return &http.Response{StatusCode: 429, Header: http.Header{"Content-Type": {"application/json"}, "Retry-After": {"12"}}, Body: io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"rate_limit_error","message":"mock rate limit"}}`)), Request: r}, nil
			})
			ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(rt))
			const model = "claude-opus-5"
			req := cliproxyexecutor.Request{Model: model, Payload: []byte(`{"max_tokens":32,"messages":[{"role":"user","content":"ordinary"}]` + extra + "}")}
			auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "fixture", "base_url": "https://fixture.invalid"}}
			_, err := NewClaudeExecutor(&config.Config{}).Execute(ctx, auth, req, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude})
			status, ok := err.(interface{ StatusCode() int })
			if !ok || status.StatusCode() != 429 || calls != 1 {
				t.Fatalf("requests=%d; error=%v", calls, err)
			}
		})
	}
}
