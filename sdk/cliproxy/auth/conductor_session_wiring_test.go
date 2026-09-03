package auth

import (
	"context"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// TestContextWithSessionIDExtractsExplicitHeaderSession proves the request-entry
// helper resolves a session identifier from the same inputs the selector uses
// and stores it on ctx, which is what the P6 usage aggregation was missing.
func TestContextWithSessionIDExtractsExplicitHeaderSession(t *testing.T) {
	opts := cliproxyexecutor.Options{
		Headers: http.Header{"X-Session-Id": {"sess-xyz"}},
	}
	ctx := contextWithSessionID(context.Background(), opts)
	if got := SessionIDFromContext(ctx); got != "header:sess-xyz" {
		t.Fatalf("SessionIDFromContext = %q, want %q", got, "header:sess-xyz")
	}
}

// TestContextWithSessionIDExtractsClaudeUserIDSession covers the primary
// real-traffic path: Claude Code sends its session inside metadata.user_id as a
// JSON object with a session_id field, which ExtractSessionID maps to a
// "claude:"-prefixed id.
func TestContextWithSessionIDExtractsClaudeUserIDSession(t *testing.T) {
	opts := cliproxyexecutor.Options{
		OriginalRequest: []byte(`{"metadata":{"user_id":"{\"device_id\":\"d1\",\"session_id\":\"abc-123\"}"}}`),
	}
	ctx := contextWithSessionID(context.Background(), opts)
	if got := SessionIDFromContext(ctx); got != "claude:abc-123" {
		t.Fatalf("SessionIDFromContext = %q, want %q", got, "claude:abc-123")
	}
}

// TestContextWithSessionIDLeavesUnclassifiableUnset enforces the "unknown is not
// a number" contract at the entry point: a request that cannot be classified
// into any session must leave ctx without a session value, so unclassifiable
// traffic is never folded into a fabricated shared bucket by the aggregation.
func TestContextWithSessionIDLeavesUnclassifiableUnset(t *testing.T) {
	ctx := contextWithSessionID(context.Background(), cliproxyexecutor.Options{})
	if got := SessionIDFromContext(ctx); got != "" {
		t.Fatalf("SessionIDFromContext = %q, want empty for unclassifiable request", got)
	}
}

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
}
