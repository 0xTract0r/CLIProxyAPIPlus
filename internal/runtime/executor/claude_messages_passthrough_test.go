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

func TestOrdinaryClaudeDirectMessagesPreservesCallerShape(t *testing.T) {
	var seenBody []byte
	var seenHeaders http.Header
	transport := ordinaryClaudeTestRoundTripper(func(req *http.Request) (*http.Response, error) {
		seenBody, _ = io.ReadAll(req.Body)
		seenHeaders = req.Header.Clone()
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"id":"msg_direct","type":"message","model":"claude-opus-5-5","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`)),
			Request:    req,
		}, nil
	})
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(transport))
	incoming := http.Header{
		"Anthropic-Beta": {"caller-beta-2099-01-01"},
	}
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "fixture", "base_url": "https://fixture.invalid"}}
	payload := []byte(`{"model":"claude-opus-5-5","system":[{"type":"text","text":"caller system","cache_control":{"type":"ephemeral"}}],"thinking":{"type":"adaptive","display":"summarized"},"tools":[{"name":"caller_tool","description":"caller tool","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"hello"}],"max_tokens":32,"prompt_cache_options":{"mode":"explicit"}}`)

	_, err := NewClaudeExecutor(&config.Config{}).Execute(ctx, auth, cliproxyexecutor.Request{Model: "claude-opus-5-5", Payload: payload}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatClaude,
		Headers:      incoming,
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got := seenHeaders.Get("Anthropic-Beta"); got != incoming.Get("Anthropic-Beta") {
		t.Fatalf("Anthropic-Beta = %q, want %q; headers=%v", got, incoming.Get("Anthropic-Beta"), seenHeaders)
	}
	if got := ojson.GetBytes(seenBody, "system.#").Int(); got != 1 {
		t.Fatalf("system block count = %d, want the caller block only", got)
	}
	if got := ojson.GetBytes(seenBody, "thinking.display").String(); got != "summarized" {
		t.Fatalf("thinking.display = %q, want summarized", got)
	}
	if got := ojson.GetBytes(seenBody, "tools.0.name").String(); got != "caller_tool" {
		t.Fatalf("tool name = %q, want caller_tool", got)
	}
	if ojson.GetBytes(seenBody, "messages.0.content.0.cache_control").Exists() {
		t.Fatal("direct Messages request gained a synthetic cache breakpoint")
	}
}
