package executor

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	log "github.com/sirupsen/logrus"
)

type ordinaryStageLogCapture struct {
	mu       sync.Mutex
	messages []string
}

func (*ordinaryStageLogCapture) Levels() []log.Level { return []log.Level{log.InfoLevel} }
func (c *ordinaryStageLogCapture) Fire(entry *log.Entry) error {
	if strings.HasPrefix(entry.Message, "codex stream stages ") {
		c.mu.Lock()
		c.messages = append(c.messages, entry.Message)
		c.mu.Unlock()
	}
	return nil
}

func TestCodexStreamStagesRealWebsocket(t *testing.T) {
	logger := log.StandardLogger()
	capture := &ordinaryStageLogCapture{}
	old := logger.ReplaceHooks(make(log.LevelHooks))
	logger.AddHook(capture)
	t.Cleanup(func() { logger.ReplaceHooks(old) })

	for _, tc := range []struct {
		name, outcome string
		frames        uint64
		visible       bool
		turns         int
	}{
		{"completed", "completed", 3, true, 2},
		{"read_error", "read_error", 1, false, 1},
		{"terminal_failure", "terminal_failure", 1, false, 1},
		{"cancel", "context_done", 1, false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var handshakes, requests atomic.Int32
			received := make(chan struct{}, tc.turns)
			sendFirst := make(chan struct{}, tc.turns)
			stop := make(chan struct{})
			upgrader := websocket.Upgrader{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				handshakes.Add(1)
				conn, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.Close()
				for range tc.turns {
					if _, _, err := conn.ReadMessage(); err != nil {
						t.Error(err)
						return
					}
					requests.Add(1)
					received <- struct{}{}
					select {
					case <-stop:
						return
					case <-sendFirst:
					}
					if tc.name == "terminal_failure" {
						_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"error","status":400,"error":{"code":"invalid_request","message":"sensitive-fixture-error"}}`))
						return
					}
					_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"fixture","status":"in_progress"}}`))
					switch tc.name {
					case "read_error":
						_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "sensitive-fixture-error"), time.Now().Add(time.Second))
						return
					case "cancel":
						<-stop
						return
					default:
						_ = conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(time.Second))
						_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.output_text.delta","delta":"sensitive-fixture-text"}`))
						_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"fixture","status":"completed","output":[]}}`))
					}
				}
			}))
			defer server.Close()
			defer close(stop)
			executor := NewCodexWebsocketsExecutor(&config.Config{})
			executor.store = &codexWebsocketSessionStore{sessions: make(map[string]*codexWebsocketSession)}
			defer executor.CloseExecutionSession(t.Name())
			auth := &cliproxyauth.Auth{ID: "fixture-grant", Provider: "codex", ProxyURL: "direct",
				Attributes: map[string]string{"api_key": "sensitive-fixture-secret", "base_url": server.URL}}
			req := cliproxyexecutor.Request{Model: "fixture-model", Payload: []byte(`{"model":"fixture-model","input":"synthetic"}`)}
			opts := cliproxyexecutor.Options{
				SourceFormat: sdktranslator.FormatCodex,
				Metadata:     map[string]any{cliproxyexecutor.ExecutionSessionMetadataKey: t.Name()},
			}
			for range tc.turns {
				ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
				defer cancel()
				ctx = fixtureCodexTenantContext(cliproxyexecutor.WithDownstreamWebsocket(ctx))
				ctx, stages := helps.BeginCodexStreamStages(ctx, nil)
				result, err := executor.ExecuteStream(ctx, auth, req, opts)
				if err != nil {
					t.Fatal(err)
				}
				select {
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				case <-received:
				}
				select {
				case <-result.Chunks:
					t.Fatal("stream emitted before the upstream response")
				default:
				}
				sendFirst <- struct{}{}
				var publishedError bool
				for chunk := range result.Chunks {
					if tc.name == "cancel" {
						if chunk.Err != nil && !errors.Is(chunk.Err, context.Canceled) {
							t.Fatalf("cancellation changed the ordinary error: %v", chunk.Err)
						}
						cancel()
					} else {
						publishedError = publishedError || chunk.Err != nil
					}
				}
				stages.HandlerDone()
				v := stages.Snapshot()
				if !v.Active || v.MainAttempts != 1 || v.Frames != tc.frames ||
					v.WireRead.Count < tc.frames || v.ConsumerWait.Count < tc.frames ||
					v.Send.Count == 0 || v.TimelineStorage != "disabled" {
					t.Fatalf("ordinary stream counters: %+v", v)
				}
				if v.FirstVisibleObserved != tc.visible || (tc.visible && v.FirstVisible <= 0) || v.Outcome != tc.outcome {
					t.Fatalf("wrong content or terminal diagnostic: %+v", v)
				}
				if tc.name == "completed" && v.Liveness.PingReceived != 1 {
					t.Fatalf("ping not attributed to main turn: %+v", v.Liveness)
				}
				if (tc.name == "read_error" || tc.name == "terminal_failure") != publishedError {
					t.Fatalf("unexpected stream error delivery: %t", publishedError)
				}
			}
			if handshakes.Load() != 1 || int(requests.Load()) != tc.turns {
				t.Fatalf("unexpected upstream handshake/request count: %d/%d", handshakes.Load(), requests.Load())
			}
		})
	}
	capture.mu.Lock()
	defer capture.mu.Unlock()
	if len(capture.messages) != 5 {
		t.Fatalf("diagnostic summaries = %d, want 5", len(capture.messages))
	}
	for _, summary := range capture.messages {
		if strings.Contains(summary, "sensitive-fixture") || !strings.Contains(summary, "first_visible_observed=") {
			t.Fatalf("summary omitted stage metadata or included private content")
		}
	}
}
