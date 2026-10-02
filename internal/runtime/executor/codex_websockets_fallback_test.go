package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type captureCodexFallbackUsage struct {
	model   string
	records chan usage.Record
}

func (p *captureCodexFallbackUsage) HandleUsage(_ context.Context, record usage.Record) {
	if record.Provider == "codex" && record.Model == p.model {
		select {
		case p.records <- record:
		default:
		}
	}
}

func TestCodexAutoExecutorMessageTooBigHTTPFallback(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, phase := range []string{"prewarm", "main"} {
			for _, httpFails := range []bool{false, true} {
				for _, confuse := range []bool{false, true} {
					t.Run(fmt.Sprintf("stream=%t/%s/httpFails=%t/confuse=%t", stream, phase, httpFails, confuse), func(t *testing.T) {
						model := strings.ReplaceAll(t.Name(), "/", "-")
						plugin := &captureCodexFallbackUsage{model: model, records: make(chan usage.Record, 4)}
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
						payload, headers, err := runCodexFallbackRequest(ctx, exec, auth, req, opts, stream)
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

func TestCodexAutoExecutorMessageTooBigHTTPFallbackGuards(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, guard := range []string{"downstream-websocket", "required-websocket", "lifecycle", "previous-payload", "previous-original", "generate-false", "response-created", "output-started", "tool-frame", "empty-frame", "eof", "other-close", "prewarm-other-close"} {
			t.Run(fmt.Sprintf("stream=%t/%s", stream, guard), func(t *testing.T) {
				if guard == "downstream-websocket" || guard == "required-websocket" || guard == "lifecycle" || strings.HasPrefix(guard, "previous-") || guard == "generate-false" {
					t.Setenv(codexFastWebsocketMessageBytesEnv, "1")
				}
				var httpCalls, wsCalls atomic.Int32
				upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if !websocket.IsWebSocketUpgrade(r) {
						httpCalls.Add(1)
						http.Error(w, "forbidden fallback", http.StatusInternalServerError)
						return
					}
					wsCalls.Add(1)
					conn, errUpgrade := upgrader.Upgrade(w, r, nil)
					if errUpgrade != nil {
						t.Errorf("upgrade WS: %v", errUpgrade)
						return
					}
					defer func() { _ = conn.Close() }()
					if _, _, errRead := conn.ReadMessage(); errRead != nil {
						t.Errorf("read prewarm: %v", errRead)
						return
					}
					if guard != "prewarm-other-close" {
						_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp_warm","status":"completed","output":[]}}`))
						if _, _, errRead := conn.ReadMessage(); errRead != nil {
							t.Errorf("read main: %v", errRead)
							return
						}
					}
					if guard == "response-created" || guard == "output-started" {
						_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"resp_started","status":"in_progress","output":[]}}`))
						if guard == "output-started" {
							_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.output_text.delta","delta":"already sent"}`))
						}
					}
					if guard == "tool-frame" {
						_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","call_id":"call_1","name":"read","arguments":""}}`))
					}
					if guard == "empty-frame" {
						_ = conn.WriteMessage(websocket.TextMessage, []byte(" "))
					}
					if guard == "eof" {
						return
					}
					code := websocket.CloseMessageTooBig
					if guard == "other-close" || guard == "prewarm-other-close" {
						code = websocket.ClosePolicyViolation
					}
					_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, "rejected"), time.Now().Add(time.Second))
				}))
				t.Cleanup(server.Close)
				exec := NewCodexAutoExecutor(&config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}})
				auth := &cliproxyauth.Auth{ID: t.Name(), Provider: "codex", ProxyURL: "direct", Attributes: map[string]string{"api_key": "test", "base_url": server.URL, "fast_models": "*", "websockets": "true"}}
				req := cliproxyexecutor.Request{Model: "gpt-5-codex", Payload: []byte(`{"model":"gpt-5-codex","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"complete input"}]}]}`)}
				opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("codex"), ResponseFormat: sdktranslator.FromString("codex"), OriginalRequest: append([]byte(nil), req.Payload...), Metadata: map[string]any{cliproxyexecutor.ExecutionSessionMetadataKey: t.Name()}}
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				switch guard {
				case "downstream-websocket":
					ctx = cliproxyexecutor.WithDownstreamWebsocket(ctx)
				case "required-websocket":
					wsURL, _ := buildCodexResponsesWebsocketURL(server.URL + "/responses")
					sess := exec.wsExec.getOrCreateSession(t.Name(), auth, wsURL)
					if _, _, _, errDial := exec.wsExec.ensureUpstreamConn(ctx, auth, sess, auth.ID, wsURL, nil); errDial != nil {
						t.Fatalf("prime required WS connection: %v", errDial)
					}
					ctx = cliproxyexecutor.WithRequiredUpstreamWebsocket(ctx)
				case "lifecycle":
					opts.ExecutionLifecycle = newTerminalFailureLifecycle()
				case "previous-payload":
					req.Payload, _ = sjson.SetBytes(req.Payload, "previous_response_id", "resp_client_previous")
				case "previous-original":
					opts.OriginalRequest, _ = sjson.SetBytes(opts.OriginalRequest, "previous_response_id", "resp_client_previous")
				case "generate-false":
					req.Payload, _ = sjson.SetBytes(req.Payload, "generate", false)
				}
				t.Cleanup(func() { exec.CloseExecutionSession(t.Name()) })
				payload, _, err := runCodexFallbackRequest(ctx, exec, auth, req, opts, stream)
				if err == nil || httpCalls.Load() != 0 || wsCalls.Load() != 1 {
					t.Fatalf("guard failed: err=%v HTTP=%d WS=%d", err, httpCalls.Load(), wsCalls.Load())
				}
				if guard != "other-close" && guard != "prewarm-other-close" && guard != "eof" {
					var scoped cliproxyexecutor.RequestScopedError
					var status cliproxyexecutor.StatusError
					if !errors.As(err, &scoped) || !scoped.IsRequestScoped() || !errors.As(err, &status) || status.StatusCode() != http.StatusRequestEntityTooLarge {
						t.Fatalf("protected close1009 lost request-scoped HTTP413: %T %v", err, err)
					}
				}
				if stream && guard == "output-started" && !strings.Contains(string(payload), "already sent") {
					t.Fatalf("expected output before close1009: %s", payload)
				}
			})
		}
	}
}

