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

func TestOrdinaryClaudePassesPublicBetasOnWire(t *testing.T) {
	for _, method := range []string{"api key", "Bearer"} {
		t.Run(method, func(t *testing.T) {
			a := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": "https://fixture.invalid", "api_key": "fixture"}}
			if method == "Bearer" {
				delete(a.Attributes, "api_key")
				a.Metadata = map[string]any{"access_token": "fixture"}
			}
			calls := 0
			ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", ordinaryClaudeTestRoundTripper(func(r *http.Request) (*http.Response, error) {
				calls++
				if got := r.Header.Get("Anthropic-Beta"); got != "client-beta-2026-01-01,body-beta-2026-01-01" {
					t.Errorf("outbound Anthropic-Beta = %q", got)
				}
				raw, err := io.ReadAll(r.Body)
				if err != nil || ojson.GetBytesNoCopy(raw, "betas").Exists() {
					t.Fatalf("beta control field leaked into payload: %v", err)
				}
				if r.Header.Get("X-App") != "" {
					t.Fatal("untrusted client identity header escaped the standard transport")
				}
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}},
					Body: io.NopCloser(strings.NewReader(`{"id":"fixture","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}]}`)), Request: r}, nil
			}))
			e := NewClaudeExecutor(&config.Config{})
			req := cliproxyexecutor.Request{Model: "claude-fixture", Payload: []byte(`{"max_tokens":30,"messages":[{"role":"user","content":"ordinary"}],"betas":["client-beta-2026-01-01","body-beta-2026-01-01","claude-code-untrusted","bad beta"]}`)}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, Headers: http.Header{
				"Anthropic-Beta": {"client-beta-2026-01-01"}, "X-App": {"fixture-value"},
			}}
			if _, err := e.Execute(ctx, a, req, opts); err != nil || calls != 1 {
				t.Fatalf("Execute attempts=%d err=%v", calls, err)
			}
		})
	}
}
