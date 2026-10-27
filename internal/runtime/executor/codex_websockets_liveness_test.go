package executor

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestCodexWebsocketControlFramesRefreshReadDeadline(t *testing.T) {
	for _, tc := range []struct {
		name    string
		control int
		session bool
		active  bool
	}{
		{"unshared-ping", websocket.PingMessage, false, false},
		{"unshared-pong", websocket.PongMessage, false, false},
		{"active-ping", websocket.PingMessage, true, true},
		{"active-pong", websocket.PongMessage, true, true},
		{"idle-ping", websocket.PingMessage, true, false},
		{"idle-pong", websocket.PongMessage, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upgrader := websocket.Upgrader{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer conn.Close()
				if _, _, err = conn.ReadMessage(); err != nil {
					return
				}
				if err = conn.WriteControl(tc.control, []byte("alive"), time.Now().Add(time.Second)); err != nil {
					return
				}
				time.Sleep(300 * time.Millisecond)
				_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed"}`))
			}))
			defer server.Close()
			exec := NewCodexWebsocketsExecutor(nil)
			conn, closer, _, err := exec.dialCodexWebsocket(context.Background(), &cliproxyauth.Auth{ProxyURL: "direct"}, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer closer.Close()
			if tc.session {
				sess := newCodexWebsocketSession(t.Name(), "auth", "ws://test")
				if tc.active {
					ch := sess.activate(conn)
					defer sess.clearActive(conn, ch)
				}
				sess.configureConn(conn)
			}
			if err = conn.SetReadDeadline(time.Now().Add(150 * time.Millisecond)); err != nil {
				t.Fatal(err)
			}
			if err = conn.WriteMessage(websocket.TextMessage, []byte("start")); err != nil {
				t.Fatal(err)
			}
			_, payload, err := conn.ReadMessage()
			if tc.session && !tc.active {
				var timeout interface{ Timeout() bool }
				if !errors.As(err, &timeout) || !timeout.Timeout() {
					t.Fatalf("idle session error = %v, want timeout", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("live control frame must refresh deadline: %v", err)
			}
			if string(payload) != `{"type":"response.completed"}` {
				t.Fatalf("unexpected payload: %s", payload)
			}
		})
	}
}

func TestCodexWebsocketReadCancellationClosesUnsharedConnection(t *testing.T) {
	upgrader := websocket.Upgrader{}
	serverClosed := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		defer close(serverClosed)
		_, _, _ = conn.ReadMessage()
	}))
	defer server.Close()
	exec := NewCodexWebsocketsExecutor(nil)
	conn, closer, _, err := exec.dialCodexWebsocket(context.Background(), &cliproxyauth.Auth{ProxyURL: "direct"}, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan codexWebsocketRead, 1)
	go func() { done <- readCodexWebsocketEvent(ctx, nil, conn, nil) }()
	cancel()
	select {
	case event := <-done:
		if !errors.Is(event.err, context.Canceled) {
			t.Fatalf("read error = %v, want context.Canceled", event.err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation left websocket read blocked")
	}
	select {
	case <-serverClosed:
	case <-time.After(time.Second):
		t.Fatal("cancellation left upstream connection open")
	}
}

func TestCodexWebsocketHeartbeatAndShutdown(t *testing.T) {
	for _, respond := range []bool{true, false} {
		t.Run(map[bool]string{true: "pong", false: "silent"}[respond], func(t *testing.T) {
			upgrader := websocket.Upgrader{}
			pings := make(chan struct{}, 10)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer conn.Close()
				defaultPing := conn.PingHandler()
				count := 0
				conn.SetPingHandler(func(data string) error {
					pings <- struct{}{}
					count++
					if !respond {
						return nil
					}
					if err := defaultPing(data); err != nil {
						return err
					}
					if count == 3 {
						return conn.WriteMessage(websocket.TextMessage, []byte("completed"))
					}
					return nil
				})
				for {
					if _, _, err := conn.ReadMessage(); err != nil {
						return
					}
				}
			}))
			defer server.Close()
			conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
			if err != nil {
				t.Fatal(err)
			}
			closer := newWebsocketConnectionCloser(conn)
			defer closer.Close()
			stopped := startCodexWebsocketHeartbeat(closer, 20*time.Millisecond)
			_ = conn.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
			_, payload, readErr := conn.ReadMessage()
			if respond {
				if readErr != nil || string(payload) != "completed" {
					t.Fatalf("heartbeat result: %s, %v", payload, readErr)
				}
			} else {
				var timeout interface{ Timeout() bool }
				if !errors.As(readErr, &timeout) || !timeout.Timeout() {
					t.Fatalf("silent upstream error = %v, want timeout", readErr)
				}
			}
			if len(pings) < 3 {
				t.Fatalf("received %d pings, want at least 3", len(pings))
			}
			_ = closer.Close()
			select {
			case <-stopped:
			case <-time.After(time.Second):
				t.Fatal("heartbeat survived connection close")
			}
		})
	}
}
