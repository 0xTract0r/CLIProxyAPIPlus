package openai

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

type responsesTimingExecutor struct {
	prematureResponsesStreamExecutor
	chunk  coreexecutor.StreamChunk
	stages *helps.CodexStreamStages
}

func (*responsesTimingExecutor) Identifier() string { return "responses-timing-fixture" }

func (e *responsesTimingExecutor) ExecuteStream(ctx context.Context, _ *coreauth.Auth, _ coreexecutor.Request, _ coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	e.stages = helps.StartCodexMainStream(ctx, time.Now(), nil)
	defer e.stages.ExecutorDone("fixture_finished")
	chunks := make(chan coreexecutor.StreamChunk, 1)
	chunks <- e.chunk
	close(chunks)
	return &coreexecutor.StreamResult{Chunks: chunks}, nil
}

func TestResponsesHandlerRecordsInitialDeliveryStages(t *testing.T) {
	const model = "synthetic-response-timing"
	for _, tc := range []struct {
		name        string
		chunk       coreexecutor.StreamChunk
		body        string
		contentType string
		isError     bool
		wantFlush   uint64
	}{
		{
			name:  "first_frame",
			chunk: coreexecutor.StreamChunk{Payload: []byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"fixture\",\"status\":\"completed\",\"output\":[]}}\n\n")},
			body:  "event: response.completed", contentType: "text/event-stream", wantFlush: 1,
		},
		{
			name:  "initial_json_error",
			chunk: coreexecutor.StreamChunk{Err: errors.New("synthetic execution error")},
			body:  "synthetic execution error", contentType: "application/json", isError: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			executor := &responsesTimingExecutor{chunk: tc.chunk}
			manager := coreauth.NewManager(nil, nil, nil)
			manager.RegisterExecutor(executor)
			auth := &coreauth.Auth{ID: "synthetic-response-timing-auth", Provider: executor.Identifier(), Status: coreauth.StatusActive}
			if _, err := manager.Register(context.Background(), auth); err != nil {
				t.Fatal(err)
			}
			registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: model}})
			t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
			handler := NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager))
			router := gin.New()
			router.POST("/v1/responses", handler.Responses)

			req := httptest.NewRequest(http.MethodPost, "/v1/responses",
				strings.NewReader(`{"model":"synthetic-response-timing","input":"fixture","stream":true}`))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if (rec.Code >= 400) != tc.isError || !strings.HasPrefix(rec.Header().Get("Content-Type"), tc.contentType) ||
				!strings.Contains(rec.Body.String(), tc.body) {
				t.Fatalf("unexpected wire response: status=%d contentType=%q body=%q", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
			}
			if executor.stages == nil || !executor.stages.HandlerManaged() {
				t.Fatal("executor did not receive handler-owned timing state")
			}
			before := executor.stages.Snapshot()
			if !before.Active || before.Write.Count != 1 || before.Flush.Count != tc.wantFlush {
				t.Fatalf("timing counts: active=%t write=%d flush=%d, want write=1 flush=%d", before.Active, before.Write.Count, before.Flush.Count, tc.wantFlush)
			}
			time.Sleep(time.Millisecond)
			if after := executor.stages.Snapshot().Elapsed; before.Elapsed != after {
				t.Fatalf("handler exit did not finalize elapsed time: before=%v after=%v", before.Elapsed, after)
			}
		})
	}
}
