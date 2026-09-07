package claude

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

type opusEntitlementHTTPExecutor struct {
	calls []string
	beta  string
}

func (*opusEntitlementHTTPExecutor) Identifier() string { return "claude" }
func (e *opusEntitlementHTTPExecutor) Execute(_ context.Context, a *coreauth.Auth, r coreexecutor.Request, o coreexecutor.Options) (coreexecutor.Response, error) {
	e.calls = append(e.calls, a.ID)
	e.beta = o.Headers.Get("Anthropic-Beta")
	body, _ := json.Marshal(map[string]any{"id": "msg_test", "type": "message", "role": "assistant", "model": r.Model, "content": []map[string]string{{"type": "text", "text": "OK"}}, "stop_reason": "end_turn", "usage": map[string]int{"input_tokens": 1, "output_tokens": 1}})
	return coreexecutor.Response{Payload: body}, nil
}
func (e *opusEntitlementHTTPExecutor) ExecuteStream(_ context.Context, a *coreauth.Auth, _ coreexecutor.Request, o coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	e.calls = append(e.calls, a.ID)
	e.beta = o.Headers.Get("Anthropic-Beta")
	ch := make(chan coreexecutor.StreamChunk, 1)
	ch <- coreexecutor.StreamChunk{Payload: []byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")}
	close(ch)
	return &coreexecutor.StreamResult{Chunks: ch}, nil
}
func (*opusEntitlementHTTPExecutor) Refresh(_ context.Context, a *coreauth.Auth) (*coreauth.Auth, error) {
	return a, nil
}
func (*opusEntitlementHTTPExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, nil
}
func (*opusEntitlementHTTPExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func TestClaudeMessagesOpus1MSubscriptionEligibility(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, withMax := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			for _, context1M := range []bool{false, true} {
				name := map[bool]string{false: "pro", true: "mixed"}[withMax] + map[bool]string{false: "/sync", true: "/stream"}[stream] + map[bool]string{false: "/ordinary", true: "/1m"}[context1M]
				t.Run(name, func(t *testing.T) {
					manager := coreauth.NewManager(nil, nil, nil)
					executor := &opusEntitlementHTTPExecutor{}
					manager.RegisterExecutor(executor)
					const model = "claude-opus-4-8"
					plans := []string{"pro"}
					if withMax {
						plans = append(plans, "max")
					}
					for _, plan := range plans {
						a := &coreauth.Auth{ID: "http-" + plan, Provider: "claude", Status: coreauth.StatusActive, ProxyURL: "http://test-proxy:8080", Attributes: map[string]string{"auth_kind": "oauth", "plan_type": plan, "priority": map[string]string{"pro": "100", "max": "0"}[plan]}, Metadata: map[string]any{"extra_usage_enabled": true}}
						if _, err := manager.Register(context.Background(), a); err != nil {
							t.Fatal(err)
						}
						registry.GetGlobalRegistry().RegisterClient(a.ID, "claude", []*registry.ModelInfo{{ID: model}})
						t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(a.ID) })
					}
					h := NewClaudeCodeAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager))
					router := gin.New()
					router.POST("/v1/messages", h.ClaudeMessages)
					body, _ := json.Marshal(map[string]any{"model": model, "max_tokens": 16, "stream": stream, "messages": []map[string]string{{"role": "user", "content": "hi"}}})
					req := httptest.NewRequest(http.MethodPost, "/v1/messages?beta=true", strings.NewReader(string(body)))
					req.Header.Set("Content-Type", "application/json")
					beta := "claude-code-20250219,interleaved-thinking-2025-05-14"
					if context1M {
						beta += ",context-1m-2025-08-07"
					}
					req.Header.Set("Anthropic-Beta", beta)
					resp := httptest.NewRecorder()
					router.ServeHTTP(resp, req)
					if context1M && !withMax {
						if resp.Code < 400 || len(executor.calls) != 0 {
							t.Fatalf("status=%d calls=%v body=%s", resp.Code, executor.calls, resp.Body.String())
						}
						return
					}
					want := "http-pro"
					if context1M {
						want = "http-max"
					}
					if resp.Code != 200 || len(executor.calls) != 1 || executor.calls[0] != want {
						t.Fatalf("status=%d calls=%v want=%s body=%s", resp.Code, executor.calls, want, resp.Body.String())
					}
					if executor.beta != beta {
						t.Fatal("wire beta changed")
					}
				})
			}
		}
	}
}
