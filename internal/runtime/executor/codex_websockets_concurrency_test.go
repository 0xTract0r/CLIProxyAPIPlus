package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestCodexFastConcurrentIndependentRequests(t *testing.T) {
	for _, streamA := range []bool{false, true} {
		for _, streamB := range []bool{false, true} {
			t.Run(fmt.Sprintf("A=%t/B=%t", streamA, streamB), func(t *testing.T) {
				startedA := make(chan struct{})
				releaseA := make(chan struct{})
				var releaseOnce sync.Once
				release := func() { releaseOnce.Do(func() { close(releaseA) }) }
				defer release()
				var connections, httpCalls atomic.Int32
				upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if !websocket.IsWebSocketUpgrade(r) {
						httpCalls.Add(1)
						http.Error(w, "unexpected HTTP", 500)
						return
					}
					connections.Add(1)
					conn, err := upgrader.Upgrade(w, r, nil)
					if err != nil {
						return
					}
					defer func() { _ = conn.Close() }()
					for {
						_, body, err := conn.ReadMessage()
						if err != nil {
							return
						}
						if gjson.GetBytes(body, "service_tier").String() != "priority" {
							t.Error("concurrent WS lost priority")
						}
						if gjson.GetBytes(body, "generate").Exists() && !gjson.GetBytes(body, "generate").Bool() {
							_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp_warm","status":"completed","output":[]}}`))
							continue
						}
						marker := gjson.GetBytes(body, "input.0.content.0.text").String()
						if gjson.GetBytes(body, "previous_response_id").String() != "resp_warm" {
							t.Error("concurrent WS lost prewarm link")
						}
						if marker == "A" {
							_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"resp_A","status":"in_progress","output":[]}}`))
							close(startedA)
							<-releaseA
						}
						_ = conn.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_" + marker, "status": "completed", "output": []any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "result_" + marker}}}}, "usage": map[string]any{"input_tokens": 2, "output_tokens": 1, "total_tokens": 3}}})
					}
				}))
				defer server.Close()
				exec := NewCodexAutoExecutor(&config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}})
				exec.wsExec.store = &codexWebsocketSessionStore{sessions: make(map[string]*codexWebsocketSession)}
				auth := &cliproxyauth.Auth{ID: t.Name(), Provider: "codex", ProxyURL: "direct", Attributes: map[string]string{"api_key": "test", "base_url": server.URL, "fast_models": "*"}}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				run := func(marker string, stream bool) <-chan error {
					out := make(chan error, 1)
					go func() {
						req := cliproxyexecutor.Request{Model: "gpt-5-codex", Payload: []byte(`{"model":"gpt-5-codex","prompt_cache_key":"shared","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"` + marker + `"}]}]}`)}
						opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("codex"), ResponseFormat: sdktranslator.FromString("codex")}
						payload, _, err := runCodexFallbackRequest(ctx, exec, auth, req, opts, stream)
						if err == nil && !strings.Contains(string(payload), "result_"+marker) {
							err = fmt.Errorf("response mixed: %s", payload)
						}
						out <- err
					}()
					return out
				}
				doneA := run("A", streamA)
				select {
				case <-startedA:
				case <-time.After(time.Second):
					t.Fatal("A did not reach main barrier")
				}
				doneB := run("B", streamB)
				select {
				case err := <-doneB:
					if err != nil {
						t.Fatalf("B: %v", err)
					}
				case <-time.After(time.Second):
					t.Fatal("independent B queued behind long A")
				}
				if connections.Load() != 2 || httpCalls.Load() != 0 {
					t.Fatalf("connections=%d HTTP=%d", connections.Load(), httpCalls.Load())
				}
				release()
				select {
				case err := <-doneA:
					if err != nil {
						t.Fatalf("A: %v", err)
					}
				case <-time.After(time.Second):
					t.Fatal("A did not complete")
				}
				select {
				case err := <-run("C", false):
					if err != nil {
						t.Fatalf("idle reuse: %v", err)
					}
				case <-time.After(time.Second):
					t.Fatal("idle reuse blocked")
				}
				if connections.Load() != 2 {
					t.Fatalf("idle request did not reuse base connection: %d", connections.Load())
				}
				exec.wsExec.store.mu.Lock()
				extras, sessions := exec.wsExec.store.extraCount, len(exec.wsExec.store.sessions)
				exec.wsExec.store.mu.Unlock()
				if extras != 0 || sessions != 1 {
					t.Fatalf("temporary session leaked: extra=%d sessions=%d", extras, sessions)
				}
				exec.CloseExecutionSession("codex-fast:pck:shared")
			})
		}
	}
}

