package executor

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestInterruptExecutionSessionRequiresActiveRead(t *testing.T) {
	received := make(chan []byte, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, errUpgrade := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if errUpgrade != nil {
			t.Errorf("upgrade: %v", errUpgrade)
			return
		}
		defer func() { _ = conn.Close() }()
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, payload, errRead := conn.ReadMessage()
		if errRead != nil {
			return
		}
		received <- payload
	}))
	defer upstream.Close()

	client, _, errDial := websocket.DefaultDialer.Dial("ws"+upstream.URL[len("http"):], nil)
	if errDial != nil {
		t.Fatal(errDial)
	}
	defer func() { _ = client.Close() }()

	const sessionID = "interrupt-requires-active-read"
	executor := NewCodexWebsocketsExecutor(nil)
	defer executor.CloseExecutionSession(sessionID)
	auth := &cliproxyauth.Auth{ID: "auth", ProxyURL: "direct"}
	sess := executor.getOrCreateSession(sessionID, auth, "ws"+upstream.URL[len("http"):])
	sess.connMu.Lock()
	sess.conn = client
	sess.authID = "auth"
	sess.connMu.Unlock()
	sess.bindDisconnectOwner(client)

	interrupt := []byte(`{"type":"response.interrupt","response_id":"r1","mode":"discard_partial_items"}`)
	errIdle := executor.InterruptExecutionSession(context.Background(), sessionID, interrupt)
	if !errors.Is(errIdle, cliproxyexecutor.ErrNoActiveUpstreamWebsocket) {
		t.Fatalf("idle socket error = %v, want ErrNoActiveUpstreamWebsocket", errIdle)
	}
	select {
	case payload := <-received:
		t.Fatalf("idle socket was written: %s", payload)
	case <-time.After(200 * time.Millisecond):
	}

	readCh := sess.activate(client)
	defer sess.clearActive(client, readCh)
	if errActive := executor.InterruptExecutionSession(context.Background(), sessionID, interrupt); errActive != nil {
		t.Fatal(errActive)
	}
	select {
	case payload := <-received:
		if !bytes.Equal(payload, interrupt) {
			t.Fatalf("active interrupt = %s", payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("active socket did not receive the interrupt")
	}
}

// 鉴权回调可能切换账号或重开同一逻辑会话；迟到 interrupt 不能写旧物理连接。
func TestInterruptExecutionSessionRechecksSelectedGeneration(t *testing.T) {
	for _, reopen := range []bool{false, true} {
		t.Run(map[bool]string{false: "auth-switch", true: "close-reopen"}[reopen], func(t *testing.T) {
			received := make(chan string, 8)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.Close()
				for {
					_, payload, err := conn.ReadMessage()
					if err != nil {
						return
					}
					received <- r.Header.Get("X-Fixture-Connection") + ":" + string(payload)
				}
			}))
			defer upstream.Close()
			exec := NewCodexWebsocketsExecutor(nil)
			exec.store = &codexWebsocketSessionStore{sessions: make(map[string]*codexWebsocketSession)}
			const sessionID = "interrupt-selected-generation"
			defer exec.CloseExecutionSession(sessionID)
			wsURL := "ws" + upstream.URL[len("http"):]
			connect := func(label string) *codexWebsocketSession {
				t.Helper()
				conn, _, err := websocket.DefaultDialer.Dial(wsURL, http.Header{"X-Fixture-Connection": []string{label}})
				if err != nil {
					t.Fatal(err)
				}
				auth := &cliproxyauth.Auth{ID: label, ProxyURL: "direct"}
				sess := exec.getOrCreateSession(sessionID, auth, wsURL)
				sess.connMu.Lock()
				sess.conn = conn
				sess.connCloser = newWebsocketConnectionCloser(conn)
				sess.connMu.Unlock()
				sess.configureConn(conn)
				sess.activate(conn)
				sess.bindDisconnectOwner(conn)
				return sess
			}
			first := connect("a")
			var second *codexWebsocketSession
			checked := false
			ctx := cliproxyexecutor.WithWebsocketAuthCheck(context.Background(), func(authID string) bool {
				checked = true
				if authID != "a" {
					t.Errorf("checked auth=%q, want a", authID)
				}
				if reopen {
					exec.CloseExecutionSession(sessionID)
				}
				second = connect("b")
				return true
			})
			payload := []byte(`{"type":"response.interrupt","response_id":"current","mode":"discard_partial_items"}`)
			if err := exec.InterruptExecutionSession(ctx, sessionID, payload); !errors.Is(err, cliproxyexecutor.ErrNoActiveUpstreamWebsocket) {
				t.Fatalf("changed generation interrupt=%v", err)
			}
			if !checked || second == nil {
				t.Fatal("interrupt never reached real selected generation auth check")
			}
			if !reopen {
				// 同一连接上的 barrier 证明没有先写入旧账号，而非依靠 sleep 猜测。
				if err := first.conn.WriteMessage(websocket.TextMessage, []byte("barrier")); err != nil {
					t.Fatal(err)
				}
				select {
				case got := <-received:
					if got != "a:barrier" {
						t.Fatalf("old generation received interrupt: %s", got)
					}
				case <-time.After(2 * time.Second):
					t.Fatal("old connection barrier timed out")
				}
			}
			if err := exec.InterruptExecutionSession(context.Background(), sessionID, payload); err != nil {
				t.Fatal(err)
			}
			select {
			case got := <-received:
				if got != "b:"+string(payload) {
					t.Fatalf("new generation payload=%s", got)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("new connection missed interrupt")
			}
		})
	}
}

