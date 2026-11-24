package executor

import (
	"context"
	"github.com/MoeForge/moego/json"
	"github.com/MoeForge/moego/ojson"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/copilot"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	_ "github.com/router-for-me/CLIProxyAPI/v8/internal/translator/openai/gemini"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCopilotChatClampsThinkingBudget(t *testing.T) {
	const id = "gemini-4-argon"
	auth := copilotOrdinaryFixtureAuth(t, "account", id, copilot.Chat)
	model := copilot.Model{ID: id, Endpoints: []string{copilot.Chat}}
	model.Capabilities.Supports.MinBudget = 128
	model.Capabilities.Supports.MaxBudget = 128000
	model.Capabilities.Supports.Reasoning = json.RawMessage(`["low","high","xhigh"]`)
	auth.CopilotCatalog = copilot.NewCatalog([]copilot.Model{model}, time.Now())
	exec := NewCopilotExecutor(&config.Config{})

	for _, tc := range []struct {
		name, model, body string
		format            sdktranslator.Format
	}{
		{"Gemini body", id, `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"thinkingConfig":{"thinkingBudget":64000}}}`, sdktranslator.FormatGemini},
		{"Claude body", id, `{"messages":[{"role":"user","content":"hi"}],"max_tokens":65536,"thinking":{"type":"enabled","budget_tokens":64000}}`, sdktranslator.FormatClaude},
		{"numeric suffix", id + "(64000)", `{"messages":[{"role":"user","content":"hi"}]}`, sdktranslator.FormatOpenAI},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := cliproxyexecutor.Request{Model: copilot.ModelPrefix + tc.model, Payload: []byte(tc.body)}
			prepared, err := exec.prepare(t.Context(), auth, req, cliproxyexecutor.Options{SourceFormat: tc.format}, false, false)
			if err != nil {
				t.Fatal(err)
			}
			if got := ojson.GetBytesNoCopy(prepared.body, "reasoning_effort").String(); got != "xhigh" {
				t.Errorf("reasoning_effort = %q, want xhigh", got)
			}
			if support := prepared.route.Info.Thinking; support == nil || support.Min != 128 || support.Max != 128000 {
				t.Errorf("route thinking capabilities changed: %+v", support)
			}
		})
	}
}

func TestCopilotResponsesClampsThinkingBudget(t *testing.T) {
	const id = "gemini-4-argon"
	auth := copilotOrdinaryFixtureAuth(t, "account", id, copilot.Responses)
	model := copilot.Model{ID: id, Endpoints: []string{copilot.Responses}}
	model.Capabilities.Supports.MinBudget = 128
	model.Capabilities.Supports.MaxBudget = 128000
	model.Capabilities.Supports.Reasoning = json.RawMessage(`["low","high","xhigh"]`)
	auth.CopilotCatalog = copilot.NewCatalog([]copilot.Model{model}, time.Now())
	exec := NewCopilotExecutor(&config.Config{})

	for _, tc := range []struct {
		name, model, body, want string
		format                  sdktranslator.Format
	}{
		{"Claude body", id, `{"messages":[{"role":"user","content":"hi"}],"max_tokens":65536,"thinking":{"type":"enabled","budget_tokens":64000}}`, "xhigh", sdktranslator.FormatClaude},
		{"Chat numeric suffix", id + "(64000)", `{"messages":[{"role":"user","content":"hi"}]}`, "xhigh", sdktranslator.FormatOpenAI},
		{"numeric suffix", id + "(64000)", `{"input":"hi"}`, "xhigh", sdktranslator.FormatOpenAIResponse},
		{"auto suffix", id + "(auto)", `{"input":"hi"}`, "low", sdktranslator.FormatOpenAIResponse},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := cliproxyexecutor.Request{Model: copilot.ModelPrefix + tc.model, Payload: []byte(tc.body)}
			prepared, err := exec.prepare(t.Context(), auth, req, cliproxyexecutor.Options{SourceFormat: tc.format}, false, false)
			if err != nil {
				t.Fatal(err)
			}
			if got := ojson.GetBytesNoCopy(prepared.body, "reasoning.effort").String(); got != tc.want {
				t.Errorf("reasoning.effort = %q, want %q", got, tc.want)
			}
			if support := prepared.route.Info.Thinking; support == nil || support.Min != 128 || support.Max != 128000 {
				t.Errorf("route thinking capabilities changed: %+v", support)
			}
		})
	}
}

func copilotOrdinaryFixtureAuth(t *testing.T, id, model, endpoint string) *cliproxyauth.Auth {
	t.Helper()
	return &cliproxyauth.Auth{ID: id, Provider: copilot.Provider, Metadata: map[string]any{"access_token": "test-credential", "base_url": "https://api.individual.githubcopilot.com"}, CopilotCatalog: copilot.NewCatalog([]copilot.Model{{ID: model, Endpoints: []string{endpoint}}}, time.Now())}
}

type copilotOrdinaryFixtureTransport func(*http.Request) (*http.Response, error)

func (rt copilotOrdinaryFixtureTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return rt(r)
}

func TestCopilotGeminiTranslationCannotChangeCredentialOrRoute(t *testing.T) {
	e := NewCopilotExecutor(&config.Config{})
	model := "claude-opus-5"
	gemini := cliproxyexecutor.Request{Model: model, Payload: []byte(`{"contents":[{"role":"user","parts":[{"text":"fixture"}]}]}`)}
	chat := copilotOrdinaryFixtureAuth(t, "account", model, copilot.Chat)
	planned := e.PlanRequestRoute(chat, gemini, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatGemini, Metadata: map[string]any{cliproxyexecutor.CanonicalSessionIDMetadataKey: "same-fixture-session"}})
	if p, err := e.prepare(t.Context(), chat, gemini, planned, false, false); err != nil || p.route.Endpoint != copilot.Chat {
		t.Fatalf("public Chat adapter failed: %v", err)
	}
	a := copilotOrdinaryFixtureAuth(t, "account", model, copilot.Responses)
	if _, err := e.prepare(t.Context(), a, gemini, planned, false, false); err == nil || !strings.Contains(err.Error(), "route changed") {
		t.Fatalf("planned endpoint escaped: %v", err)
	}
	key := copilotOrdinaryFixtureAuth(t, "account", model, copilot.Chat)
	key.Metadata = map[string]any{"base_url": "https://api.individual.githubcopilot.com"}
	key.Attributes = map[string]string{"api_key": "caller-key"}
	calls := 0
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", copilotOrdinaryFixtureTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		t.Fatal("caller API key reached Copilot transport")
		return nil, nil
	}))
	if _, err := e.Execute(ctx, key, gemini, planned); err == nil || calls != 0 {
		t.Fatalf("caller key bypassed the credential contract: %v", err)
	}
}

func TestCopilotOrdinaryRejectionLogsOnlyAllowedMetadata(t *testing.T) {
	g, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx := context.WithValue(t.Context(), "gin", g)
	ctx = context.WithValue(ctx, "cliproxy.roundtripper", copilotOrdinaryFixtureTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 429, Header: http.Header{"Content-Type": {"application/json"}, "Retry-After": {"11"}, "Set-Cookie": {"private-cookie"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"private-upstream-body"}}`)), Request: r}, nil
	}))
	model := "claude-opus-5"
	a := copilotOrdinaryFixtureAuth(t, "account", model, copilot.Chat)
	req := cliproxyexecutor.Request{Model: model, Payload: []byte(`{"messages":[{"role":"user","content":"private-user-prompt"}]}`)}
	_, err := NewCopilotExecutor(&config.Config{}).Execute(ctx, a, req, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI})
	if err == nil {
		t.Fatal("provider refusal was lost")
	}
	value, ok := g.Get(logging.DeferredAPIResponseContextKey)
	if !ok {
		t.Fatal("no deferred ordinary metadata")
	}
	rendered := string(value.(logging.DeferredAPIResponse).RenderAPIResponse())
	if !strings.Contains(rendered, "Status: 429") || !strings.Contains(rendered, "Retry-After: 11") {
		t.Fatalf("metadata missing: %s", rendered)
	}
	for _, s := range []string{"private-cookie", "private-upstream-body", "private-user-prompt", "test-credential"} {
		if strings.Contains(rendered, s) {
			t.Fatal("private data in deferred log")
		}
	}
}

func TestCopilotOrdinaryInBandErrorLogsClassWithoutPayload(t *testing.T) {
	g, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx := context.WithValue(t.Context(), "gin", g)
	ctx = context.WithValue(ctx, "cliproxy.roundtripper", copilotOrdinaryFixtureTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"fixture-secret-response","code":"server_error"}}`)), Request: r}, nil
	}))
	model := "claude-opus-5"
	auth := copilotOrdinaryFixtureAuth(t, "account", model, copilot.Chat)
	req := cliproxyexecutor.Request{Model: model, Payload: []byte(`{"messages":[{"role":"user","content":"fixture-secret-request"}]}`)}
	_, err := NewCopilotExecutor(&config.Config{}).Execute(ctx, auth, req, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI})
	if err == nil {
		t.Fatal("in-band error lost")
	}
	value, ok := g.Get(logging.DeferredAPIResponseContextKey)
	if !ok {
		t.Fatal("missing bounded response log")
	}
	capture := value.(*logging.DeferredAPICapture)
	msg := string(capture.RenderAPIResponse())
	diagnostics := capture.ResponseDiagnostics()
	if len(diagnostics) != 1 || !strings.Contains(diagnostics[0].Error.Error(), "did not complete") {
		t.Fatal("missing bounded error classification")
	}
	for _, text := range []string{msg, diagnostics[0].Error.Error()} {
		if strings.Contains(text, "fixture-secret-response") || strings.Contains(text, "fixture-secret-request") {
			t.Fatal("private request or response content leaked")
		}
	}
}