type codexLeaseWaitContext struct {
	context.Context
	entered chan struct{}
	once    sync.Once
}

func (c *codexLeaseWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.entered) })
	return c.Context.Done()
}

func newCodexLeaseTestExecutor() *CodexWebsocketsExecutor {
	exec := NewCodexWebsocketsExecutor(&config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}})
	exec.store = &codexWebsocketSessionStore{sessions: make(map[string]*codexWebsocketSession)}
	return exec
}

func TestCodexFastSessionLeaseLimitsAndWake(t *testing.T) {
	exec := newCodexLeaseTestExecutor()
	ctx := context.Background()
	url := "ws://test/responses"
	auth := &cliproxyauth.Auth{ID: "one", ProxyURL: "direct"}
	base, err := exec.acquireSessionLease(ctx, "base", auth, url, true)
	if err != nil {
		t.Fatal(err)
	}
	defer base.release()
	extra, err := exec.acquireSessionLease(ctx, "base", auth, url, true)
	if err != nil || !extra.extra {
		t.Fatalf("extra=%v err=%v", extra, err)
	}
	defer extra.release()
	otherBase, err := exec.acquireSessionLease(ctx, "different-cache-key", auth, url, true)
	if err != nil {
		t.Fatal(err)
	}
	defer otherBase.release()
	waitCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	observed := &codexLeaseWaitContext{Context: waitCtx, entered: make(chan struct{})}
	done := make(chan *codexWebsocketSessionLease, 1)
	go func() {
		lease, err := exec.acquireSessionLease(observed, "different-cache-key", auth, url, true)
		if err != nil {
			t.Errorf("waiter: %v", err)
		}
		done <- lease
	}()
	select {
	case <-observed.entered:
	case <-time.After(time.Second):
		t.Fatal("per-target cap was not enforced across cache keys")
	}
	extra.release() // The base remains busy; wake must reconsider the extra slot.
	select {
	case lease := <-done:
		if lease == nil || !lease.extra {
			t.Fatal("did not use released extra slot")
		}
		lease.release()
	case <-time.After(time.Second):
		t.Fatal("extra release did not wake waiter")
	}
	otherBase.release()
	base.release() // Idempotent release must not double unlock/count.
	exec.store.mu.Lock()
	count := exec.store.extraCount
	exec.store.mu.Unlock()
	if count != 0 {
		t.Fatalf("extra permits=%d", count)
	}
}

func TestCodexFastSessionLeaseGlobalCapAndCancellation(t *testing.T) {
	exec := newCodexLeaseTestExecutor()
	ctx := context.Background()
	url := "ws://test/responses"
	var bases, extras []*codexWebsocketSessionLease
	defer func() {
		for _, l := range extras {
			l.release()
		}
		for _, l := range bases {
			l.release()
		}
	}()
	for i := 0; i < codexWebsocketExtraGlobal+1; i++ {
		auth := &cliproxyauth.Auth{ID: fmt.Sprintf("auth-%d", i), ProxyURL: "direct"}
		base, err := exec.acquireSessionLease(ctx, "same", auth, url, true)
		if err != nil {
			t.Fatal(err)
		}
		bases = append(bases, base)
		if i < codexWebsocketExtraGlobal {
			extra, err := exec.acquireSessionLease(ctx, "same", auth, url, true)
			if err != nil || !extra.extra {
				t.Fatalf("extra %d: %v", i, err)
			}
			extras = append(extras, extra)
		}
	}
	auth := &cliproxyauth.Auth{ID: fmt.Sprintf("auth-%d", codexWebsocketExtraGlobal), ProxyURL: "direct"}
	for _, cancelWait := range []bool{true, false} {
		waitCtx, cancel := context.WithCancel(ctx)
		observed := &codexLeaseWaitContext{Context: waitCtx, entered: make(chan struct{})}
		type outcome struct {
			lease *codexWebsocketSessionLease
			err   error
		}
		done := make(chan outcome, 1)
		go func() { l, err := exec.acquireSessionLease(observed, "same", auth, url, true); done <- outcome{l, err} }()
		select {
		case <-observed.entered:
		case <-time.After(time.Second):
			t.Fatal("global cap exceeded")
		}
		if cancelWait {
			cancel()
		} else {
			extras[0].release()
		}
		select {
		case result := <-done:
			if cancelWait {
				if !errors.Is(result.err, context.Canceled) {
					t.Fatalf("cancel=%v", result.err)
				}
			} else {
				if result.err != nil || !result.lease.extra {
					t.Fatalf("released global slot=%+v", result)
				}
				result.lease.release()
			}
		case <-time.After(time.Second):
			t.Fatal("global wait did not cancel/wake")
		}
		cancel()
	}
}

