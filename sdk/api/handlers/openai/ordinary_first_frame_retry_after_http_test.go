package openai

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

type ordinaryFirstFrameRateLimit struct{}

func (ordinaryFirstFrameRateLimit) Error() string   { return "synthetic upstream wait" }
func (ordinaryFirstFrameRateLimit) StatusCode() int { return http.StatusTooManyRequests }
func (ordinaryFirstFrameRateLimit) RetryAfter() *time.Duration {
	delay := 61 * time.Second
	return &delay
}

type ordinaryFirstFrameRateLimitExecutor struct {
	initialFailureStreamExecutor
	calls atomic.Int32
}

func (e *ordinaryFirstFrameRateLimitExecutor) ExecuteStream(_ context.Context, a *coreauth.Auth, _ coreexecutor.Request, _ coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	if e.calls.Add(1) != 1 || a == nil || a.ID != "ordinary-retry-after-grant" {
		return nil, errors.New("unexpected execution or synthetic grant")
	}
	chunks := make(chan coreexecutor.StreamChunk, 1)
	chunks <- coreexecutor.StreamChunk{Err: ordinaryFirstFrameRateLimit{}}
	close(chunks)
	return &coreexecutor.StreamResult{Chunks: chunks}, nil
}

func TestOrdinaryChatFirstFrameRetryAfterHTTP(t *testing.T) {
	const path = "/v1/chat/completions"
	const model = "ordinary-first-frame-retry-after"
	exec := &ordinaryFirstFrameRateLimitExecutor{}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(exec)
	manager.SetRetryConfig(0, 0, 1)
	grant := &coreauth.Auth{ID: "ordinary-retry-after-grant", Provider: exec.Identifier(), Status: coreauth.StatusActive}
	if _, err := manager.Register(context.Background(), grant); err != nil {
		t.Fatal(err)
	}
	models := registry.GetGlobalRegistry()
	models.RegisterClient(grant.ID, grant.Provider, []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { models.UnregisterClient(grant.ID) })
	handler := NewOpenAIAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager))
	router := gin.New()
	router.POST(path, handler.ChatCompletions)
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"model":"ordinary-first-frame-retry-after","messages":[{"role":"user","content":"fixture"}],"stream":true}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	start := time.Now()
	router.ServeHTTP(response, request)
	minSeconds := 61 - int(time.Since(start)/time.Second)
	header := response.Header().Values("Retry-After")
	if response.Code != http.StatusTooManyRequests || len(header) != 1 {
		t.Fatalf("HTTP status=%d Retry-After=%v, want 429 with one wait value", response.Code, header)
	}
	seconds, err := strconv.Atoi(header[0])
	if err != nil || seconds <= 0 || seconds < minSeconds || seconds > 61 {
		t.Fatalf("HTTP Retry-After=%q, want provider deadline rounded up (remaining %d–61 seconds)", header[0], minSeconds)
	}
	if !strings.HasPrefix(response.Header().Get("Content-Type"), "application/json") ||
		!strings.Contains(response.Body.String(), "synthetic upstream wait") || exec.calls.Load() != 1 {
		t.Fatalf("HTTP must return terminal JSON and call only the selected grant once: status=%d calls=%d body=%q",
			response.Code, exec.calls.Load(), response.Body.String())
	}
	t.Logf("HTTP status=%d Retry-After=%q single call, header passthrough disabled", response.Code, header[0])
}
