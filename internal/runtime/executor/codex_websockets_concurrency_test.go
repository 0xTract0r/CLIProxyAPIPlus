package executor

import (
	"context"

	"net/http"
	"net/http/httptest"

	"testing"
	"time"

	"github.com/gorilla/websocket"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

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
	exec := newOrdinaryCodexSetupTestExecutor()
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
