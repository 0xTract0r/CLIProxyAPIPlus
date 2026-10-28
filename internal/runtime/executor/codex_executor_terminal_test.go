package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestCodexTerminalFailureStatusConnectionPressure(t *testing.T) {
	for _, tc := range []struct {
		name, event string
		want        int
	}{
		{"connection limit", `{"type":"error","error":{"type":"invalid_request_error","code":"websocket_connection_limit_reached","message":"connection limit"}}`, http.StatusServiceUnavailable},
		{"capacity message does not override code", `{"type":"error","error":{"type":"invalid_request_error","code":"websocket_connection_limit_reached","message":"Selected model is at capacity. Please try a different model."}}`, http.StatusServiceUnavailable},
		{"usage message does not override code", `{"type":"error","error":{"type":"invalid_request_error","code":"websocket_connection_limit_reached","message":"usage_limit_reached"}}`, http.StatusServiceUnavailable},
		{"failed response", `{"type":"response.failed","response":{"error":{"type":"invalid_request_error","code":"websocket_connection_limit_reached"}}}`, http.StatusServiceUnavailable},
		{"ordinary invalid request", `{"type":"error","error":{"type":"invalid_request_error","code":"invalid_parameter","message":"websocket_connection_limit_reached is not an error code here"}}`, http.StatusBadRequest},
		{"overload unchanged", `{"type":"error","error":{"type":"service_unavailable_error","code":"server_is_overloaded"}}`, http.StatusBadGateway},
		{"explicit status wins", `{"type":"error","error":{"type":"invalid_request_error","code":"websocket_connection_limit_reached","status_code":401}}`, http.StatusUnauthorized},
		{"explicit status with capacity message", `{"type":"error","error":{"type":"invalid_request_error","code":"websocket_connection_limit_reached","status":401,"message":"Selected model is at capacity"}}`, http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err, body, ok := codexTerminalFailureErr([]byte(tc.event))
			if !ok || len(body) == 0 {
				t.Fatal("terminal error was not recognized")
			}
			if err.StatusCode() != tc.want {
				t.Fatalf("status = %d, want %d", err.StatusCode(), tc.want)
			}
			if tc.want == http.StatusServiceUnavailable && gjson.Get(err.Error(), "error.code").String() != "websocket_connection_limit_reached" {
				t.Fatalf("connection pressure code lost: %s", err.Error())
			}
		})
	}
}

func TestCodexConnectionPressureNeverReplaysStartedResponse(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			var wsCalls, httpCalls atomic.Int32
			upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !websocket.IsWebSocketUpgrade(r) {
					httpCalls.Add(1)
					_, _ = io.WriteString(w, "unexpected HTTP replay")
					return
				}
				wsCalls.Add(1)
				conn, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer func() { _ = conn.Close() }()
				if _, _, err = conn.ReadMessage(); err != nil {
					t.Error(err)
					return
				}
				_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"pressure_fixture","status":"in_progress","output":[]}}`))
				_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"tool1","call_id":"call1","name":"fixture","arguments":""}}`))
				_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"error","error":{"type":"invalid_request_error","code":"websocket_connection_limit_reached","message":"connection limit"}}`))
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			exec := NewCodexAutoExecutor(&config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}})
			auth := &cliproxyauth.Auth{ID: t.Name(), Provider: "codex", ProxyURL: "direct", Attributes: map[string]string{"api_key": "fixture", "base_url": server.URL, "fast_models": ""}, Metadata: map[string]any{"websockets": true}}
			payload := []byte(`{"model":"gpt-6.1-sol","input":[{"role":"user","content":"synthetic test"}]}`)
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("codex"), ResponseFormat: sdktranslator.FromString("codex"), OriginalRequest: payload}
			_, _, err := runCodexFallbackRequest(ctx, exec, auth, cliproxyexecutor.Request{Model: "gpt-6.1-sol", Payload: payload}, opts, stream)
			var status interface{ StatusCode() int }
			if !errors.As(err, &status) || status.StatusCode() != http.StatusServiceUnavailable {
				t.Fatalf("got %v, want connection pressure status 503", err)
			}
			if wsCalls.Load() != 1 || httpCalls.Load() != 0 {
				t.Fatalf("unsafe replay: websocket=%d HTTP=%d", wsCalls.Load(), httpCalls.Load())
			}
		})
	}
}