func TestCodexFastIndependentRequestGuards(t *testing.T) {
	base := []byte(`{"input":[{"role":"user","content":"complete"}]}`)
	for _, guard := range []string{"fast-off", "execution-session", "downstream", "required", "lifecycle", "previous-request", "previous-original", "previous-body", "generate-request", "generate-original", "empty", "missing", "reference", "unpaired-output", "unpaired-call", "computer-output", "shell-output", "approval", "unknown", "complete-tools"} {
		t.Run(guard, func(t *testing.T) {
			ctx := context.Background()
			req := cliproxyexecutor.Request{Payload: base}
			opts := cliproxyexecutor.Options{OriginalRequest: base}
			body := base
			fast := true
			switch guard {
			case "fast-off":
				fast = false
			case "execution-session":
				opts.Metadata = map[string]any{cliproxyexecutor.ExecutionSessionMetadataKey: "real"}
			case "downstream":
				ctx = cliproxyexecutor.WithDownstreamWebsocket(ctx)
			case "required":
				ctx = cliproxyexecutor.WithRequiredUpstreamWebsocket(ctx)
			case "lifecycle":
				opts.ExecutionLifecycle = newTerminalFailureLifecycle()
			case "previous-request":
				req.Payload, _ = sjson.SetRawBytes(base, "previous_response_id", []byte("null"))
			case "previous-original":
				opts.OriginalRequest, _ = sjson.SetBytes(base, "previous_response_id", "previous")
			case "previous-body":
				body, _ = sjson.SetBytes(base, "previous_response_id", "previous")
			case "generate-request":
				req.Payload, _ = sjson.SetBytes(base, "generate", false)
			case "generate-original":
				opts.OriginalRequest, _ = sjson.SetBytes(base, "generate", false)
			case "empty":
				body = []byte(`{"input":[]}`)
			case "missing":
				body = []byte(`{}`)
			case "reference":
				body = []byte(`{"input":[{"type":"item_reference","id":"previous"}]}`)
			case "unpaired-output":
				body = []byte(`{"input":[{"type":"function_call_output","call_id":"c1","output":"old"}]}`)
			case "unpaired-call":
				body = []byte(`{"input":[{"type":"function_call","call_id":"c1","name":"read","arguments":"{}"}]}`)
			case "computer-output", "shell-output", "approval", "unknown":
				typeName := map[string]string{"computer-output": "computer_call_output", "shell-output": "local_shell_call_output", "approval": "mcp_approval_response", "unknown": "future_tool"}[guard]
				body = []byte(`{"input":[{"type":"` + typeName + `","call_id":"c1","output":"old"}]}`)
			case "complete-tools":
				body = []byte(`{"input":[{"type":"function_call","call_id":"c1"},{"type":"custom_tool_call","call_id":"c2"},{"type":"custom_tool_call_output","call_id":"c2"},{"type":"function_call_output","call_id":"c1"},{"role":"user","content":"next"}]}`)
			}
			if got := codexFastIndependentRequest(ctx, req, opts, body, fast); got != (guard == "complete-tools") {
				t.Fatalf("guard=%s independent=%t", guard, got)
			}
		})
	}
	exec := newCodexLeaseTestExecutor()
	auth := &cliproxyauth.Auth{ID: "guard", ProxyURL: "direct"}
	baseLease, _ := exec.acquireSessionLease(context.Background(), "same", auth, "ws://test", true)
	defer baseLease.release()
	ctx, cancel := context.WithCancel(context.Background())
	observed := &codexLeaseWaitContext{Context: ctx, entered: make(chan struct{})}
	done := make(chan error, 1)
	go func() { _, err := exec.acquireSessionLease(observed, "same", auth, "ws://test", false); done <- err }()
	select {
	case <-observed.entered:
	case <-time.After(time.Second):
		t.Fatal("dependent request did not wait")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("dependent wait ignored cancellation")
	}
}

