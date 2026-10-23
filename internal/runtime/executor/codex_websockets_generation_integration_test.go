package executor

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func TestCodexDisconnectNotificationFollowsPhysicalGeneration(t *testing.T) {
	for _, mode := range []string{"auth_switch", "proxy_switch", "same_object_reconnect"} {
		t.Run(mode, func(t *testing.T) {
			exec := NewCodexWebsocketsExecutor(&config.Config{})
			exec.store = &codexWebsocketSessionStore{sessions: make(map[string]*codexWebsocketSession)}
			const id, url = "logical", "wss://synthetic.invalid/responses"
			ch := exec.UpstreamDisconnectChan(id)
			authA := &cliproxyauth.Auth{ID: "a", ProxyURL: "direct"}
			authB := &cliproxyauth.Auth{ID: "b", ProxyURL: "direct"}
			if mode == "proxy_switch" {
				authB.ID = "a"
				authB.ProxyURL = "http://proxy.invalid:8080"
			}
			if mode == "same_object_reconnect" {
				authB = authA
			}
			leaseA, err := exec.acquireSessionLease(context.Background(), id, authA, url, false)
			if err != nil {
				t.Fatal(err)
			}
			a, connA := leaseA.session, &websocket.Conn{}
			a.connMu.Lock()
			a.conn = connA
			a.connMu.Unlock()
			a.bindDisconnectOwner(connA)
			leaseA.release()
			leaseB, err := exec.acquireSessionLease(context.Background(), id, authB, url, false)
			if err != nil {
				t.Fatal(err)
			}
			defer leaseB.release()
			b, connB := leaseB.session, &websocket.Conn{}
			b.connMu.Lock()
			b.conn = connB
			b.connMu.Unlock()
			b.bindDisconnectOwner(connB)
			if mode != "same_object_reconnect" && a == b {
				t.Fatal("different auth/proxy shared physical session")
			}
			if mode == "same_object_reconnect" && a != b {
				t.Fatal("fixture did not reuse physical session")
			}
			oldErr := errors.New("old connection failed")
			exec.invalidateUpstreamConn(a, connA, "old_failure", oldErr)
			// Also cover delayed post-delivery notification after invalidation released its lock.
			a.notifyUpstreamDisconnect(oldErr, connA)
			select {
			case got := <-ch:
				t.Fatalf("old generation closed current logical session: %v", got)
			default:
			}
			currentErr := errors.New("current connection failed")
			exec.invalidateUpstreamConn(b, connB, "current_failure", currentErr)
			select {
			case got, ok := <-ch:
				if !ok || got != currentErr {
					t.Fatalf("got %v/%v", got, ok)
				}
			default:
				t.Fatal("current failure was not delivered")
			}
			exec.CloseExecutionSession(id)
			reopened := exec.UpstreamDisconnectChan(id)
			if reopened == ch {
				t.Fatal("logical reopen reused closed channel")
			}
			b.notifyUpstreamDisconnect(currentErr, connB)
			select {
			case got := <-reopened:
				t.Fatalf("prior logical generation leaked: %v", got)
			default:
			}
		})
	}
}

func TestCodexCanceledLeaseDoesNotStealDisconnectOwner(t *testing.T) {
	exec := NewCodexWebsocketsExecutor(&config.Config{})
	exec.store = &codexWebsocketSessionStore{sessions: make(map[string]*codexWebsocketSession)}
	const id, url = "cancel-owner", "wss://synthetic.invalid/responses"
	auth := &cliproxyauth.Auth{ID: "a", ProxyURL: "direct"}
	ch := exec.UpstreamDisconnectChan(id)
	held, err := exec.acquireSessionLease(context.Background(), id, auth, url, false)
	if err != nil {
		t.Fatal(err)
	}
	conn := &websocket.Conn{}
	held.session.connMu.Lock()
	held.session.conn = conn
	held.session.connMu.Unlock()
	held.session.bindDisconnectOwner(conn)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rejected, err := exec.acquireSessionLease(ctx, id, &cliproxyauth.Auth{ID: "b", ProxyURL: "direct"}, url, false)
	if !errors.Is(err, context.Canceled) || rejected != nil {
		t.Fatalf("canceled acquisition = %v/%v", rejected, err)
	}
	held.release()
	again, err := exec.acquireSessionLease(context.Background(), id, auth, url, false)
	if err != nil {
		t.Fatal(err)
	}
	again.release()
	sentinel := errors.New("owner still active")
	held.session.notifyUpstreamDisconnect(sentinel, conn)
	select {
	case got := <-ch:
		if got != sentinel {
			t.Fatal(got)
		}
	default:
		t.Fatal("cancel or release stole owner")
	}
}

