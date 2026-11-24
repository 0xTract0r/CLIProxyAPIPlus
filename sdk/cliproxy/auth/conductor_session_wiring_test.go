package auth

import (
	"context"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

type sessionContextCaptureExecutor struct {
	capturedSessionID string
	called            bool
}

func (e *sessionContextCaptureExecutor) Identifier() string { return "codex" }

func (e *sessionContextCaptureExecutor) Execute(ctx context.Context, _ *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.called = true
	e.capturedSessionID = SessionIDFromContext(ctx)
	return cliproxyexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
}

func (e *sessionContextCaptureExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}

func (e *sessionContextCaptureExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *sessionContextCaptureExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *sessionContextCaptureExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

// TestManagerExecuteWiresSessionIntoExecutorContext is the end-to-end wiring
// proof: driving the real Manager.Execute entry point with a request that
// carries a session must make that session observable on the exact ctx the
// executor receives -- which is where the per-request UsageReporter is built and
// reporter.Publish(ctx, ...) is invoked. Combined with the already-covered sink
// fallback (internal/usage RequestStatistics.Record reading SessionIDFromContext)
// and the P6 aggregation, this closes the "session count stays 0 under real
// traffic" gap.
func TestManagerExecuteWiresSessionIntoExecutorContext(t *testing.T) {
	authID := "codex-session-wiring-test.json"
	modelID := "session-wiring-test-model"
	registry.GetGlobalRegistry().RegisterClient(authID, "codex", []*registry.ModelInfo{{ID: modelID, Object: "model", Type: "codex"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(authID) })

	exec := &sessionContextCaptureExecutor{}
	manager := NewManager(nil, nil, nil)
	manager.RegisterExecutor(exec)
	if _, err := manager.Register(context.Background(), &Auth{
		ID:       authID,
		Provider: "codex",
		ProxyURL: "http://test-proxy:8080",
		Metadata: map[string]any{"type": "codex"},
	}); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	opts := cliproxyexecutor.Options{
		Headers: http.Header{"X-Session-Id": {"sess-e2e"}},
	}
	if _, err := manager.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: modelID}, opts); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !exec.called {
		t.Fatal("executor was not called")
	}
	if exec.capturedSessionID != "header:sess-e2e" {
		t.Fatalf("session id on executor ctx = %q, want %q", exec.capturedSessionID, "header:sess-e2e")
	}
	if _, err := manager.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: modelID}, cliproxyexecutor.Options{}); err != nil {
		t.Fatalf("execute without a session: %v", err)
	}
	if exec.capturedSessionID != "" {
		t.Fatalf("a separate request inherited a session: %q", exec.capturedSessionID)
	}

}
