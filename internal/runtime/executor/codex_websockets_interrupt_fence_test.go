package executor

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// 在完成真实 loopback handshake 后堵住底层 Write；Close 必须唤醒它。
type ws69BlockedConn struct {
	net.Conn
	armed     atomic.Bool
	entered   chan struct{}
	closed    chan struct{}
	enterOnce sync.Once
	closeOnce sync.Once
}

func (c *ws69BlockedConn) Write(p []byte) (int, error) {
	if !c.armed.Load() {
		return c.Conn.Write(p)
	}
	c.enterOnce.Do(func() { close(c.entered) })
	<-c.closed
	return 0, net.ErrClosed
}
func (c *ws69BlockedConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

type ws69Fixture struct {
	exec     *CodexWebsocketsExecutor
	a, b     *codexWebsocketSession
	received chan string
}

func newWS69Fixture(t *testing.T, blocked *ws69BlockedConn) ws69Fixture {
	t.Helper()
	f := ws69Fixture{exec: NewCodexWebsocketsExecutor(nil), received: make(chan string, 8)}
	f.exec.store = &codexWebsocketSessionStore{sessions: make(map[string]*codexWebsocketSession)}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			f.received <- r.Header.Get("X-Fixture-Connection") + ":" + string(payload)
		}
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { f.exec.CloseExecutionSession("ws69") })
	wsURL := "ws" + server.URL[len("http"):]
	connect := func(label string) *codexWebsocketSession {
		dialer := *websocket.DefaultDialer
		if label == "a" && blocked != nil {
			dialer.NetDialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
				raw, err := (&net.Dialer{}).DialContext(ctx, network, address)
				if err != nil {
					return nil, err
				}
				blocked.Conn = raw
				return blocked, nil
			}
		}
		conn, _, err := dialer.Dial(wsURL, http.Header{"X-Fixture-Connection": []string{label}})
		if err != nil {
			t.Fatal(err)
		}
		s := f.exec.getOrCreateSession("ws69", &cliproxyauth.Auth{ID: label, ProxyURL: "direct"}, wsURL)
		s.connMu.Lock()
		s.conn = conn
		s.connCloser = newWebsocketConnectionCloser(conn)
		s.connMu.Unlock()
		s.configureConn(conn)
		s.activate(conn)
		return s
	}
	f.a, f.b = connect("a"), connect("b")
	f.a.bindDisconnectOwner(f.a.conn)
	return f
}

func TestWS69InterruptFenceCoversCompleteWireWrite(t *testing.T) {
	f := newWS69Fixture(t, nil)
	owner := f.a.disconnectOwner
	entered, release := make(chan struct{}), make(chan struct{})
	previous := testWebsocketWriteChunkHook
	testWebsocketWriteChunkHook = func(index, total int) {
		if index == 0 {
			close(entered)
			<-release
		}
	}
	defer func() { testWebsocketWriteChunkHook = previous }()
	payload := append([]byte(`{"type":"response.interrupt","response_id":"current","extension":"`), bytes.Repeat([]byte("x"), 2*codexWebsocketWriteChunkSize)...)
	payload = append(payload, []byte(`"}`)...)
	done := make(chan error, 1)
	go func() { done <- f.exec.InterruptExecutionSession(context.Background(), "ws69", payload) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("write did not reach actual chunk barrier")
	}
	if owner.interruptFence.TryLock() {
		owner.interruptFence.Unlock()
		close(release)
		t.Fatal("wire write lacks generation fence")
	}
	for _, mu := range []*sync.Mutex{&f.a.connMu, &owner.connMu, &f.a.activeMu} {
		if !mu.TryLock() {
			close(release)
			t.Fatal("state lock held across wire write")
		}
		mu.Unlock()
	}
	bound := make(chan struct{})
	go func() { f.b.bindDisconnectOwner(f.b.conn); close(bound) }()
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("write blocked")
	}
	select {
	case <-bound:
	case <-time.After(time.Second):
		t.Fatal("generation bind blocked after write")
	}
	select {
	case got := <-f.received:
		if got != "a:"+string(payload) {
			t.Fatal("committed interrupt payload changed")
		}
	case <-time.After(time.Second):
		t.Fatal("committed frame missing")
	}
	testWebsocketWriteChunkHook = previous
	small := []byte(`{"type":"response.interrupt","response_id":"next"}`)
	if err := f.exec.InterruptExecutionSession(context.Background(), "ws69", small); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-f.received:
		if got != "b:"+string(small) {
			t.Fatalf("new selection missed: %s", got)
		}
	case <-time.After(time.Second):
		t.Fatal("new generation missing")
	}
}

