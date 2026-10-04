package executor

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestCodexStreamStagesRealWebsocket(t *testing.T) {
	for _, session := range []bool{false, true} {
		for _, mode := range []string{"completed", "read_error", "cancel"} {
			t.Run(fmt.Sprintf("session=%t/%s", session, mode), func(t *testing.T) {
				mainReceived := make(chan struct{})
				allowFirst := make(chan struct{})
				burstSent := make(chan struct{})
				releaseServer := make(chan struct{})
				var firstOnce, serverOnce sync.Once
				releaseFirst := func() { firstOnce.Do(func() { close(allowFirst) }) }
				release := func() { serverOnce.Do(func() { close(releaseServer) }) }
				defer releaseFirst()
				defer release()
				upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if !websocket.IsWebSocketUpgrade(r) {
						http.Error(w, "unexpected HTTP", 500)
						return
					}
					conn, err := upgrader.Upgrade(w, r, nil)
					if err != nil {
						return
					}
					defer func() { _ = conn.Close() }()
					if _, _, err = conn.ReadMessage(); err != nil {
						return
					}
					_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp_warm","status":"completed","output":[]}}`))
					_, body, err := conn.ReadMessage()
					if err != nil {
						return
					}
					if gjson.GetBytes(body, "service_tier").String() != "priority" {
						t.Error("priority changed")
					}
					close(mainReceived)
					<-allowFirst
					_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"resp_main","status":"in_progress","output":[]}}`))
					for i := 0; i < 64; i++ {
						_ = conn.WriteJSON(map[string]any{"type": "response.reasoning_summary_text.delta", "delta": fmt.Sprintf("synthetic-%d", i)})
					}
					if mode == "completed" {
						_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.output_text.delta","delta":"visible"}`))
						_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp_main","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"visible"}]}],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}`))
					}
					close(burstSent)
					if mode == "read_error" {
						_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "synthetic"), time.Now().Add(time.Second))
						return
					}
					<-releaseServer
				}))
				defer server.Close()
				cfg := &config.Config{SDKConfig: config.SDKConfig{RequestLog: true, DisableImageGeneration: config.DisableImageGenerationAll}}
				exec := NewCodexAutoExecutor(cfg)
				exec.wsExec.store = &codexWebsocketSessionStore{sessions: make(map[string]*codexWebsocketSession)}
				ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
				ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
				source, err := logging.NewFileBodySourceInDir(t.TempDir(), "dense")
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = source.Cleanup() }()
				ginCtx.Set(logging.APIWebsocketTimelineSourceContextKey, source)
				ctx, cancel := context.WithTimeout(context.WithValue(context.Background(), "gin", ginCtx), 3*time.Second)
				defer cancel()
				ctx, stages := helps.BeginCodexStreamStages(ctx, ginCtx)
				payload := []byte(`{"model":"gpt-5-codex","input":"synthetic input"}`)
				if session {
					payload = []byte(`{"model":"gpt-5-codex","prompt_cache_key":"stage-cache","input":"synthetic input"}`)
				}
				req := cliproxyexecutor.Request{Model: "gpt-5-codex", Payload: payload}
				opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("codex"), ResponseFormat: sdktranslator.FromString("codex")}
				auth := &cliproxyauth.Auth{ID: t.Name(), Provider: "codex", ProxyURL: "direct", Attributes: map[string]string{"api_key": "test", "base_url": server.URL, "fast_models": "*"}}
				defer exec.CloseExecutionSession(codexFastSessionFallbackID(opts, req))
				type result struct {
					stream *cliproxyexecutor.StreamResult
					err    error
				}
				ready := make(chan result, 1)
				go func() { stream, err := exec.ExecuteStream(ctx, auth, req, opts); ready <- result{stream, err} }()
				select {
				case <-mainReceived:
				case <-ctx.Done():
					t.Fatal("main not sent")
				}
				select {
				case <-ready:
					t.Fatal("peek returned before a real upstream frame")
				default:
				}
				releaseFirst()
				var stream *cliproxyexecutor.StreamResult
				select {
				case r := <-ready:
					if r.err != nil {
						t.Fatal(r.err)
					}
					stream = r.stream
				case <-ctx.Done():
					t.Fatal("first frame not delivered")
				}
				select {
				case <-burstSent:
				case <-ctx.Done():
					t.Fatal("dense upstream burst stalled")
				}
				var output strings.Builder
				var streamErr error
				if mode == "cancel" {
					cancel()
				}
				for chunk := range stream.Chunks {
					output.Write(chunk.Payload)
					if chunk.Err != nil {
						streamErr = chunk.Err
					}
				}
				stages.HandlerDone()
				release()
				v := stages.Snapshot()
				if !v.Active || v.MainAttempts != 1 || v.WireRead.Count == 0 || v.ConsumerWait.Count == 0 || v.Send.Count == 0 || v.Timeline.Count == 0 || v.TimelineStorage != "file" {
					t.Fatalf("missing actual stage counters: %+v", v)
				}
				if mode == "completed" {
					if streamErr != nil || !strings.Contains(output.String(), "visible") || v.Frames != 67 || v.Timeline.Count != 67 {
						t.Fatalf("dense stream changed: err=%v frames=%d timeline=%d", streamErr, v.Frames, v.Timeline.Count)
					}
				} else if mode == "read_error" {
					if streamErr == nil || v.Outcome != "read_error" {
						t.Fatalf("read error omitted: %v %+v", streamErr, v)
					}
				} else if streamErr != nil && !errors.Is(streamErr, context.Canceled) {
					t.Fatal(streamErr)
				}
				bytes, err := source.Bytes()
				if err != nil {
					t.Fatal(err)
				}
				if mode == "completed" && strings.Count(string(bytes), "Event: api.websocket.response") != 68 {
					t.Fatal("dense frame log bytes lost")
				}
				if len(source.Paths()) != 1 {
					t.Fatal("dense logging created one file per frame")
				}
			})
		}
	}
}
