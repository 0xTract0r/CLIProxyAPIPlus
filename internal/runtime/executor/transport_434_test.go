package executor

import (
	"context"
	"errors"
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type captureCodexFallbackUsage434 struct {
	model   string
	records chan usage.Record
}

func (p *captureCodexFallbackUsage434) HandleUsage(_ context.Context, record usage.Record) {
	if record.Provider == "codex" && record.Model == p.model {
		select {
		case p.records <- record:
		default:
		}
	}
}

func TestCodexAutoExecutorMessageTooBigHTTPFallback434(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, phase := range []string{"prewarm", "main"} {
			for _, httpFails := range []bool{false, true} {
				for _, confuse := range []bool{false, true} {
					t.Run(fmt.Sprintf("stream=%t/%s/httpFails=%t/confuse=%t", stream, phase, httpFails, confuse), func(t *testing.T) {
						model := strings.ReplaceAll(t.Name(), "/", "-")
						plugin := &captureCodexFallbackUsage434{model: model, records: make(chan usage.Record, 4)}
						usage.RegisterPlugin(plugin)
						var wsCalls, httpCalls atomic.Int32
						capturedHTTP := make(chan []byte, 1)
						capturedWS := make(chan []byte, 1)
						upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
						server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							if !websocket.IsWebSocketUpgrade(r) {
								httpCalls.Add(1)
								body, errRead := io.ReadAll(r.Body)
								if errRead != nil {
									t.Errorf("read HTTP body: %v", errRead)
								}
								capturedHTTP <- body
								if r.Method != http.MethodPost || r.URL.Path != "/responses" {
									t.Errorf("unexpected HTTP request: %s %s", r.Method, r.URL.Path)
								}
								if got := gjson.Get(r.Header.Get("X-Codex-Turn-Metadata"), "turn_id").String(); got != codexIdentityConfuseUUID(model, "turn", "turn-original") {
									t.Errorf("HTTP identity header = %q", got)
								}
								w.Header().Set("X-Fallback-Transport", "http")
								if httpFails {
									http.Error(w, `{"error":{"message":"HTTP rejected","type":"invalid_request_error"}}`, http.StatusBadRequest)
									return
								}
								w.Header().Set("Content-Type", "text/event-stream")
								_, _ = io.WriteString(w, "data: "+`{"type":"response.completed","response":{"id":"resp_http","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"fallback ok"}]}],"usage":{"input_tokens":12,"output_tokens":2,"total_tokens":14}}}`+"\n\n")
								return
							}
							wsCalls.Add(1)
							conn, errUpgrade := upgrader.Upgrade(w, r, nil)
							if errUpgrade != nil {
								t.Errorf("upgrade WS: %v", errUpgrade)
								return
							}
							defer func() { _ = conn.Close() }()
							if _, body, errRead := conn.ReadMessage(); errRead != nil {
								t.Errorf("read prewarm: %v", errRead)
								return
							} else if !gjson.GetBytes(body, "generate").Exists() || gjson.GetBytes(body, "generate").Bool() {
								t.Errorf("expected generate:false prewarm: %s", body)
							}
							if phase == "main" {
								if errWrite := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp_warm","status":"completed","output":[]}}`)); errWrite != nil {
									t.Errorf("write prewarm completion: %v", errWrite)
									return
								}
								_, body, errRead := conn.ReadMessage()
								if errRead != nil {
									t.Errorf("read main: %v", errRead)
									return
								}
								capturedWS <- body
							}
							if errWrite := conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseMessageTooBig, "too big"), time.Now().Add(time.Second)); errWrite != nil {
								t.Errorf("write close1009: %v", errWrite)
							}
						}))
						t.Cleanup(server.Close)
						cfg := &config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}, Codex: config.CodexConfig{IdentityConfuse: confuse}, Routing: config.RoutingConfig{Strategy: "fill-first"}}
						exec := NewCodexAutoExecutor(cfg)
						auth := &cliproxyauth.Auth{ID: model, Provider: "codex", ProxyURL: "direct", Attributes: map[string]string{"api_key": "test", "base_url": server.URL, "fast_models": "*"}}
						text := strings.Repeat("complete original input ", 512)
						req := cliproxyexecutor.Request{Model: model, Payload: []byte(`{"model":"` + model + `","instructions":"original instructions","service_tier":"auto","prompt_cache_key":"original-cache","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"earlier turn"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"` + text + `"}]}],"client_metadata":{"x-codex-installation-id":"install-original","x-codex-turn-metadata":"{\"turn_id\":\"turn-original\"}"}}`)}
						opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("codex"), ResponseFormat: sdktranslator.FromString("codex"), OriginalRequest: append([]byte(nil), req.Payload...), Headers: http.Header{"X-Codex-Turn-Metadata": {`{"turn_id":"turn-original"}`}}}
						t.Cleanup(func() { exec.CloseExecutionSession(codexFastSessionFallbackID(opts, req)) })
						ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
						defer cancel()
						ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
						ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
						ginCtx.Request.Header = opts.Headers.Clone()
						ctx = context.WithValue(ctx, "gin", ginCtx)
						payload, headers, err := runCodexFallbackRequest434(ctx, exec, auth, req, opts, stream)
						if httpFails {
							var status cliproxyexecutor.StatusError
							if !errors.As(err, &status) || status.StatusCode() != http.StatusBadRequest {
								t.Fatalf("fallback error = %v, want HTTP400", err)
							}
							var scoped interface{ IsRequestScoped() bool }
							if !errors.As(err, &scoped) || !scoped.IsRequestScoped() {
								t.Fatal("HTTP recovery failure must remain request-scoped")
							}
						} else if err != nil || !strings.Contains(string(payload), "fallback ok") || headers.Get("X-Fallback-Transport") != "http" {
							t.Fatalf("fallback response: err=%v headers=%v payload=%s", err, headers, payload)
						}
						if gotWS, gotHTTP := wsCalls.Load(), httpCalls.Load(); gotWS != 1 || gotHTTP != 1 {
							t.Fatalf("transport calls WS=%d HTTP=%d, want 1 each", gotWS, gotHTTP)
						}
						body := <-capturedHTTP
						if got := gjson.GetBytes(body, "input.1.content.0.text").String(); got != text || len(gjson.GetBytes(body, "input").Array()) != 2 {
							t.Fatal("HTTP fallback truncated or replaced the original input")
						}
						if gjson.GetBytes(body, "service_tier").String() != "priority" || gjson.GetBytes(body, "instructions").String() != "original instructions" || gjson.GetBytes(body, "previous_response_id").Exists() || gjson.GetBytes(body, "generate").Exists() {
							t.Fatalf("HTTP fallback policy/context mismatch: %s", body)
						}
						wantCache := "original-cache"
						if confuse {
							wantCache = codexIdentityConfuseUUID(model, "prompt-cache", wantCache)
						}
						if got := gjson.GetBytes(body, "prompt_cache_key").String(); got != wantCache {
							t.Fatalf("fallback prompt-cache = %q, want %q", got, wantCache)
						}
						if got := gjson.GetBytes(body, "client_metadata.x-codex-installation-id").String(); got != codexIdentityConfuseUUID(model, "installation", "install-original") {
							t.Fatalf("fallback installation identity = %q", got)
						}
						if phase == "main" {
							wsBody := <-capturedWS
							if gjson.GetBytes(wsBody, "input").Raw != gjson.GetBytes(body, "input").Raw || gjson.GetBytes(wsBody, "previous_response_id").String() != "resp_warm" {
								t.Fatal("main1009 fixture did not reject the complete WS input")
							}
						}
						select {
						case record := <-plugin.records:
							if record.Failed != httpFails || record.Telemetry == nil || record.Telemetry.FastContext == nil || record.Telemetry.FastContext.UpstreamRequestServiceTier != "priority" {
								t.Fatalf("final HTTP usage outcome = %+v", record)
							}
						case <-ctx.Done():
							t.Fatal("missing final HTTP usage outcome")
						}
						select {
						case duplicate := <-plugin.records:
							t.Fatalf("duplicate WS failure usage: %+v", duplicate)
						case <-time.After(20 * time.Millisecond):
						}
						// Cleanup must leave the session lock available after HTTP fallback.
						wsURL, _ := buildCodexResponsesWebsocketURL(server.URL + "/responses")
						sess := exec.wsExec.getOrCreateSession(codexFastSessionFallbackID(opts, req), auth, wsURL)
						if !sess.reqMu.TryLock() {
							t.Fatal("HTTP fallback retained the websocket session lock")
						}
						sess.reqMu.Unlock()
					})
				}
			}
		}
	}
}

func runCodexFallbackRequest434(ctx context.Context, exec *CodexAutoExecutor, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, stream bool) ([]byte, http.Header, error) {
	if !stream {
		response, err := exec.Execute(ctx, auth, req, opts)
		return response.Payload, response.Headers, err
	}
	result, err := exec.ExecuteStream(ctx, auth, req, opts)
	if err != nil {
		return nil, nil, err
	}
	if result.InterruptOwner != cliproxyexecutor.StreamInterruptLocal {
		return nil, nil, fmt.Errorf("HTTP fallback provenance=%v", result.InterruptOwner)
	}
	var payload []byte
	for chunk := range result.Chunks {
		payload = append(payload, chunk.Payload...)
		if chunk.Err != nil {
			err = chunk.Err
		}
	}
	return payload, result.Headers, err
}
