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

func TestOrdinaryClaudePayloadBetaFilterPreservesExplicitHeader(t *testing.T) {
	const model = "claude-opus-5"
	const headerBeta = "effort-2025-11-24"
	calls := 0
	transport := ordinaryClaudeTestRoundTripper(func(r *http.Request) (*http.Response, error) {
		calls++
		body, err := io.ReadAll(r.Body)
		if err != nil || ojson.GetBytes(body, "betas").Exists() || r.Header.Get("Anthropic-Beta") != headerBeta {
			t.Fatalf("explicit beta changed: %q, body=%s, read=%v", r.Header.Get("Anthropic-Beta"), body, err)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"fixture","content":[{"type":"text","text":"ok"}]}`)), Request: r}, nil
	})
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(transport))
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "fixture", "base_url": "https://fixture.invalid"}}
	cfg := &config.Config{Payload: config.PayloadConfig{Filter: []config.PayloadFilterRule{{Models: []config.PayloadModelRule{{Name: model, Protocol: "claude"}}, Params: []string{"betas"}}}}}
	req := cliproxyexecutor.Request{Model: model, Payload: []byte(`{"max_tokens":32,"betas":["fixture-body-2026-01-01"],"messages":[{"role":"user","content":"ordinary"}]}`)}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, Headers: http.Header{"Anthropic-Beta": {headerBeta}}}
	if _, err := NewClaudeExecutor(cfg).Execute(ctx, auth, req, opts); err != nil || calls != 1 {
		t.Fatalf("requests=%d; error=%v", calls, err)
	}
}
