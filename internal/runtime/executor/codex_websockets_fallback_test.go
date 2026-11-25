package executor

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestCodexWebsocketFirstReadCancellationWithoutSession(t *testing.T) {
	requestRead, peerClosed := make(chan struct{}), make(chan struct{})
	var httpCalls atomic.Int32
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !websocket.IsWebSocketUpgrade(r) {
			httpCalls.Add(1)
			http.Error(w, "unexpected fallback", http.StatusInternalServerError)
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		if _, _, err = conn.ReadMessage(); err != nil {
			t.Error(err)
			return
		}
		close(requestRead)
		_, _, _ = conn.ReadMessage()
		close(peerClosed)
	}))
	defer server.Close()
	executor := NewCodexWebsocketsExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Provider: "codex", ProxyURL: "direct", Attributes: map[string]string{"api_key": "test", "base_url": server.URL}}
	req := cliproxyexecutor.Request{Model: "gpt-5-codex", Payload: []byte(`{"model":"gpt-5-codex","input":"hello"}`)}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	result, err := executor.ExecuteStream(ctx, auth, req, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatCodex})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-requestRead:
	case <-ctx.Done():
		t.Fatal("provider fixture did not receive its initial frame")
	}
	cancel()
	select {
	case chunk, ok := <-result.Chunks:
		if ok && !errors.Is(chunk.Err, context.Canceled) {
			t.Fatalf("canceled stream returned visible output: %+v", chunk)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("canceled stream did not close")
	}
	select {
	case <-peerClosed:
	case <-time.After(2 * time.Second):
		t.Fatal("canceled sessionless connection remained open")
	}
	if httpCalls.Load() != 0 {
		t.Fatal("canceled websocket was replayed over HTTP")
	}
}
