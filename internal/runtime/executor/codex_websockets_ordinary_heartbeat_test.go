package executor

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func ordinaryPipeWebsocket(t *testing.T) (*websocket.Conn, net.Conn) {
	t.Helper()
	client, peer := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = peer.Close() })
	_ = client.SetDeadline(time.Now().Add(3 * time.Second))
	_ = peer.SetDeadline(time.Now().Add(3 * time.Second))
	upgraded := make(chan error, 1)
	go func() {
		req, err := http.ReadRequest(bufio.NewReader(peer))
		if err == nil {
			hash := sha1.Sum([]byte(req.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
			_, err = fmt.Fprintf(peer, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", base64.StdEncoding.EncodeToString(hash[:]))
		}
		upgraded <- err
	}()
	conn, _, err := websocket.NewClient(client, &url.URL{Scheme: "ws", Host: "fixture.invalid"}, nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-upgraded; err != nil {
		t.Fatal(err)
	}
	return conn, peer
}

func TestOrdinaryHeartbeatSendsPingAndStopsOnClose(t *testing.T) {
	conn, peer := ordinaryPipeWebsocket(t)
	closer := newWebsocketConnectionCloser(conn)
	defer closer.Close()
	heartbeat := startCodexWebsocketHeartbeat(closer, 10*time.Millisecond)
	frame := make([]byte, 6)
	if _, err := io.ReadFull(peer, frame); err != nil {
		t.Fatal(err)
	}
	if frame[0] != 0x89 || frame[1] != 0x80 {
		t.Fatal("invalid client ping control frame")
	}
	_ = closer.Close()
	select {
	case <-heartbeat:
	case <-time.After(time.Second):
		t.Fatal("heartbeat did not terminate when owner closed")
	}
}

func TestOrdinaryWebsocketDisconnectReportsTurnCause(t *testing.T) {
	conn, peer := ordinaryPipeWebsocket(t)
	closer := newWebsocketConnectionCloser(conn)
	defer closer.Close()
	sess := &codexWebsocketSession{conn: conn, connCloser: closer, readerConn: conn}
	sess.configureConn(conn)
	received := sess.activate(conn)
	done := make(chan struct{})
	go func() { defer close(done); NewCodexWebsocketsExecutor(nil).readUpstreamLoop(sess, conn) }()
	_ = peer.Close()
	select {
	case ev := <-received:
		var closeErr *websocket.CloseError
		if !errors.As(ev.err, &closeErr) || closeErr.Code != websocket.CloseAbnormalClosure {
			t.Fatalf("disconnect cause = %v", ev.err)
		}
		got := codexWebsocketTurnTransportError(ev.err)
		if !cliproxyexecutor.IsUpstreamWebsocketTurnError(got) || !errors.Is(got, ev.err) {
			t.Fatalf("turn lost transport cause: %v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("active session did not receive disconnect")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("socket reader did not terminate")
	}
}

func TestOrdinaryWebsocketInvalidExplicitProxyDoesNotFallback(t *testing.T) {
	for _, raw := range []string{"ftp://fixture.invalid", "http://:8080", "socks5://fixture.invalid:99999"} {
		cfg := &config.Config{SDKConfig: config.SDKConfig{ProxyURL: raw}}
		dialer := newProxyAwareWebsocketDialer(context.Background(), cfg, nil)
		if dialer.Proxy != nil {
			t.Fatal("bad explicit proxy fell back to inherited proxy")
		}
		conn, err := dialer.NetDialContext(context.Background(), "tcp", "fixture.invalid:443")
		if conn != nil || !errors.Is(err, errInvalidWebsocketProxy) {
			t.Fatalf("explicit proxy acceptance: %v", err)
		}
	}
}

func TestOrdinaryWebsocketUsesStandardTLSForExplicitRoutes(t *testing.T) {
	for _, raw := range []string{"direct", "https://proxy.fixture.invalid:8443", "socks5h://proxy.fixture.invalid:1080"} {
		cfg := &config.Config{SDKConfig: config.SDKConfig{ProxyURL: raw}}
		dial := newProxyAwareWebsocketDialer(context.Background(), cfg, nil)
		if dial.NetDialTLSContext != nil || dial.Proxy != nil || dial.NetDialContext == nil {
			t.Fatalf("route %q did not preserve standard TLS over explicit transport", raw)
		}
	}
}
