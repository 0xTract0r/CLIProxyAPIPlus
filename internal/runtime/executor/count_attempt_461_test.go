package executor

import (
	"context"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	auth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	ex "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	tr "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestCountAttempt461GateAndWire(t *testing.T) {
	for _, denied := range []bool{false, true} {
		t.Run(map[bool]string{true: "gate-denied", false: "sent"}[denied], func(t *testing.T) {
			ctx := ex.WithUpstreamAttemptTracker(context.Background())
			gate := &claudeAttemptTestGate{}
			if denied {
				gate.before = func(int) error { return io.ErrUnexpectedEOF }
			}
			ctx = ex.WithHTTPAttemptGate(ctx, gate)
			calls := 0
			client := &http.Client{Transport: claudeAttemptRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if !ex.UpstreamAttempted(ctx) {
					t.Error("RoundTrip reached without attempt")
				}
				if values := r.Header["X-Stainless-OS"]; len(values) != 1 || values[0] != "MacOS" {
					t.Error("wire casing lost")
				}
				body, _ := io.ReadAll(r.Body)
				if string(body) != "final-signed-body461" {
					t.Error("final body changed at send")
				}
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("{}")), Request: r}, nil
			})}
			req, _ := http.NewRequestWithContext(ctx, "POST", "https://api.anthropic.com/v1/messages/count_tokens", strings.NewReader("final-signed-body461"))
			req.Header.Set("X-Stainless-Os", "MacOS")
			resp, err := doClaudeHTTPWithTransportRetry(ctx, client, req)
			if denied {
				if !ex.IsHTTPAttemptGateError(err) || calls != 0 || ex.UpstreamAttempted(ctx) {
					t.Fatal("local gate denial marked or sent")
				}
			} else {
				if err != nil || calls != 1 || !ex.UpstreamAttempted(ctx) {
					t.Fatalf("send state: %d %v", calls, err)
				}
				resp.Body.Close()
			}
		})
	}
}
func TestCountAttempt461LocalNeverSends(t *testing.T) {
	var calls atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(500) }))
	defer s.Close()
	cfg := &config.Config{Claude: config.ClaudeConfig{LocalTokenCount: true}}
	a := &auth.Auth{Provider: "claude", ProxyURL: "direct", Attributes: map[string]string{"api_key": "local461", "base_url": s.URL}}
	ctx := ex.WithUpstreamAttemptTracker(context.Background())
	_, err := NewClaudeExecutor(cfg).CountTokens(ctx, a, ex.Request{Model: "claude-3-5-sonnet", Payload: []byte(`{"model":"claude-3-5-sonnet","messages":[{"role":"user","content":"local token count"}]}`)}, ex.Options{SourceFormat: tr.FormatClaude})
	if err != nil || calls.Load() != 0 || ex.UpstreamAttempted(ctx) {
		t.Fatalf("local Count attempted upstream: calls=%d attempted=%v err=%v", calls.Load(), ex.UpstreamAttempted(ctx), err)
	}
}
