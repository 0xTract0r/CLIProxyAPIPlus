package executor

import (
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestCodexWebsocketsExecutor_SessionStoreSurvivesExecutorReplacement(t *testing.T) {
	sessionID := "test-session-store-survives-replace"
	auth := &cliproxyauth.Auth{ID: "codex-auth-a", ProxyURL: "http://proxy.local:8080"}
	wsURL := "wss://chatgpt.com/backend-api/codex/responses"
	storeKey := codexWebsocketSessionStoreKey(sessionID, auth, wsURL)

	globalCodexWebsocketSessionStore.mu.Lock()
	delete(globalCodexWebsocketSessionStore.sessions, storeKey)
	globalCodexWebsocketSessionStore.mu.Unlock()

	exec1 := NewCodexWebsocketsExecutor(nil)
	sess1 := exec1.getOrCreateSession(sessionID, auth, wsURL)
	if sess1 == nil {
		t.Fatalf("expected session to be created")
	}

	exec2 := NewCodexWebsocketsExecutor(nil)
	sess2 := exec2.getOrCreateSession(sessionID, auth, wsURL)
	if sess2 == nil {
		t.Fatalf("expected session to be available across executors")
	}
	if sess1 != sess2 {
		t.Fatalf("expected the same session instance across executors")
	}

	manager := cliproxyauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(exec1)
	manager.RegisterExecutor(exec2)

	globalCodexWebsocketSessionStore.mu.Lock()
	_, stillPresent := globalCodexWebsocketSessionStore.sessions[storeKey]
	globalCodexWebsocketSessionStore.mu.Unlock()
	if !stillPresent {
		t.Fatalf("expected session to remain after shared-store executor replacement")
	}

	exec2.CloseExecutionSession(sessionID)

	globalCodexWebsocketSessionStore.mu.Lock()
	_, presentAfterClose := globalCodexWebsocketSessionStore.sessions[storeKey]
	globalCodexWebsocketSessionStore.mu.Unlock()
	if presentAfterClose {
		t.Fatalf("expected session to be removed after explicit close")
	}
}

func TestCodexWebsocketsExecutor_SessionStoreSeparatesDifferentAuths(t *testing.T) {
	sessionID := "test-session-store-separates-auths"
	authA := &cliproxyauth.Auth{ID: "codex-auth-a", ProxyURL: "http://proxy-a.local:8080"}
	authB := &cliproxyauth.Auth{ID: "codex-auth-b", ProxyURL: "http://proxy-a.local:8080"}
	wsURL := "wss://chatgpt.com/backend-api/codex/responses"

	keyA := codexWebsocketSessionStoreKey(sessionID, authA, wsURL)
	keyB := codexWebsocketSessionStoreKey(sessionID, authB, wsURL)

	globalCodexWebsocketSessionStore.mu.Lock()
	delete(globalCodexWebsocketSessionStore.sessions, keyA)
	delete(globalCodexWebsocketSessionStore.sessions, keyB)
	globalCodexWebsocketSessionStore.mu.Unlock()

	exec := NewCodexWebsocketsExecutor(nil)
	sessA := exec.getOrCreateSession(sessionID, authA, wsURL)
	sessB := exec.getOrCreateSession(sessionID, authB, wsURL)

	if sessA == nil || sessB == nil {
		t.Fatalf("expected both sessions to be created")
	}
	if sessA == sessB {
		t.Fatalf("expected same execution session ID to remain isolated across auth IDs")
	}

	exec.CloseExecutionSession(sessionID)
}

func TestManagerRegisterExecutor_PreservesSharedWebsocketSessionsAcrossExecutorReplacement(t *testing.T) {
	tests := []struct {
		name                string
		store               *codexWebsocketSessionStore
		newExecutor         func(*config.Config) cliproxyauth.ProviderExecutor
		newIsolatedExecutor func(*config.Config) cliproxyauth.ProviderExecutor
		getSession          func(cliproxyauth.ProviderExecutor, string, *cliproxyauth.Auth, string) *codexWebsocketSession
	}{
		{
			name:  "CodexAutoExecutor",
			store: globalCodexWebsocketSessionStore,
			newExecutor: func(cfg *config.Config) cliproxyauth.ProviderExecutor {
				return NewCodexAutoExecutor(cfg)
			},
			newIsolatedExecutor: func(cfg *config.Config) cliproxyauth.ProviderExecutor {
				exec := NewCodexAutoExecutor(cfg)
				exec.wsExec.store = &codexWebsocketSessionStore{sessions: make(map[string]*codexWebsocketSession)}
				return exec
			},
			getSession: func(exec cliproxyauth.ProviderExecutor, sessionID string, auth *cliproxyauth.Auth, wsURL string) *codexWebsocketSession {
				return exec.(*CodexAutoExecutor).wsExec.getOrCreateSession(sessionID, auth, wsURL)
			},
		},
		{
			name:  "XAIAutoExecutor",
			store: globalXAIWebsocketSessionStore,
			newExecutor: func(cfg *config.Config) cliproxyauth.ProviderExecutor {
				return NewXAIAutoExecutor(cfg)
			},
			newIsolatedExecutor: func(cfg *config.Config) cliproxyauth.ProviderExecutor {
				exec := NewXAIAutoExecutor(cfg)
				exec.wsExec.store = &codexWebsocketSessionStore{sessions: make(map[string]*codexWebsocketSession)}
				return exec
			},
			getSession: func(exec cliproxyauth.ProviderExecutor, sessionID string, auth *cliproxyauth.Auth, wsURL string) *codexWebsocketSession {
				return exec.(*XAIAutoExecutor).wsExec.getOrCreateSession(sessionID)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sessionID := "test-hot-reload-preserves-ws-" + tt.name
			server, _ := newWebsocketTargetServer(t)
			defer server.Close()
			wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
			auth := &cliproxyauth.Auth{ID: "auth-1", ProxyURL: "direct"}
			storeKey := codexWebsocketSessionStoreKey(sessionID, auth, wsURL)
			if tt.name == "XAIAutoExecutor" {
				storeKey = sessionID // XAI 仍采用它自身的裸 ID store 契约。
			}
			tt.store.mu.Lock()
			delete(tt.store.sessions, storeKey)
			tt.store.mu.Unlock()

			manager := cliproxyauth.NewManager(nil, nil, nil)
			exec1 := tt.newExecutor(&config.Config{})
			manager.RegisterExecutor(exec1)
			t.Cleanup(func() { manager.CloseExecutionSession(sessionID) })

			sess1 := tt.getSession(exec1, sessionID, auth, wsURL)
			if sess1 == nil {
				t.Fatal("expected session to be created")
			}
			conn, physical := newCloseCountingWebsocketConn(t, wsURL)
			sess1.connMu.Lock()
			sess1.conn = conn
			sess1.connCloser = newWebsocketConnectionCloser(conn)
			sess1.wsURL = wsURL
			sess1.authID = "auth-1"
			sess1.readerConn = conn
			sess1.connMu.Unlock()

			exec2 := tt.newExecutor(&config.Config{})
			manager.RegisterExecutor(exec2)

			if got := physical.closes.Load(); got != 0 {
				t.Fatalf("physical websocket closes after executor replacement = %d, want 0", got)
			}
			tt.store.mu.Lock()
			preserved := tt.store.sessions[storeKey]
			tt.store.mu.Unlock()
			if preserved != sess1 {
				t.Fatalf("session after executor replacement = %v, want %v", preserved, sess1)
			}

			manager.RegisterExecutor(tt.newIsolatedExecutor(&config.Config{}))
			if got := physical.closes.Load(); got != 1 {
				t.Fatalf("physical websocket closes after isolated executor replacement = %d, want 1", got)
			}
		})
	}
}

func TestManagerRemove_ClosesOnlyRemovedAuthWebsocketSessions(t *testing.T) {
	tests := []struct {
		name        string
		provider    string
		store       *codexWebsocketSessionStore
		newExecutor func(*config.Config) cliproxyauth.ProviderExecutor
		getSession  func(cliproxyauth.ProviderExecutor, string, *cliproxyauth.Auth, string) *codexWebsocketSession
	}{
		{
			name:     "CodexAutoExecutor",
			provider: "codex",
			store:    globalCodexWebsocketSessionStore,
			newExecutor: func(cfg *config.Config) cliproxyauth.ProviderExecutor {
				return NewCodexAutoExecutor(cfg)
			},
			getSession: func(exec cliproxyauth.ProviderExecutor, sessionID string, auth *cliproxyauth.Auth, wsURL string) *codexWebsocketSession {
				return exec.(*CodexAutoExecutor).wsExec.getOrCreateSession(sessionID, auth, wsURL)
			},
		},
		{
			name:     "XAIAutoExecutor",
			provider: "xai",
			store:    globalXAIWebsocketSessionStore,
			newExecutor: func(cfg *config.Config) cliproxyauth.ProviderExecutor {
				return NewXAIAutoExecutor(cfg)
			},
			getSession: func(exec cliproxyauth.ProviderExecutor, sessionID string, auth *cliproxyauth.Auth, wsURL string) *codexWebsocketSession {
				return exec.(*XAIAutoExecutor).wsExec.getOrCreateSession(sessionID)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sessKeepID := "test-remove-keep-ws-" + tt.name
			sessDropID := "test-remove-drop-ws-" + tt.name
			server, _ := newWebsocketTargetServer(t)
			defer server.Close()
			wsURL := "ws" + strings.TrimPrefix(server.URL, "http")

			manager := cliproxyauth.NewManager(nil, nil, nil)
			exec := tt.newExecutor(&config.Config{})
			manager.RegisterExecutor(exec)
			t.Cleanup(func() {
				manager.CloseExecutionSession(sessKeepID)
				manager.CloseExecutionSession(sessDropID)
			})

			authKeep := &cliproxyauth.Auth{ID: "auth-keep-" + tt.provider, ProxyURL: "direct", Provider: tt.provider, Status: cliproxyauth.StatusActive}
			authDrop := &cliproxyauth.Auth{ID: "auth-drop-" + tt.provider, ProxyURL: "direct", Provider: tt.provider, Status: cliproxyauth.StatusActive}
			if _, errReg := manager.Register(t.Context(), authKeep); errReg != nil {
				t.Fatalf("register authKeep: %v", errReg)
			}
			if _, errReg := manager.Register(t.Context(), authDrop); errReg != nil {
				t.Fatalf("register authDrop: %v", errReg)
			}

			sessKeep := tt.getSession(exec, sessKeepID, authKeep, wsURL)
			connKeep, physicalKeep := newCloseCountingWebsocketConn(t, wsURL)
			sessKeep.connMu.Lock()
			sessKeep.conn = connKeep
			sessKeep.connCloser = newWebsocketConnectionCloser(connKeep)
			sessKeep.wsURL = wsURL
			sessKeep.authID = authKeep.ID
			sessKeep.readerConn = connKeep
			sessKeep.connMu.Unlock()

			sessDrop := tt.getSession(exec, sessDropID, authDrop, wsURL)
			connDrop, physicalDrop := newCloseCountingWebsocketConn(t, wsURL)
			sessDrop.connMu.Lock()
			sessDrop.conn = connDrop
			sessDrop.connCloser = newWebsocketConnectionCloser(connDrop)
			sessDrop.wsURL = wsURL
			sessDrop.authID = authDrop.ID
			sessDrop.readerConn = connDrop
			sessDrop.connMu.Unlock()

			manager.Remove(t.Context(), authDrop.ID)

			if got := physicalDrop.closes.Load(); got != 1 {
				t.Fatalf("removed auth physical websocket closes = %d, want 1", got)
			}
			if got := physicalKeep.closes.Load(); got != 0 {
				t.Fatalf("active sibling auth physical websocket closes = %d, want 0", got)
			}
		})
	}
}

func TestCodexWebsocketsExecutor_ExplicitShutdownClosesAllPhysicalSessions(t *testing.T) {
	server, _ := newWebsocketTargetServer(t)
	defer server.Close()
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	exec := NewCodexWebsocketsExecutor(nil)
	exec.store = &codexWebsocketSessionStore{sessions: make(map[string]*codexWebsocketSession)}
	defer exec.CloseExecutionSession(cliproxyauth.CloseAllExecutionSessionsID)
	assertClosed := make([]func(), 0, 2)
	for _, authID := range []string{"shutdown-a", "shutdown-b"} {
		auth := &cliproxyauth.Auth{ID: authID, ProxyURL: "direct"}
		sess := exec.getOrCreateSession("shared-shutdown-session", auth, wsURL)
		conn, physical := newCloseCountingWebsocketConn(t, wsURL)
		sess.connMu.Lock()
		sess.conn = conn
		sess.connCloser = newWebsocketConnectionCloser(conn)
		sess.connMu.Unlock()
		sess.bindDisconnectOwner(conn)
		assertClosed = append(assertClosed, func() {
			if got := physical.closes.Load(); got != 1 {
				t.Errorf("%s physical closes=%d, want 1", authID, got)
			}
			select {
			case <-sess.closedContext.Done():
			default:
				t.Errorf("%s lifecycle still open", authID)
			}
		})
	}
	exec.CloseExecutionSession(cliproxyauth.CloseAllExecutionSessionsID)
	exec.CloseExecutionSession(cliproxyauth.CloseAllExecutionSessionsID)
	for _, check := range assertClosed {
		check()
	}
	exec.store.mu.Lock()
	defer exec.store.mu.Unlock()
	if len(exec.store.sessions) != 0 || len(exec.store.disconnects) != 0 {
		t.Fatalf("shutdown retained physical=%d logical=%d", len(exec.store.sessions), len(exec.store.disconnects))
	}
}