func TestCodexFastConcurrentLeaseErrorsRelease(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, phase := range []string{"dial", "426", "prewarm", "main-1009"} {
			t.Run(fmt.Sprintf("stream=%t/%s", stream, phase), func(t *testing.T) {
				exec := NewCodexAutoExecutor(&config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}})
				exec.wsExec.store = &codexWebsocketSessionStore{sessions: make(map[string]*codexWebsocketSession)}
				var httpCalls atomic.Int32
				upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if !websocket.IsWebSocketUpgrade(r) {
						httpCalls.Add(1)
						exec.wsExec.store.mu.Lock()
						count := exec.wsExec.store.extraCount
						exec.wsExec.store.mu.Unlock()
						if count != 0 {
							t.Error("HTTP recovery started before temporary lease release")
						}
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, "data: "+`{"type":"response.completed","response":{"id":"resp_http","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"recovered"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`+"\n\n")
						return
					}
					if phase == "dial" {
						http.Error(w, "mock rejected", 500)
						return
					}
					if phase == "426" {
						http.Error(w, "use HTTP", http.StatusUpgradeRequired)
						return
					}
					conn, err := upgrader.Upgrade(w, r, nil)
					if err != nil {
						return
					}
					defer func() { _ = conn.Close() }()
					_, _, err = conn.ReadMessage()
					if err != nil {
						return
					}
					code := websocket.ClosePolicyViolation
					if phase == "main-1009" {
						_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp_warm","status":"completed","output":[]}}`))
						_, _, err = conn.ReadMessage()
						if err != nil {
							return
						}
						code = websocket.CloseMessageTooBig
					}
					_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, "mock close"), time.Now().Add(time.Second))
				}))
				defer server.Close()
				auth := &cliproxyauth.Auth{ID: t.Name(), Provider: "codex", ProxyURL: "direct", Attributes: map[string]string{"api_key": "test", "base_url": server.URL, "fast_models": "*"}}
				req := cliproxyexecutor.Request{Model: "gpt-5-codex", Payload: []byte(`{"model":"gpt-5-codex","prompt_cache_key":"shared","input":[{"role":"user","content":"complete"}]}`)}
				opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("codex")}
				wsURL, _ := buildCodexResponsesWebsocketURL(server.URL + "/responses")
				base, err := exec.wsExec.acquireSessionLease(context.Background(), codexFastSessionFallbackID(opts, req), auth, wsURL, true)
				if err != nil {
					t.Fatal(err)
				}
				defer base.release()
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				payload, _, err := runCodexFallbackRequest(ctx, exec, auth, req, opts, stream)
				if phase == "main-1009" || phase == "426" {
					if err != nil || !strings.Contains(string(payload), "recovered") || httpCalls.Load() != 1 {
						t.Fatalf("recovery: %v HTTP=%d", err, httpCalls.Load())
					}
				} else if err == nil || httpCalls.Load() != 0 {
					t.Fatalf("fault did not fail closed: %v HTTP=%d", err, httpCalls.Load())
				}
				exec.wsExec.store.mu.Lock()
				extras, sessions := exec.wsExec.store.extraCount, len(exec.wsExec.store.sessions)
				exec.wsExec.store.mu.Unlock()
				if extras != 0 || sessions != 1 || base.session.reqMu.TryLock() {
					t.Fatalf("release invalid: extra=%d sessions=%d", extras, sessions)
				}
				base.release()
				exec.CloseExecutionSession(codexFastSessionFallbackID(opts, req))
			})
		}
	}
}

