package executor

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

// newCodexWebsocketsPathNormalizeExecutor mirrors newCodexWiringExecutor (see
// codex_path_normalize_wiring_test.go) but wires a *CodexWebsocketsExecutor whose
// embedded *CodexExecutor shares the same NormalizeAccountEnv-gated
// normalizeCodexPaths implementation. There is no NewCodexWebsocketsExecutorWithManager
// constructor, so the struct is built directly (same package, same fields
// NewCodexWebsocketsExecutor itself would set) with a manager-backed CodexExecutor.
func newCodexWebsocketsPathNormalizeExecutor(t *testing.T, serverURL string) (*CodexWebsocketsExecutor, *cliproxyauth.Auth, string) {
	t.Helper()
	helps.ResetCodexClientProfileCacheForTests()

	store := &codexServingHighWaterStore{}
	mgr := cliproxyauth.NewManager(store, nil, nil)

	const authID = "codex-ws-path-wiring-1"
	const apiKey = "key-ws-path-wiring"
	registered := &cliproxyauth.Auth{
		ID:       authID,
		Provider: "codex",
		Metadata: map[string]any{"type": "codex"},
		Attributes: map[string]string{
			"api_key":  apiKey,
			"base_url": serverURL,
		},
	}
	if _, err := mgr.Register(context.Background(), registered); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	// Switch ON: only then does e.normalizeCodexPaths / restoreCodexResponseCwd
	// wiring engage (see config.NormalizeAccountEnvEnabled). Same precondition as
	// the HTTP-path wiring fixture in codex_path_normalize_wiring_test.go.
	cfg := &config.Config{AuthDir: t.TempDir(), NormalizeAccountEnv: anticorrTruePtr()}
	if !config.NormalizeAccountEnvEnabled(cfg) {
		t.Fatal("test precondition: NormalizeAccountEnv switch must be ON for the wiring to engage")
	}
	executor := &CodexWebsocketsExecutor{
		CodexExecutor: NewCodexExecutorWithManager(cfg, mgr),
		store:         globalCodexWebsocketSessionStore,
	}
	auth := &cliproxyauth.Auth{
		ID:       authID,
		ProxyURL: "direct",
		Provider: "codex",
		Attributes: map[string]string{
			"api_key":  apiKey,
			"base_url": serverURL,
		},
	}
	return executor, auth, apiKey
}

// newCodexWebsocketsCapturingServer starts a websocket upstream that captures the
// single response.create message the WS executor sends, then replies with a
// minimal response.completed frame so Execute/ExecuteStream can return normally.
func newCodexWebsocketsCapturingServer(t *testing.T) (*httptest.Server, func() []byte) {
	t.Helper()
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	var mu sync.Mutex
	var capturedBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, errUpgrade := upgrader.Upgrade(w, r, nil)
		if errUpgrade != nil {
			t.Errorf("upgrade websocket: %v", errUpgrade)
			return
		}
		defer func() { _ = conn.Close() }()

		_, payload, errRead := conn.ReadMessage()
		if errRead != nil {
			t.Errorf("read upstream websocket message: %v", errRead)
			return
		}
		mu.Lock()
		capturedBody = bytes.Clone(payload)
		mu.Unlock()

		completed := []byte(`{"type":"response.completed","response":{"id":"resp_1","object":"response","status":"completed","model":"gpt-5.4-mini","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`)
		if errWrite := conn.WriteMessage(websocket.TextMessage, completed); errWrite != nil {
			t.Errorf("write completed websocket message: %v", errWrite)
		}
	}))
	getBody := func() []byte {
		mu.Lock()
		defer mu.Unlock()
		return capturedBody
	}
	return server, getBody
}

// assertCodexWebsocketOutboundBodyNormalized is the shared assertion for both the
// Execute and ExecuteStream subtests below: the real cwd sentinel must never reach
// the codex websocket upstream, and the per-account canonical cwd must be present
// (proving the outbound body was actually normalized, not merely emptied).
func assertCodexWebsocketOutboundBodyNormalized(t *testing.T, body []byte, auth *cliproxyauth.Auth, apiKey string) {
	t.Helper()
	if len(body) == 0 {
		t.Fatal("upstream captured no request body")
	}
	if bytes.Contains(body, []byte(codexWiringRealCwd)) {
		t.Fatalf("real cwd %q leaked to codex websocket upstream (normalizeCodexPaths call site missing in codex_websockets_execute.go / codex_websockets_stream.go):\n%s", codexWiringRealCwd, body)
	}
	canonical := helps.AccountCanonicalCwd(auth, apiKey)
	if !bytes.Contains(body, []byte(canonical)) {
		t.Fatalf("canonical cwd %q not present in outbound websocket body (normalization did not run):\n%s", canonical, body)
	}
}

// TestCodexWebsocketsExecutor_NormalizesOutboundCwd is the WS-serving-path analogue
// of TestCodexExecutor_Execute_NormalizesOutboundCwdOnServingPath (see
// codex_path_normalize_wiring_test.go). CodexWebsocketsExecutor embeds the same
// *CodexExecutor whose normalizeCodexPaths implements the fork(anticorr) F3 guard,
// but the WS Execute/ExecuteStream call sites are separate wiring:
//   - codex_websockets_execute.go:65   body = e.normalizeCodexPaths(ctx, body, auth, apiKey)
//   - codex_websockets_stream.go:62    body = e.normalizeCodexPaths(ctx, body, auth, apiKey)
//
// The HTTP-path wiring test cannot catch a merge that keeps the shared helper but
// drops either of these WS call sites, because it never drives the WS transport.
// This test closes that gap by dialing a real (httptest) websocket upstream and
// asserting the OUTBOUND wire frame contains no real cwd literal.
//
// Red condition: delete `body = e.normalizeCodexPaths(ctx, body, auth, apiKey)` in
// codex_websockets_execute.go (Execute, ~L65) or codex_websockets_stream.go
// (ExecuteStream, ~L62). The real cwd then leaks onto the websocket wire and the
// corresponding subtest below fails.
//
// Level: executor-wiring (websocket transport).
func TestCodexWebsocketsExecutor_NormalizesOutboundCwd(t *testing.T) {
	t.Run("Execute", func(t *testing.T) {
		server, getBody := newCodexWebsocketsCapturingServer(t)
		defer server.Close()

		executor, auth, apiKey := newCodexWebsocketsPathNormalizeExecutor(t, server.URL)

		if _, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
			Model:   "gpt-5.4-mini",
			Payload: codexWiringBody(),
		}, cliproxyexecutor.Options{
			SourceFormat: sdktranslator.FromString("openai-response"),
		}); err != nil {
			t.Fatalf("Execute returned error: %v", err)
		}

		assertCodexWebsocketOutboundBodyNormalized(t, getBody(), auth, apiKey)
	})

	t.Run("ExecuteStream", func(t *testing.T) {
		server, getBody := newCodexWebsocketsCapturingServer(t)
		defer server.Close()

		executor, auth, apiKey := newCodexWebsocketsPathNormalizeExecutor(t, server.URL)

		result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
			Model:   "gpt-5.4-mini",
			Payload: codexWiringBody(),
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

		assertCodexWebsocketOutboundBodyNormalized(t, getBody(), auth, apiKey)
	})
}