func runCodexFallbackRequest(ctx context.Context, exec *CodexAutoExecutor, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, stream bool) ([]byte, http.Header, error) {
	if !stream {
		response, err := exec.Execute(ctx, auth, req, opts)
		return response.Payload, response.Headers, err
	}
	result, err := exec.ExecuteStream(ctx, auth, req, opts)
	if err != nil {
		return nil, nil, err
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

func TestCodexWebsocketHTTPFallbackFirstReadCancellationWithoutSession(t *testing.T) {
	requestRead := make(chan struct{})
	var httpCalls atomic.Int32
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !websocket.IsWebSocketUpgrade(r) {
			httpCalls.Add(1)
			http.Error(w, "unexpected fallback", http.StatusInternalServerError)
			return
		}
		conn, errUpgrade := upgrader.Upgrade(w, r, nil)
		if errUpgrade != nil {
			t.Errorf("upgrade WS: %v", errUpgrade)
			return
		}
		defer func() { _ = conn.Close() }()
		if _, _, errRead := conn.ReadMessage(); errRead != nil {
			t.Errorf("read request: %v", errRead)
			return
		}
		close(requestRead)
		_, _, _ = conn.ReadMessage()
	}))
	t.Cleanup(server.Close)
	exec := NewCodexWebsocketsExecutor(&config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}})
	auth := &cliproxyauth.Auth{Provider: "codex", ProxyURL: "direct", Attributes: map[string]string{"api_key": "test", "base_url": server.URL, "fast_models": "*"}}
	req := cliproxyexecutor.Request{Model: "gpt-5-codex", Payload: []byte(`{"model":"gpt-5-codex","input":"hello"}`)}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("codex")}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	finished := make(chan error, 1)
	go func() {
		_, err := exec.ExecuteStream(ctx, auth, req, opts)
		finished <- err
	}()
	select {
	case <-requestRead:
	case <-time.After(time.Second):
		t.Fatal("upstream did not receive request")
	}
	cancel()
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) || httpCalls.Load() != 0 {
			t.Fatalf("cancelled first read: err=%v HTTP=%d", err, httpCalls.Load())
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled sessionless first read did not return promptly")
	}
}

