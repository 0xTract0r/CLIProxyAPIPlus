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

func TestOrdinaryClaudeExplicitEffortDirectivePreserved(t *testing.T) {
	calls := 0
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", ordinaryClaudeTestRoundTripper(func(r *http.Request) (*http.Response, error) {
		calls++
		b, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		doc := ojson.NewDoc(b)
		if doc.Get("messages.0.role").String() != "user" ||
			doc.Get("messages.1.role").String() != "system" ||
			doc.Get("messages.1.output_config.effort").String() != "low" ||
			doc.Get("messages.2.role").String() != "user" ||
			doc.Get("output_config.effort").String() != "medium" {
			t.Fatal("explicit mid-turn effort directive was changed or reordered")
		}
		if !strings.Contains(string(b), "caller guidance") {
			t.Fatal("system guidance was lost")
		}
		return &http.Response{Request: r, StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{"id":"fixture","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}]}`))}, nil
	}))
	opts := cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatClaude, Headers: http.Header{"Anthropic-Beta": {"mid-conversation-output-config-2026-07-01"}},
	}
	a := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "fixture", "base_url": "https://fixture.invalid"}}
	req := cliproxyexecutor.Request{Model: "claude-opus-5-5", Payload: []byte(`{"max_tokens":32,"system":"caller guidance","output_config":{"effort":"medium"},"thinking":{"type":"adaptive"},"messages":[{"role":"user","content":"earlier summary"},{"role":"system","content":[],"output_config":{"effort":"low"}},{"role":"user","content":"continue"}]}`)}
	if _, err := NewClaudeExecutor(&config.Config{}).Execute(ctx, a, req, opts); err != nil || calls != 1 {
		t.Fatalf("ordinary request failed: attempts=%d err=%v", calls, err)
	}
}
