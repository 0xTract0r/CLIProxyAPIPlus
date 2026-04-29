package executor

import (
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
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

	exec1.CloseExecutionSession(cliproxyauth.CloseAllExecutionSessionsID)

	globalCodexWebsocketSessionStore.mu.Lock()
	_, stillPresent := globalCodexWebsocketSessionStore.sessions[storeKey]
	globalCodexWebsocketSessionStore.mu.Unlock()
	if !stillPresent {
		t.Fatalf("expected session to remain after executor replacement close marker")
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