func TestCodexFastWebsocketMessageBudget(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  int
	}{
		{"", codexFastWebsocketDefaultMessageBytes}, {"0", codexFastWebsocketDefaultMessageBytes},
		{"-1", codexFastWebsocketDefaultMessageBytes}, {"garbage", codexFastWebsocketDefaultMessageBytes},
		{"999999999999999999999999", codexFastWebsocketDefaultMessageBytes}, {" 2048 ", 2048}, {"1", 1},
	} {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv(codexFastWebsocketMessageBytesEnv, tc.value)
			if got := codexFastWebsocketMessageBytes(); got != tc.want {
				t.Fatalf("budget=%d want %d", got, tc.want)
			}
		})
	}
	ctx := context.Background()
	if codexFastWebsocketSizeGate(ctx, []byte("1234"), 4, "test") || !codexFastWebsocketSizeGate(ctx, []byte("12345"), 4, "test") {
		t.Fatal("serialized byte boundary incorrect")
	}
	exec := NewCodexAutoExecutorWithManager(&config.Config{}, nil)
	if exec.wsExec.httpFallbackExecutor() != exec.httpExec {
		t.Fatal("HTTP recovery bypasses manager-aware executor")
	}
}

func TestCodexFastHTTPFallbackCancellationPhases(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, phase := range []string{"prewarm", "main", "after-frame"} {
			t.Run(fmt.Sprintf("stream=%t/%s", stream, phase), func(t *testing.T) {
				var httpCalls atomic.Int32
				ready := make(chan struct{})
				upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if !websocket.IsWebSocketUpgrade(r) {
						httpCalls.Add(1)
						http.Error(w, "forbidden", 500)
						return
					}
					conn, err := upgrader.Upgrade(w, r, nil)
					if err != nil {
						t.Errorf("upgrade: %v", err)
						return
					}
					defer func() { _ = conn.Close() }()
					if _, _, err := conn.ReadMessage(); err != nil {
						t.Errorf("prewarm read: %v", err)
						return
					}
					if phase != "prewarm" {
						_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp_warm","status":"completed","output":[]}}`))
						if _, _, err := conn.ReadMessage(); err != nil {
							t.Errorf("main read: %v", err)
							return
						}
						if phase == "after-frame" {
							_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"resp_main","status":"in_progress","output":[]}}`))
						}
					}
					close(ready)
					_, _, _ = conn.ReadMessage()
				}))
				t.Cleanup(server.Close)
				exec := NewCodexAutoExecutor(&config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}})
				auth := &cliproxyauth.Auth{Provider: "codex", ProxyURL: "direct", Attributes: map[string]string{"api_key": "test", "base_url": server.URL, "fast_models": "*"}}
				req := cliproxyexecutor.Request{Model: "gpt-5-codex", Payload: []byte(`{"model":"gpt-5-codex","input":"hello"}`)}
				opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("codex")}
				if codexFastSessionFallbackID(opts, req) != "" {
					t.Fatal("fixture must use no session")
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				finished := make(chan error, 1)
				go func() { _, _, err := runCodexFallbackRequest(ctx, exec, auth, req, opts, stream); finished <- err }()
				select {
				case <-ready:
				case <-time.After(time.Second):
					t.Fatal("upstream did not reach phase")
				}
				cancel()
				select {
				case err := <-finished:
					if (!stream || phase != "after-frame") && !errors.Is(err, context.Canceled) {
						t.Fatalf("cancel error=%v", err)
					}
					if httpCalls.Load() != 0 {
						t.Fatal("cancellation replayed over HTTP")
					}
				case <-time.After(time.Second):
					t.Fatal("sessionless cancellation blocked")
				}
			})
		}
	}
}

func TestCodexAutoExecutorFastHTTPSizeGate(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, variant := range []string{"images", "instructions", "tools", "prewarm-id"} {
			t.Run(fmt.Sprintf("stream=%t/%s", stream, variant), func(t *testing.T) {
				t.Setenv(codexFastWebsocketMessageBytesEnv, "4096")
				model := strings.ReplaceAll(t.Name(), "/", "-")
				plugin := &captureCodexFallbackUsage{model: model, records: make(chan usage.Record, 4)}
				usage.RegisterPlugin(plugin)
				var wsCalls, httpCalls, wsMessages atomic.Int32
				captured := make(chan []byte, 1)
				upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if websocket.IsWebSocketUpgrade(r) {
						wsCalls.Add(1)
						conn, err := upgrader.Upgrade(w, r, nil)
						if err != nil {
							t.Errorf("upgrade: %v", err)
							return
						}
						defer func() { _ = conn.Close() }()
						if _, _, err := conn.ReadMessage(); err != nil {
							t.Errorf("prewarm read: %v", err)
							return
						}
						wsMessages.Add(1)
						_ = conn.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{"id": strings.Repeat("r", 5000), "status": "completed", "output": []any{}}})
						if _, _, err := conn.ReadMessage(); err == nil {
							wsMessages.Add(1)
							t.Error("over-budget main was sent to WS")
						}
						return
					}
					httpCalls.Add(1)
					body, err := io.ReadAll(r.Body)
					if err != nil {
						t.Errorf("HTTP body: %v", err)
					}
					captured <- body
					if r.Header.Get("Authorization") != "Bearer test" {
						t.Error("HTTP auth changed")
					}
					w.Header().Set("X-Fallback-Transport", "http")
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: "+`{"type":"response.completed","response":{"id":"resp_http","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"size gate ok"}]}],"usage":{"input_tokens":21,"output_tokens":3,"total_tokens":24}}}`+"\n\n")
				}))
				t.Cleanup(server.Close)
				payload := []byte(`{"model":"` + model + `","instructions":"preserved","prompt_cache_key":"size-cache","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"complete history"}]}]}`)
				switch variant {
				case "images":
					items := []any{}
					for i := 0; i < 8; i++ {
						items = append(items, map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_image", "image_url": "data:image/jpeg;base64," + strings.Repeat(fmt.Sprintf("%02d", i), 512)}}})
					}
					payload, _ = sjson.SetBytes(payload, "input", items)
				case "instructions":
					payload, _ = sjson.SetBytes(payload, "instructions", strings.Repeat("preserved instruction ", 512))
				case "tools":
					payload, _ = sjson.SetBytes(payload, "tools", []any{map[string]any{"type": "function", "name": "large_tool", "description": strings.Repeat("preserved tool ", 512), "parameters": map[string]any{"type": "object"}}})
				}
				exec := NewCodexAutoExecutor(&config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}})
				auth := &cliproxyauth.Auth{ID: model, Provider: "codex", ProxyURL: "direct", Attributes: map[string]string{"api_key": "test", "base_url": server.URL, "fast_models": "*"}}
				req := cliproxyexecutor.Request{Model: model, Payload: payload}
				opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("codex"), ResponseFormat: sdktranslator.FromString("codex"), OriginalRequest: append([]byte(nil), payload...)}
				t.Cleanup(func() { exec.CloseExecutionSession(codexFastSessionFallbackID(opts, req)) })
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				out, headers, err := runCodexFallbackRequest(ctx, exec, auth, req, opts, stream)
				if err != nil || !strings.Contains(string(out), "size gate ok") || headers.Get("X-Fallback-Transport") != "http" {
					t.Fatalf("HTTP size gate: err=%v headers=%v", err, headers)
				}
				wantWS := int32(0)
				if variant == "prewarm-id" {
					wantWS = 1
				}
				if httpCalls.Load() != 1 || wsCalls.Load() != wantWS || wsMessages.Load() != wantWS {
					t.Fatalf("HTTP=%d WS=%d messages=%d", httpCalls.Load(), wsCalls.Load(), wsMessages.Load())
				}
				body := <-captured
				for _, key := range []string{"input", "instructions", "tools"} {
					if gjson.GetBytes(body, key).Raw != gjson.GetBytes(payload, key).Raw {
						t.Fatalf("HTTP changed original %s", key)
					}
				}
				if gjson.GetBytes(body, "service_tier").String() != "priority" || gjson.GetBytes(body, "prompt_cache_key").String() != "size-cache" || gjson.GetBytes(body, "previous_response_id").Exists() {
					t.Fatal("HTTP Fast policy/cache/context changed")
				}
				select {
				case record := <-plugin.records:
					if record.Failed || record.Detail.InputTokens != 21 || record.Detail.OutputTokens != 3 {
						t.Fatalf("usage=%+v", record)
					}
				case <-ctx.Done():
					t.Fatal("missing usage")
				}
				select {
				case record := <-plugin.records:
					t.Fatalf("duplicate WS usage=%+v", record)
				case <-time.After(20 * time.Millisecond):
				}
			})
		}
	}
}
