package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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

// The first connection keeps an unfinished main response alive after cancellation.
// Reusing it delivers that response to the next prewarm, then leaks the empty
// prewarm completion to the next main: the production 11-input/0-output failure.
func TestCodexFastCanceledTurnDoesNotContaminateNextTurn(t *testing.T) {
	for _, mode := range []string{"execute", "stream_read", "stream_delivery"} {
		t.Run(mode, func(t *testing.T) {
			var connections atomic.Int32
			mainSeen := make(chan struct{}, 1)
			upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer func() { _ = conn.Close() }()
				n := connections.Add(1)
				if !fastTurnReadPrewarm(t, conn) || !fastTurnComplete(conn, fmt.Sprintf("warm_%d", n), true) {
					return
				}
				if _, _, errRead := conn.ReadMessage(); errRead != nil {
					return
				}
				if n == 1 {
					if errWrite := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"old_main","status":"in_progress","output":[]}}`)); errWrite != nil {
						return
					}
					mainSeen <- struct{}{}
					// Fixed behavior closes this connection. Old behavior sends the next
					// prewarm on it, so reproduce the out-of-phase completions exactly.
					if !fastTurnReadPrewarm(t, conn) {
						return
					}
					if !fastTurnComplete(conn, "old_main", false) || !fastTurnComplete(conn, "leaked_warmup", true) {
						return
					}
					if _, _, errRead := conn.ReadMessage(); errRead != nil {
						return
					}
				} else if !fastTurnComplete(conn, "fresh_main", false) {
					return
				}
				for {
					if _, _, errRead := conn.ReadMessage(); errRead != nil {
						return
					}
				}
			}))
			t.Cleanup(server.Close)
			exec, auth, req, opts := fastTurnExecutor(t, server.URL)
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			finished := make(chan error, 1)
			if mode == "execute" {
				go func() {
					_, err := exec.Execute(ctx, auth, req, opts)
					finished <- err
				}()
			} else {
				result, err := exec.ExecuteStream(ctx, auth, req, opts)
				if err != nil {
					t.Fatalf("first ExecuteStream: %v", err)
				}
				if mode == "stream_read" {
					select {
					case chunk := <-result.Chunks:
						if chunk.Err != nil {
							t.Fatalf("first chunk: %v", chunk.Err)
						}
					case <-time.After(5 * time.Second):
						t.Fatal("first chunk did not arrive")
					}
				}
				// Leave stream_delivery blocked on its first downstream write.
				go func() {
					<-ctx.Done()
					for range result.Chunks {
					}
					finished <- ctx.Err()
				}()
			}
			select {
			case <-mainSeen:
			case <-time.After(5 * time.Second):
				t.Fatal("first main was not sent")
			}
			cancel()
			select {
			case err := <-finished:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("first turn error = %v, want cancellation", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("canceled turn did not release its session")
			}

			ctxNext, cancelNext := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancelNext()
			payload := fastTurnRun(t, exec, ctxNext, auth, req, opts, mode != "execute")
			if !bytes.Contains(payload, []byte("fresh-main")) {
				t.Errorf("next turn returned a stale/prewarm completion instead of fresh content: %s", payload)
			}
			if got := connections.Load(); got != 2 {
				t.Errorf("connections = %d, want 2 after an incomplete main turn", got)
			}
		})
	}
}

// A fully consumed terminal event makes a connection safe to reuse, including a
// legitimate empty business response. Cancellation after that boundary is harmless.
func TestCodexFastCompletedTurnKeepsReusableConnection(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream_%t", stream), func(t *testing.T) {
			var connections atomic.Int32
			upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer func() { _ = conn.Close() }()
				connections.Add(1)
				for turn := 0; ; turn++ {
					if !fastTurnReadPrewarm(t, conn) || !fastTurnComplete(conn, fmt.Sprintf("warm_%d", turn), true) {
						return
					}
					if _, _, errRead := conn.ReadMessage(); errRead != nil {
						return
					}
					if !fastTurnComplete(conn, fmt.Sprintf("main_%d", turn), turn == 0) {
						return
					}
				}
			}))
			t.Cleanup(server.Close)
			exec, auth, req, opts := fastTurnExecutor(t, server.URL)
			for turn := 0; turn < 2; turn++ {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				payload := fastTurnRun(t, exec, ctx, auth, req, opts, stream)
				cancel()
				if turn == 1 && !bytes.Contains(payload, []byte("fresh-main")) {
					t.Fatalf("second completed turn lost content: %s", payload)
				}
			}
			if got := connections.Load(); got != 1 {
				t.Fatalf("connections = %d, want successful-turn reuse", got)
			}
		})
	}
}

func fastTurnExecutor(t *testing.T, baseURL string) (*CodexAutoExecutor, *cliproxyauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) {
	t.Helper()
	exec := NewCodexAutoExecutor(&config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}})
	auth := &cliproxyauth.Auth{ID: t.Name(), Provider: "codex", Attributes: map[string]string{"api_key": "sk-test", "base_url": baseURL, "fast_models": "*"}}
	req := cliproxyexecutor.Request{Model: "gpt-5-codex", Payload: []byte(`{"model":"gpt-5-codex","input":[{"role":"user","content":"continue"}],"prompt_cache_key":"fast-turn-test"}`)}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("codex"), ResponseFormat: sdktranslator.FromString("codex")}
	t.Cleanup(func() { exec.CloseExecutionSession(codexFastSessionFallbackID(opts, req)) })
	return exec, auth, req, opts
}

func fastTurnReadPrewarm(t *testing.T, conn *websocket.Conn) bool {
	t.Helper()
	_, body, err := conn.ReadMessage()
	if err != nil {
		return false
	}
	if generate := gjson.GetBytes(body, "generate"); !generate.Exists() || generate.Bool() {
		t.Errorf("expected prewarm, received %s", body)
		return false
	}
	return true
}

func fastTurnComplete(conn *websocket.Conn, id string, empty bool) bool {
	output, inputTokens, outputTokens := `[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"fresh-main"}]}]`, 100, 8
	if empty {
		output, inputTokens, outputTokens = `[]`, 11, 0
		created := fmt.Sprintf(`{"type":"response.created","response":{"id":%q,"status":"in_progress","output":[]}}`, id)
		if conn.WriteMessage(websocket.TextMessage, []byte(created)) != nil {
			return false
		}
	}
	body := fmt.Sprintf(`{"type":"response.completed","response":{"id":%q,"status":"completed","output":%s,"usage":{"input_tokens":%d,"output_tokens":%d,"total_tokens":%d}}}`, id, output, inputTokens, outputTokens, inputTokens+outputTokens)
	return conn.WriteMessage(websocket.TextMessage, []byte(body)) == nil
}

func fastTurnRun(t *testing.T, exec *CodexAutoExecutor, ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, stream bool) []byte {
	t.Helper()
	if !stream {
		response, err := exec.Execute(ctx, auth, req, opts)
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		return response.Payload
	}
	result, err := exec.ExecuteStream(ctx, auth, req, opts)
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}
	var payload []byte
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream: %v", chunk.Err)
		}
		payload = append(payload, chunk.Payload...)
	}
	return payload
}