func TestCodexFastConcurrentLeaseCloseDuringDial(t *testing.T) {
	for _, byAuth := range []bool{false, true} {
		t.Run(fmt.Sprintf("byAuth=%t", byAuth), func(t *testing.T) {
			entered := make(chan struct{})
			allowResponse := make(chan struct{})
			var once sync.Once
			release := func() { once.Do(func() { close(allowResponse) }) }
			defer release()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(entered)
				<-allowResponse
				http.Error(w, "closed", 500)
			}))
			defer func() { release(); server.Close() }()
			exec := NewCodexWebsocketsExecutor(&config.Config{})
			auth := &cliproxyauth.Auth{ID: t.Name(), ProxyURL: "direct"}
			url, _ := buildCodexResponsesWebsocketURL(server.URL + "/responses")
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			base, err := exec.acquireSessionLease(ctx, t.Name(), auth, url, true)
			if err != nil {
				t.Fatal(err)
			}
			defer base.release()
			extra, err := exec.acquireSessionLease(ctx, t.Name(), auth, url, true)
			if err != nil {
				t.Fatal(err)
			}
			defer extra.release()
			done := make(chan error, 1)
			go func() {
				_, _, _, err := exec.ensureUpstreamConn(ctx, auth, extra.session, auth.ID, url, nil)
				done <- err
			}()
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("dial not started")
			}
			if byAuth {
				CloseCodexWebsocketSessionsForAuthID(auth.ID, "test_closed")
			} else {
				exec.CloseExecutionSession(t.Name())
			}
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("revoked dial succeeded")
				}
			case <-time.After(time.Second):
				t.Fatal("revocation did not cancel dial")
			}
			release()
			extra.session.connMu.Lock()
			conn, closed := extra.session.conn, extra.session.closed
			extra.session.connMu.Unlock()
			if conn != nil || !closed {
				t.Fatal("revoked session published a connection")
			}
			extra.release()
			base.release()
			exec.store.mu.Lock()
			for _, sess := range exec.store.sessions {
				if sess.sessionID == t.Name() {
					t.Error("revoked session remained indexed")
				}
			}
			exec.store.mu.Unlock()
		})
	}
}

func TestCodexFastConcurrentLeaseTargetIsolation(t *testing.T) {
	for _, change := range []string{"auth", "url", "proxy", "tls"} {
		t.Run(change, func(t *testing.T) {
			exec := newCodexLeaseTestExecutor()
			ctx := context.Background()
			url := "ws://one/responses"
			auth := &cliproxyauth.Auth{ID: "same-auth", Provider: "codex", ProxyURL: "direct"}
			base, _ := exec.acquireSessionLease(ctx, "cache-one", auth, url, true)
			defer base.release()
			extra, _ := exec.acquireSessionLease(ctx, "cache-one", auth, url, true)
			defer extra.release()
			other := &cliproxyauth.Auth{ID: auth.ID, Provider: "codex", ProxyURL: auth.ProxyURL}
			otherURL := url
			switch change {
			case "auth":
				other.ID = "different-auth"
			case "url":
				otherURL = "ws://two/responses"
			case "proxy":
				other.ProxyURL = "http://isolated.invalid:8080"
			case "tls":
				other.Metadata = map[string]any{"account_settings": map[string]any{"transport_profile": map[string]any{"preset": "provider-default"}, "tls_profile": map[string]any{"preset": "codex_go_http11_v1", "force_http11": true}}}
			}
			otherBase, err := exec.acquireSessionLease(ctx, "cache-two", other, otherURL, true)
			if err != nil {
				t.Fatal(err)
			}
			defer otherBase.release()
			limited, cancel := context.WithTimeout(ctx, time.Second)
			defer cancel()
			otherExtra, err := exec.acquireSessionLease(limited, "cache-two", other, otherURL, true)
			if err != nil || !otherExtra.extra {
				t.Fatalf("target isolation %s: %v", change, err)
			}
			defer otherExtra.release()
			if otherExtra.targetKey == extra.targetKey {
				t.Fatal("distinct targets shared permit bucket")
			}
		})
	}
}