func TestWS69InterruptLockWaitCancels(t *testing.T) {
	for _, lock := range []string{"writer", "fence"} {
		t.Run(lock, func(t *testing.T) {
			f := newWS69Fixture(t, nil)
			mu := &f.a.writeMu
			if lock == "fence" {
				mu = &f.a.disconnectOwner.interruptFence
			}
			mu.Lock()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			checked := make(chan struct{})
			ctx = cliproxyexecutor.WithWebsocketAuthCheck(ctx, func(string) bool { close(checked); return true })
			done := make(chan error, 1)
			go func() { done <- f.exec.InterruptExecutionSession(ctx, "ws69", []byte(`{"type":"response.interrupt"}`)) }()
			select {
			case <-checked:
			case <-time.After(time.Second):
				mu.Unlock()
				t.Fatal("auth callback blocked by lifecycle lock")
			}
			cancel()
			select {
			case err := <-done:
				mu.Unlock()
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("lock wait error=%v", err)
				}
			case <-time.After(time.Second):
				mu.Unlock()
				t.Fatal("lock wait ignored cancel")
			}
			select {
			case got := <-f.received:
				t.Fatalf("cancelled waiter wrote: %s", got)
			default:
			}
		})
	}
}

func TestWS69InterruptBlockedWriteIsBounded(t *testing.T) {
	for _, mode := range []string{"cancel", "deadline", "default-budget"} {
		t.Run(mode, func(t *testing.T) {
			blocked := &ws69BlockedConn{entered: make(chan struct{}), closed: make(chan struct{})}
			f := newWS69Fixture(t, blocked)
			blocked.armed.Store(true)
			ctx := context.Background()
			cancel := func() {}
			if mode == "cancel" {
				ctx, cancel = context.WithCancel(ctx)
			}
			if mode == "deadline" {
				ctx, cancel = context.WithTimeout(ctx, 100*time.Millisecond)
			}
			defer cancel()
			done := make(chan error, 1)
			go func() {
				done <- f.exec.InterruptExecutionSession(ctx, "ws69", []byte(`{"type":"response.interrupt","response_id":"current"}`))
			}()
			select {
			case <-blocked.entered:
			case <-time.After(time.Second):
				t.Fatal("fixture did not block actual net.Conn.Write")
			}
			if mode == "cancel" {
				cancel()
			}
			limit := time.Second
			if mode == "default-budget" {
				limit = codexWebsocketInterruptWriteTimeout + time.Second
			}
			select {
			case err := <-done:
				want := context.DeadlineExceeded
				if mode == "cancel" {
					want = context.Canceled
				}
				if !errors.Is(err, want) {
					t.Fatalf("write error=%v want=%v", err, want)
				}
			case <-time.After(limit):
				t.Fatal("blocked wire write exceeded budget")
			}
			select {
			case <-blocked.closed:
			default:
				t.Fatal("cancel did not close blocked physical connection")
			}
			if !f.a.disconnectOwner.interruptFence.TryLock() {
				t.Fatal("interrupt leaked lifecycle fence")
			}
			f.a.disconnectOwner.interruptFence.Unlock()
			f.b.bindDisconnectOwner(f.b.conn)
			if err := f.exec.InterruptExecutionSession(context.Background(), "ws69", []byte(`{"type":"response.interrupt","response_id":"new"}`)); err != nil {
				t.Fatal(err)
			}
			select {
			case got := <-f.received:
				if got != `b:{"type":"response.interrupt","response_id":"new"}` {
					t.Fatalf("unexpected write %s", got)
				}
			case <-time.After(time.Second):
				t.Fatal("successor write missing after cancellation")
			}
		})
	}
}

func TestWS69CloseKeepsGenerationIndexedUntilFenceReleased(t *testing.T) {
	for _, mode := range []string{"session", "all", "auth"} {
		t.Run(mode, func(t *testing.T) {
			f := newWS69Fixture(t, nil)
			// 只留目标 auth，确保按 auth 关闭也会移除逻辑 owner。
			f.exec.CloseAuthExecutionSessions("b")
			owner := f.a.disconnectOwner
			owner.interruptFence.Lock()
			started, done := make(chan struct{}), make(chan struct{})
			go func() {
				close(started)
				switch mode {
				case "session":
					f.exec.CloseExecutionSession("ws69")
				case "all":
					f.exec.CloseExecutionSession(cliproxyauth.CloseAllExecutionSessionsID)
				case "auth":
					f.exec.CloseAuthExecutionSessions("a")
				}
				close(done)
			}()
			<-started
			select {
			case <-done:
				t.Error("close completed while wire fence was held")
			case <-time.After(100 * time.Millisecond):
			}
			f.exec.store.mu.Lock()
			indexed := f.exec.store.disconnects["ws69"]
			f.exec.store.mu.Unlock()
			if indexed != owner {
				t.Error("logical generation removed before its wire commit fence released")
			}
			owner.interruptFence.Unlock()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("close did not finish after fence release")
			}
			fresh := f.exec.getOrCreateSession("ws69")
			if fresh == owner || fresh.closed {
				t.Fatal("reopened logical session reused the closed generation")
			}
		})
	}
}

