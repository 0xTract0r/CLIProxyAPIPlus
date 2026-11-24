package gemini

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

type geminiSingleGrantRefusalFixture struct {
	primary, alternate atomic.Int32
	status             int
}

func (*geminiSingleGrantRefusalFixture) Identifier() string { return "vertex" }
func (e *geminiSingleGrantRefusalFixture) Execute(_ context.Context, selected *coreauth.Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if selected.ID == "ordinary-gemini-a" {
		e.primary.Add(1)
	} else {
		e.alternate.Add(1)
	}
	return cliproxyexecutor.Response{}, geminiSingleGrantRefusal{status: e.status}
}
func (e *geminiSingleGrantRefusalFixture) CountTokens(ctx context.Context, selected *coreauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return e.Execute(ctx, selected, req, opts)
}
func (e *geminiSingleGrantRefusalFixture) ExecuteStream(ctx context.Context, selected *coreauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	_, err := e.Execute(ctx, selected, req, opts)
	return nil, err
}
func (*geminiSingleGrantRefusalFixture) Refresh(_ context.Context, selected *coreauth.Auth) (*coreauth.Auth, error) {
	return selected, nil
}
func (*geminiSingleGrantRefusalFixture) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("fixture has no outbound transport")
}

type geminiSingleGrantRefusal struct{ status int }

func (e geminiSingleGrantRefusal) Error() string              { return "synthetic provider refusal" }
func (e geminiSingleGrantRefusal) StatusCode() int            { return e.status }
func (e geminiSingleGrantRefusal) Headers() http.Header       { return http.Header{"Retry-After": {"75"}} }
func (e geminiSingleGrantRefusal) RetryAfter() *time.Duration { wait := 75 * time.Second; return &wait }

func TestGeminiStreamRefusalDoesNotDispatchAnAlternateGrant(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusTooManyRequests} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			engine := &geminiSingleGrantRefusalFixture{status: status}
			manager := coreauth.NewManager(nil, &coreauth.FillFirstSelector{}, nil)
			manager.RegisterExecutor(engine)
			const model = "gemini-ordinary-no-replay"
			for _, id := range []string{"ordinary-gemini-a", "ordinary-gemini-z"} {
				a := &coreauth.Auth{ID: id, Provider: "vertex", Status: coreauth.StatusActive, ProxyURL: "direct"}
				if id == "ordinary-gemini-a" {
					a.Attributes = map[string]string{"priority": "10"}
				}
				if _, err := manager.Register(t.Context(), a); err != nil {
					t.Fatal(err)
				}
				registry.GetGlobalRegistry().RegisterClient(id, a.Provider, []*registry.ModelInfo{{ID: model}})
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
			}
			h := NewGeminiAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{Streaming: sdkconfig.StreamingConfig{BootstrapRetries: 3}}, manager))
			router := gin.New()
			router.POST("/v1beta/models/*action", h.GeminiHandler)
			req := httptest.NewRequest(http.MethodPost, "/v1beta/models/"+model+":streamGenerateContent?alt=sse",
				strings.NewReader(`{"contents":[{"role":"user","parts":[{"text":"fixture"}]}]}`))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != status || rec.Header().Get("Retry-After") != "75" || engine.primary.Load() != 1 || engine.alternate.Load() != 0 {
				t.Fatalf("expected terminal %d and no additional dispatch, got HTTP %d, retry-after %q and %d/%d calls", status, rec.Code, rec.Header().Get("Retry-After"), engine.primary.Load(), engine.alternate.Load())
			}
		})
	}
}

func TestIssue7513_GeminiStreamAndNonStreamReturnHTTP502WhenRetriesExhausted(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var upstreamCalls atomic.Int32
	antigravityServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		if strings.Contains(r.URL.Path, "streamGenerateContent") {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, `data: {"response":{"candidates":[{"content":{"role":"model","parts":[]},"finishReason":"MALFORMED_FUNCTION_CALL"}],"usageMetadata":{"promptTokenCount":540000,"candidatesTokenCount":10,"thoughtsTokenCount":400,"totalTokenCount":540410},"modelVersion":"gemini-4-hydrogen"}}`+"\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"response":{"candidates":[{"content":{"role":"model","parts":[]},"finishReason":"MALFORMED_FUNCTION_CALL"}],"usageMetadata":{"promptTokenCount":540000,"candidatesTokenCount":10,"thoughtsTokenCount":400,"totalTokenCount":540410},"modelVersion":"gemini-4-hydrogen"}}`)
	}))
	defer antigravityServer.Close()

	cfg := &config.Config{}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor.NewAntigravityExecutor(cfg))

	model := "gemini-4-hydrogen-issue7513-exhausted"
	agAuth := &coreauth.Auth{
		ID:       "ag-issue7513-only",
		Provider: "antigravity",
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{
			"access_token":    "token-ag",
			"expired":         time.Now().Add(24 * time.Hour).Format(time.RFC3339),
			"project_id":      "project-ag",
			"disable_cooling": true,
		},
		Attributes: map[string]string{"base_url": antigravityServer.URL},
	}
	if _, errRegister := manager.Register(context.Background(), agAuth); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	registry.GetGlobalRegistry().RegisterClient(agAuth.ID, agAuth.Provider, []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(agAuth.ID)
	})

	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{
		Streaming: sdkconfig.StreamingConfig{
			BootstrapRetries: 1,
		},
	}, manager)
	h := NewGeminiAPIHandler(base)
	router := gin.New()
	router.POST("/v1beta/models/*action", h.GeminiHandler)

	streamReq := httptest.NewRequest(http.MethodPost, "/v1beta/models/"+model+":streamGenerateContent?alt=sse", strings.NewReader(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`))
	streamReq.Header.Set("Content-Type", "application/json")
	streamRec := httptest.NewRecorder()
	router.ServeHTTP(streamRec, streamReq)
	if streamRec.Code != http.StatusBadGateway {
		t.Fatalf("stream status = %d, want 502; body=%s", streamRec.Code, streamRec.Body.String())
	}
	if upstreamCalls.Load() < 2 {
		t.Fatalf("expected bootstrap retry to retry upstream at least once, got %d calls", upstreamCalls.Load())
	}

	nonStreamReq := httptest.NewRequest(http.MethodPost, "/v1beta/models/"+model+":generateContent", strings.NewReader(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`))
	nonStreamReq.Header.Set("Content-Type", "application/json")
	nonStreamRec := httptest.NewRecorder()
	router.ServeHTTP(nonStreamRec, nonStreamReq)
	if nonStreamRec.Code != http.StatusBadGateway {
		t.Fatalf("non-stream status = %d, want 502; body=%s", nonStreamRec.Code, nonStreamRec.Body.String())
	}
}
