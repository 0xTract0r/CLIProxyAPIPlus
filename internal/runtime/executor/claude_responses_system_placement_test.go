package executor

import (
	"context"
	"errors"
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

func TestOrdinaryClaudeResponsesPreservesSystemText(t *testing.T) {
	for _, model := range []string{"claude-sonnet-4-5", "claude-sonnet-5"} {
		t.Run(model, func(t *testing.T) {
			var sent string
			ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", ordinaryClaudeTestRoundTripper(func(r *http.Request) (*http.Response, error) {
				b, err := io.ReadAll(r.Body)
				if err != nil {
					return nil, err
				}
				sent = string(b)
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"fixture","type":"message","role":"assistant","model":"claude-fixture","content":[{"type":"text","text":"ok"}]}`)), Request: r}, nil
			}))
			a := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": "https://fixture.invalid", "api_key": "fixture"}}
			e := NewClaudeExecutor(&config.Config{})
			req := cliproxyexecutor.Request{Model: model, Payload: []byte(`{"instructions":"TOP_RULE","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"FIRST_USER"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"MODEL_TURN"}]},{"type":"message","role":"developer","content":[{"type":"input_text","text":"MID_RULE"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"FINAL_USER"}]}]}`)}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse}
			if _, err := e.Execute(ctx, a, req, opts); err != nil {
				t.Fatal(err)
			}
			for _, marker := range []string{"TOP_RULE", "MID_RULE", "FIRST_USER", "MODEL_TURN", "FINAL_USER"} {
				if strings.Count(sent, marker) != 1 {
					t.Fatalf("%s was lost or duplicated: %s", marker, sent)
				}
			}
			if strings.Index(sent, "FIRST_USER") > strings.Index(sent, "MODEL_TURN") || strings.Index(sent, "MODEL_TURN") > strings.Index(sent, "FINAL_USER") {
				t.Fatal("conversation messages reordered")
			}
			if count, err := e.CountTokens(t.Context(), a, req, opts); err != nil || ojson.GetBytesNoCopy(count.Payload, "input_tokens").Int() == 0 {
				t.Fatalf("local count=%s err=%v", count.Payload, err)
			}
			if model == "claude-sonnet-5" {
				official := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": "https://api.anthropic.com", "api_key": "fixture"}}
				noSend := context.WithValue(t.Context(), "cliproxy.roundtripper", ordinaryClaudeTestRoundTripper(func(*http.Request) (*http.Response, error) {
					t.Fatal("system role reached native count endpoint")
					return nil, nil
				}))
				_, err := e.CountTokens(noSend, official, req, opts)
				var status interface{ StatusCode() int }
				if !errors.As(err, &status) || status.StatusCode() != http.StatusBadRequest {
					t.Fatalf("native count accepted translated system role: %v", err)
				}
			}
		})
	}
}

func TestOrdinaryClaudeResponsesRejectsNonTextSystemPart(t *testing.T) {
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", ordinaryClaudeTestRoundTripper(func(*http.Request) (*http.Response, error) {
		t.Fatal("unsupported system part reached the provider")
		return nil, nil
	}))
	a := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": "https://fixture.invalid", "api_key": "fixture"}}
	e := NewClaudeExecutor(&config.Config{})
	req := cliproxyexecutor.Request{Model: "claude-sonnet-5", Payload: []byte(`{"input":[{"type":"message","role":"developer","content":[{"type":"input_image","image_url":"https://fixture.invalid/system-image.png"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"ordinary"}]}]}`)}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse}
	_, err := e.Execute(ctx, a, req, opts)
	var status interface{ StatusCode() int }
	if !errors.As(err, &status) || status.StatusCode() != http.StatusBadRequest {
		t.Fatalf("unsupported system part returned %v", err)
	}
}
