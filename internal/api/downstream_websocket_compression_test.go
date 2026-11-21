package api

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers/openai"
)

type downstreamCompressionPipeListener struct {
	net.Conn
	accepted  bool
	done      chan struct{}
	closeOnce sync.Once
}

func (l *downstreamCompressionPipeListener) Accept() (net.Conn, error) {
	if !l.accepted {
		l.accepted = true
		return l.Conn, nil
	}
	<-l.done
	return nil, net.ErrClosed
}

func (l *downstreamCompressionPipeListener) Close() error {
	l.closeOnce.Do(func() { close(l.done) })
	return nil
}
func (l *downstreamCompressionPipeListener) Addr() net.Addr { return l.LocalAddr() }

func dialDownstreamCompressionPipe(t *testing.T, router http.Handler, path string, offer bool, origin string) (*websocket.Conn, bool) {
	t.Helper()
	serverConn, clientConn := net.Pipe()
	listener := &downstreamCompressionPipeListener{Conn: serverConn, done: make(chan struct{})}
	server := &http.Server{Handler: router, ReadHeaderTimeout: 3 * time.Second}
	done := make(chan struct{})
	go func() { defer close(done); _ = server.Serve(listener) }()
	t.Cleanup(func() {
		_ = clientConn.Close()
		_ = serverConn.Close()
		_ = server.Close()
		<-done
	})
	dialer := websocket.Dialer{HandshakeTimeout: 3 * time.Second, EnableCompression: offer, NetDialContext: func(context.Context, string, string) (net.Conn, error) { return clientConn, nil }}
	headers := http.Header{}
	if origin != "" {
		headers.Set("Origin", origin)
	}
	conn, response, err := dialer.Dial("ws://fixture.invalid"+path, headers)
	if err != nil {
		t.Fatalf("downstream handshake: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	ext := response.Header.Get("Sec-WebSocket-Extensions")
	return conn, strings.Contains(ext, "permessage-deflate")
}

func TestDownstreamWebsocketCompressionInstancesAndReload(t *testing.T) {
	const responses = "/v1/responses"
	const backend = "/backend-api/codex/responses"
	t.Cleanup(func() { openai.SetResponsesWebsocketCompressionEnabled(true) })
	off, on := false, true
	oneConfig, otherConfig := &config.Config{}, &config.Config{}
	otherConfig.Codex.WebsocketCompression = &off
	one := newTestServerWithConfig(t, oneConfig)
	other := newTestServerWithConfig(t, otherConfig)
	check := func(router http.Handler, path string, offer, want bool) *websocket.Conn {
		t.Helper()
		conn, got := dialDownstreamCompressionPipe(t, router, path, offer, "http://different-origin.fixture.invalid")
		if got != want {
			t.Errorf("%s compression negotiated=%t, want=%t", path, got, want)
		}
		return conn
	}
	sdk := openai.NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&config.SDKConfig{}, nil))
	standalone := gin.New()
	standalone.GET(responses, sdk.ResponsesWebsocket)
	established := check(one.Handler(), responses, true, true)
	check(one.Handler(), responses, false, false)
	for _, path := range []string{responses, backend} {
		check(one.Handler(), path, true, true)
		check(other.Handler(), path, true, false)
	}
	check(standalone, responses, true, true)
	openai.SetResponsesWebsocketCompressionEnabled(false)
	check(standalone, responses, true, false)
	for _, path := range []string{responses, backend} {
		check(one.Handler(), path, true, true)
		check(other.Handler(), path, true, false)
	}
	oneConfig.Codex.WebsocketCompression = &off
	otherConfig.Codex.WebsocketCompression = &on
	one.UpdateClients(oneConfig)
	other.UpdateClients(otherConfig)
	check(standalone, responses, true, false)
	openai.SetResponsesWebsocketCompressionEnabled(true)
	check(standalone, responses, true, true)
	for _, path := range []string{responses, backend} {
		check(one.Handler(), path, true, false)
		check(other.Handler(), path, true, true)
	}

	// Send only a control frame on the established socket after both reloads.
	// No application request reaches an executor.
	pong := make(chan string, 1)
	established.SetPongHandler(func(data string) error { pong <- data; return nil })
	if err := established.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() { _, _, err := established.ReadMessage(); finished <- err }()
	if err := established.WriteControl(websocket.PingMessage, []byte("fixture"), time.Now().Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-pong:
		if got != "fixture" {
			t.Fatalf("pong payload=%q", got)
		}
	case err := <-finished:
		t.Fatalf("existing connection closed during reload: %v", err)
	case <-time.After(4 * time.Second):
		t.Fatal("missing pong on established socket")
	}
	_ = established.Close()
	select {
	case <-finished:
	case <-time.After(4 * time.Second):
		t.Fatal("reader did not stop after close")
	}

	// Malformed HTTP upgrade still uses the existing handler's error path.
	resp := httptest.NewRecorder()
	one.Handler().ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/v1/responses", nil))
	if resp.Code != http.StatusBadRequest || resp.Header().Get("Sec-WebSocket-Extensions") != "" {
		t.Fatalf("invalid upgrade status=%d extensions=%q", resp.Code, resp.Header().Get("Sec-WebSocket-Extensions"))
	}
	var registered bool
	for _, route := range one.engine.Routes() {
		if route.Method == http.MethodPost && route.Path == "/v1/responses" {
			registered = true
		}
	}
	if !registered {
		t.Fatal("ordinary POST Responses route missing")
	}
}