func TestCodexWebsocketRequestProxyOverrideScopesLease(t *testing.T) {
	exec := NewCodexWebsocketsExecutor(&config.Config{SDKConfig: sdkconfig.SDKConfig{ProxyURL: "http://global.invalid:80"}})
	exec.store = &codexWebsocketSessionStore{sessions: make(map[string]*codexWebsocketSession)}
	auth := &cliproxyauth.Auth{ID: "a", ProxyURL: "http://account.invalid:80"}
	first, err := exec.acquireSessionLease(context.Background(), "proxy-scope", auth, "wss://synthetic.invalid", false)
	if err != nil {
		t.Fatal(err)
	}
	first.release()
	ctx := cliproxyexecutor.WithRequestProxyURL(context.Background(), "direct")
	second, err := exec.acquireSessionLease(ctx, "proxy-scope", auth, "wss://synthetic.invalid", false)
	if err != nil {
		t.Fatal(err)
	}
	defer second.release()
	if first.session == second.session {
		t.Fatal("request override reused account-proxy session")
	}
	if got := executionProxyURL(ctx, exec.cfg, auth); got != "direct" {
		t.Fatalf("effective proxy = %q", got)
	}
	if got := executionProxyURL(cliproxyexecutor.WithoutRequestProxyURL(ctx), exec.cfg, auth); got != auth.ProxyURL {
		t.Fatalf("cleared override = %q", got)
	}
}

func TestCodexWebsocketDialHonorsOverrideAndFailsClosed(t *testing.T) {
	var hits atomic.Int32
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		c, err := upgrader.Upgrade(w, r, nil)
		if err == nil {
			_ = c.Close()
		}
	}))
	defer server.Close()
	exec := NewCodexWebsocketsExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{ID: "synthetic", ProxyURL: "unsupported://proxy.invalid"}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	conn, closer, _, err := exec.dialCodexWebsocket(cliproxyexecutor.WithRequestProxyURL(ctx, "direct"), auth, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil || conn == nil {
		t.Fatalf("explicit direct override failed: %v", err)
	}
	_ = closer.Close()
	before := hits.Load()
	for _, proxy := range []string{"unsupported://proxy.invalid", "http://"} {
		t.Run(proxy, func(t *testing.T) {
			conn, _, _, err := exec.dialCodexWebsocket(cliproxyexecutor.WithRequestProxyURL(ctx, proxy), &cliproxyauth.Auth{ID: "synthetic", ProxyURL: "direct"}, "wss"+strings.TrimPrefix(server.URL, "http"), nil)
			if err == nil || conn != nil {
				t.Fatalf("invalid proxy accepted: conn=%v err=%v", conn, err)
			}
			if hits.Load() != before {
				t.Fatal("invalid proxy fell back to direct")
			}
		})
	}
}

func TestCodexAuthRevocationPreservesOtherLogicalSubscribers(t *testing.T) {
	for _, sameID := range []bool{false, true} {
		t.Run(fmt.Sprintf("sameID=%t", sameID), func(t *testing.T) {
			exec := NewCodexWebsocketsExecutor(&config.Config{})
			exec.store = &codexWebsocketSessionStore{sessions: make(map[string]*codexWebsocketSession)}
			const pendingID, url = "pending-subscriber", "wss://synthetic.invalid/responses"
			ch := exec.UpstreamDisconnectChan(pendingID)
			idA := "unrelated-revoked"
			if sameID {
				idA = pendingID
			}
			a, err := exec.acquireSessionLease(context.Background(), idA, &cliproxyauth.Auth{ID: "revoked", ProxyURL: "direct"}, url, false)
			if err != nil {
				t.Fatal(err)
			}
			defer a.release()
			var b *codexWebsocketSessionLease
			acquireB := func() {
				var err error
				b, err = exec.acquireSessionLease(context.Background(), pendingID, &cliproxyauth.Auth{ID: "current", ProxyURL: "direct"}, url, false)
				if err != nil {
					t.Fatal(err)
				}
			}
			if sameID {
				acquireB()
			}
			exec.CloseAuthExecutionSessions("revoked")
			if !sameID {
				acquireB()
			}
			defer b.release()
			conn := &websocket.Conn{}
			b.session.connMu.Lock()
			b.session.conn = conn
			b.session.connMu.Unlock()
			b.session.bindDisconnectOwner(conn)
			sentinel := errors.New("current subscriber")
			exec.invalidateUpstreamConn(b.session, conn, "current", sentinel)
			select {
			case got := <-ch:
				if got != sentinel {
					t.Fatal(got)
				}
			default:
				t.Fatal("auth revocation detached a live or pending subscriber")
			}
		})
	}
}