func TestWS69AuthCloseDoesNotWaitForUnrelatedLogicalFence(t *testing.T) {
	blocked := &ws69BlockedConn{entered: make(chan struct{}), closed: make(chan struct{})}
	f := newWS69Fixture(t, blocked)
	target := f.exec.getOrCreateSession("unrelated", &cliproxyauth.Auth{ID: "remove-me", ProxyURL: "direct"}, "")
	defer f.exec.CloseExecutionSession("unrelated")
	blocked.armed.Store(true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	writing := make(chan error, 1)
	go func() {
		writing <- f.exec.InterruptExecutionSession(ctx, "ws69", []byte(`{"type":"response.interrupt"}`))
	}()
	select {
	case <-blocked.entered:
	case <-time.After(time.Second):
		t.Fatal("unrelated interrupt did not block actual wire Write")
	}
	done := make(chan struct{})
	go func() { f.exec.CloseAuthExecutionSessions("remove-me"); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Error("auth removal waited for an unrelated interrupt wire write")
	}
	cancel()
	select {
	case err := <-writing:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("blocked interrupt=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked interrupt did not cancel")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("auth removal stuck")
	}
	target.connMu.Lock()
	closed := target.closed
	target.connMu.Unlock()
	if !closed {
		t.Fatal("selected auth session was not closed")
	}
}

// 关闭仅处理快照内的 physical；快照之后进入的 C 保留，owner/index 必须仍然一致。
func TestWS70CloseSnapshotKeepsNewPhysicalReachable(t *testing.T) {
	for _, mode := range []string{"session", "all"} {
		t.Run(mode, func(t *testing.T) {
			f := newWS69Fixture(t, nil)
			wsURL := f.a.wsURL
			f.a.connMu.Lock()
			held := true
			defer func() {
				if held {
					f.a.connMu.Unlock()
				}
			}()
			done := make(chan struct{})
			go func() {
				if mode == "all" {
					f.exec.CloseExecutionSession(cliproxyauth.CloseAllExecutionSessionsID)
				} else {
					f.exec.CloseExecutionSession("ws69")
				}
				close(done)
			}()
			reached := false
			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) {
				stack := make([]byte, 128*1024)
				n := runtime.Stack(stack, true)
				for _, goroutine := range strings.Split(string(stack[:n]), "\n\n") {
					// 本fixture只有a.connMu被持有；同一阻塞goroutine必须已进入快照后的筛选。
					if strings.Contains(goroutine, "[sync.Mutex.Lock]:") && strings.Contains(goroutine, "closeCodexWebsocketLogicalSessions(") {
						reached = true
						break
					}
				}
				if reached {
					break
				}
				time.Sleep(time.Millisecond)
			}
			if !reached {
				t.Fatal("close did not reach post-snapshot physical lock")
			}
			authC := &cliproxyauth.Auth{ID: "c", ProxyURL: "direct"}
			late := f.exec.getOrCreateSession("ws69", authC, wsURL)
			if late.disconnectOwner != f.a.disconnectOwner {
				t.Fatal("fixture did not enter the snapshotted generation")
			}
			f.a.connMu.Unlock()
			held = false
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("close did not finish")
			}
			late.connMu.Lock()
			lateClosed := late.closed
			late.connMu.Unlock()
			owner := late.disconnectOwner
			owner.connMu.Lock()
			ownerClosed := owner.closed
			owner.connMu.Unlock()
			f.exec.store.mu.Lock()
			indexed := f.exec.store.disconnects["ws69"]
			retained := f.exec.store.sessions[codexWebsocketSessionStoreKey("ws69", authC, wsURL)] == late
			f.exec.store.mu.Unlock()
			if lateClosed || ownerClosed || indexed != owner || !retained {
				t.Fatalf("late physical inconsistent: physicalClosed=%v ownerClosed=%v indexed=%v retained=%v", lateClosed, ownerClosed, indexed == owner, retained)
			}
			if f.exec.getOrCreateSession("ws69", authC, wsURL) != late || f.exec.getOrCreateSession("ws69") != owner {
				t.Fatal("same-key reopen lost the live physical/owner pair")
			}
			conn, _, err := websocket.DefaultDialer.Dial(wsURL, http.Header{"X-Fixture-Connection": []string{"c"}})
			if err != nil {
				t.Fatal(err)
			}
			late.connMu.Lock()
			late.conn = conn
			late.connCloser = newWebsocketConnectionCloser(conn)
			late.connMu.Unlock()
			late.configureConn(conn)
			late.activate(conn)
			late.bindDisconnectOwner(conn)
			payload := []byte(`{"type":"response.interrupt","response_id":"late-c","mode":"discard_partial_items"}`)
			if err := f.exec.InterruptExecutionSession(context.Background(), "ws69", payload); err != nil {
				t.Fatalf("retained physical interrupt unreachable: %v", err)
			}
			select {
			case got := <-f.received:
				if got != "c:"+string(payload) {
					t.Fatalf("wrong physical received interrupt: %s", got)
				}
			case <-time.After(time.Second):
				t.Fatal("C did not receive unchanged interrupt")
			}
			for _, old := range []*codexWebsocketSession{f.a, f.b} {
				old.connMu.Lock()
				closed := old.closed
				old.connMu.Unlock()
				if !closed {
					t.Fatal("snapshot physical survived close")
				}
			}
		})
	}
}
