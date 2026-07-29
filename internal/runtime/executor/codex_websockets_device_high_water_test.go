package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

// newCodexWebsocketsServingHighWaterFixture is the WS analogue of
// newCodexServingHighWaterFixture (see codex_device_high_water_serving_test.go): it
// wires a Manager (with a capturing store) and a registered codex auth into a
// *CodexWebsocketsExecutor whose embedded *CodexExecutor carries the manager, so
// persistCodexDeviceHighWater (promoted from CodexExecutor) can resolve and raise
// the manager-side record. There is no NewCodexWebsocketsExecutorWithManager
// constructor, so the struct is built directly with the same fields
// NewCodexWebsocketsExecutor itself would set.
func newCodexWebsocketsServingHighWaterFixture(t *testing.T, serverURL string) (*CodexWebsocketsExecutor, *cliproxyauth.Auth, *codexServingHighWaterStore, *cliproxyauth.Manager) {
	t.Helper()
	helps.ResetCodexClientProfileCacheForTests()

	store := &codexServingHighWaterStore{}
	mgr := cliproxyauth.NewManager(store, nil, nil)

	const authID = "codex-ws-serving-hw-1"
	registered := &cliproxyauth.Auth{
		ID:       authID,
		Provider: "codex",
		Metadata: map[string]any{"type": "codex"},
		Attributes: map[string]string{
			"api_key":  "key-ws-serving-hw",
			"base_url": serverURL,
		},
	}
	if _, err := mgr.Register(context.Background(), registered); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	executor := &CodexWebsocketsExecutor{
		CodexExecutor: NewCodexExecutorWithManager(&config.Config{AuthDir: t.TempDir()}, mgr),
		store:         globalCodexWebsocketSessionStore,
	}
	servingAuth := &cliproxyauth.Auth{
		ID:       authID,
		ProxyURL: "direct",
		Provider: "codex",
		Attributes: map[string]string{
			"api_key":  "key-ws-serving-hw",
			"base_url": serverURL,
		},
	}
	return executor, servingAuth, store, mgr
}

// newCodexWebsocketsCompletingServer starts a minimal websocket upstream that reads
// one message and replies with a response.completed frame, so the WS
// Execute/ExecuteStream serving flow can return normally.
func newCodexWebsocketsCompletingServer(t *testing.T) *httptest.Server {
	t.Helper()
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, errUpgrade := upgrader.Upgrade(w, r, nil)
		if errUpgrade != nil {
			t.Errorf("upgrade websocket: %v", errUpgrade)
			return
		}
		defer func() { _ = conn.Close() }()

		if _, _, errRead := conn.ReadMessage(); errRead != nil {
			t.Errorf("read upstream websocket message: %v", errRead)
			return
		}
		completed := []byte(`{"type":"response.completed","response":{"id":"resp_1","object":"response","status":"completed","model":"gpt-5.4-mini","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`)
		if errWrite := conn.WriteMessage(websocket.TextMessage, completed); errWrite != nil {
			t.Errorf("write completed websocket message: %v", errWrite)
		}
	}))
}

// TestCodexWebsocketsExecutor_PersistsDeviceHighWater is the WS-serving-path
// analogue of TestCodexExecutor_Execute_PersistsDeviceHighWaterFromServingPath /
// TestCodexExecutor_ExecuteStream_PersistsDeviceHighWaterFromServingPath (see
// codex_device_high_water_serving_test.go). The G8 write-back call sites live in
// the WS transport, not (only) the HTTP one:
//   - codex_websockets_execute.go:95  e.persistCodexDeviceHighWater(ctx, auth)
//   - codex_websockets_stream.go:90   e.persistCodexDeviceHighWater(ctx, auth)
//
// The HTTP-path serving test cannot catch a merge that keeps
// persistCodexDeviceHighWater itself but drops either WS call site, because it
// never drives the WS transport. This test closes that gap by driving the real WS
// Execute/ExecuteStream flow with a version-bearing inbound codex CLI UA and
// asserting codex_device_high_water lands in auth.Metadata (and is actually
// persisted to the store), exactly like the HTTP counterpart.
//
// Red condition: delete `e.persistCodexDeviceHighWater(ctx, auth)` in
// codex_websockets_execute.go (~L95) or codex_websockets_stream.go (~L90). The
// corresponding subtest below then fails because auth.Metadata never carries
// codex_device_high_water and the store never observes a Save call.
//
// Level: executor-wiring (websocket transport).
func TestCodexWebsocketsExecutor_PersistsDeviceHighWater(t *testing.T) {
	t.Run("Execute", func(t *testing.T) {
		server := newCodexWebsocketsCompletingServer(t)
		defer server.Close()

		executor, auth, store, mgr := newCodexWebsocketsServingHighWaterFixture(t, server.URL)

		ctx := codexVersionedInboundContext("0.150.0")
		if _, err := executor.Execute(ctx, auth, cliproxyexecutor.Request{
			Model:   "gpt-5.4-mini",
			Payload: []byte(`{"model":"gpt-5.4-mini","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`),
		}, cliproxyexecutor.Options{
			SourceFormat: sdktranslator.FromString("openai-response"),
		}); err != nil {
			t.Fatalf("Execute returned error: %v", err)
		}

		assertCodexServingHighWaterPersisted(t, mgr, store, auth.ID, "0.150.0")
	})

	t.Run("ExecuteStream", func(t *testing.T) {
		server := newCodexWebsocketsCompletingServer(t)
		defer server.Close()

		executor, auth, store, mgr := newCodexWebsocketsServingHighWaterFixture(t, server.URL)

		ctx := codexVersionedInboundContext("0.160.0")
		result, err := executor.ExecuteStream(ctx, auth, cliproxyexecutor.Request{
			Model:   "gpt-5.4-mini",
			Payload: []byte(`{"model":"gpt-5.4-mini","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`),
		}, cliproxyexecutor.Options{
			SourceFormat: sdktranslator.FromString("openai-response"),
		})
		if err != nil {
			t.Fatalf("ExecuteStream returned error: %v", err)
		}

		drained := make(chan struct{})
		go func() {
			for range result.Chunks {
			}
			close(drained)
		}()
		select {
		case <-drained:
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for websocket stream completion")
		}

		assertCodexServingHighWaterPersisted(t, mgr, store, auth.ID, "0.160.0")
	})
}