func TestCodexFastConcurrentLeaseBaseReleaseWakesDependent(t *testing.T) {
	exec := newCodexLeaseTestExecutor()
	auth := &cliproxyauth.Auth{ID: "base-release", ProxyURL: "direct"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	base, _ := exec.acquireSessionLease(ctx, "same", auth, "ws://one", false)
	defer base.release()
	observed := &codexLeaseWaitContext{Context: ctx, entered: make(chan struct{})}
	done := make(chan *codexWebsocketSessionLease, 1)
	go func() {
		lease, err := exec.acquireSessionLease(observed, "same", auth, "ws://one", false)
		if err != nil {
			t.Errorf("dependent waiter: %v", err)
		}
		done <- lease
	}()
	select {
	case <-observed.entered:
	case <-time.After(time.Second):
		t.Fatal("dependent waiter did not queue")
	}
	base.release()
	select {
	case lease := <-done:
		if lease == nil || lease.extra {
			t.Fatal("dependent did not reuse base")
		}
		lease.release()
	case <-time.After(time.Second):
		t.Fatal("base release lost wakeup")
	}
}

func TestCodexWebsocketHandshakeCancellationOwnership(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		for {
			kind, body, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if conn.WriteMessage(kind, body) != nil {
				return
			}
		}
	}))
	defer server.Close()
	exec := newCodexLeaseTestExecutor()
	auth := &cliproxyauth.Auth{ProxyURL: "direct"}
	url, _ := buildCodexResponsesWebsocketURL(server.URL + "/responses")
	ctx, cancel := context.WithCancel(context.Background())
	conn, closer, _, err := exec.dialCodexWebsocket(ctx, auth, url, nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	defer func() { _ = closer.Close() }()
	cancel() // The handshake callback must no longer own the established socket.
	if err := conn.WriteMessage(websocket.TextMessage, []byte("owned")); err != nil {
		t.Fatalf("handshake cancel closed handed-off socket: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	_, body, err := conn.ReadMessage()
	if err != nil || string(body) != "owned" {
		t.Fatalf("ownership echo: %s %v", body, err)
	}
}

type codexCancelAfterLeaseContext struct {
	context.Context
	cancel context.CancelFunc
	checks int
}

func (c *codexCancelAfterLeaseContext) Err() error {
	c.checks++
	if c.checks == 2 {
		c.cancel()
	}
	return c.Context.Err()
}

func TestCodexFastConcurrentLeaseCanceledAfterAcquisition(t *testing.T) {
	for _, extra := range []bool{false, true} {
		t.Run(fmt.Sprintf("extra=%t", extra), func(t *testing.T) {
			exec := newCodexLeaseTestExecutor()
			auth := &cliproxyauth.Auth{ID: "cancel-acquired", ProxyURL: "direct"}
			url := "ws://test/responses"
			var held *codexWebsocketSessionLease
			if extra {
				held, _ = exec.acquireSessionLease(context.Background(), "same", auth, url, true)
				defer held.release()
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			controlled := &codexCancelAfterLeaseContext{Context: ctx, cancel: cancel}
			lease, err := exec.acquireSessionLease(controlled, "same", auth, url, true)
			if !errors.Is(err, context.Canceled) || lease != nil {
				t.Fatalf("acquisition cancellation: %v %v", lease, err)
			}
			exec.store.mu.Lock()
			count := exec.store.extraCount
			sessions := len(exec.store.sessions)
			exec.store.mu.Unlock()
			if count != 0 || sessions != 1 {
				t.Fatalf("cancelled lease leaked: extra=%d sessions=%d", count, sessions)
			}
			if !extra {
				base := exec.getOrCreateSession("same", auth, url)
				if !base.reqMu.TryLock() {
					t.Fatal("cancelled base stayed locked")
				}
				base.reqMu.Unlock()
			}
		})
	}
}