// 独立窄审：auth callback 之后、实际写帧之前的 generation 切换。
func TestWS67Independent_InterruptGenerationSwitchBeforeWireWrite(t *testing.T) {
	received := make(chan string, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer c.Close()
		for {
			_, payload, err := c.ReadMessage()
			if err != nil {
				return
			}
			received <- r.Header.Get("X-Fixture-Connection") + ":" + string(payload)
		}
	}))
	defer server.Close()
	exec := NewCodexWebsocketsExecutor(nil)
	exec.store = &codexWebsocketSessionStore{sessions: make(map[string]*codexWebsocketSession)}
	const sessionID = "ws67-independent-late-interrupt"
	defer exec.CloseExecutionSession(sessionID)
	wsURL := "ws" + server.URL[len("http"):]
	connect := func(label string) *codexWebsocketSession {
		c, _, err := websocket.DefaultDialer.Dial(wsURL, http.Header{"X-Fixture-Connection": []string{label}})
		if err != nil {
			t.Fatal(err)
		}
		s := exec.getOrCreateSession(sessionID, &cliproxyauth.Auth{ID: label, ProxyURL: "direct"}, wsURL)
		s.connMu.Lock()
		s.conn, s.connCloser = c, newWebsocketConnectionCloser(c)
		s.connMu.Unlock()
		s.configureConn(c)
		s.activate(c)
		return s
	}
	a, b := connect("a"), connect("b")
	a.bindDisconnectOwner(a.conn)
	entered, release := make(chan struct{}), make(chan struct{})
	priorHook := testWebsocketWritePayloadHook
	testWebsocketWritePayloadHook = func(c *websocket.Conn) {
		if c == a.conn {
			close(entered)
			<-release
		}
	}
	defer func() { testWebsocketWritePayloadHook = priorHook }()
	payload := []byte(`{"type":"response.interrupt","response_id":"current","mode":"discard_partial_items"}`)
	done := make(chan error, 1)
	go func() { done <- exec.InterruptExecutionSession(context.Background(), sessionID, payload) }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("interrupt did not reach pre-write barrier")
	}
	// 真实 bindDisconnectOwner 切到账号 b，旧账号 a 连接按复用契约保持开放。
	b.bindDisconnectOwner(b.conn)
	owner := b.disconnectOwner
	owner.connMu.Lock()
	selected := owner.activeDisconnectSession == b && owner.activeDisconnectConn == b.conn
	owner.connMu.Unlock()
	if !selected {
		t.Fatal("fixture failed to switch selected generation")
	}
	close(release)
	select {
	case err := <-done:
		t.Logf("interrupt returned %v after selection moved to b", err)
	case <-time.After(2 * time.Second):
		t.Fatal("interrupt did not finish")
	}
	select {
	case got := <-received:
		if got == "a:"+string(payload) {
			t.Fatalf("stale interrupt reached account a after current generation switched to b: %s", got)
		}
		t.Fatalf("unexpected forwarded interrupt: %s", got)
	case <-time.After(200 * time.Millisecond):
	}
}
