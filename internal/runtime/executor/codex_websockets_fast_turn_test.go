package executor

import (
	"context"
	"errors"
	"testing"
)

func TestCodexOrdinaryCanceledTurnDoesNotContaminateNextTurn(t *testing.T) {
	e := NewCodexWebsocketsExecutor(nil)
	session := e.getOrCreateSession(t.Name())
	t.Cleanup(func() { e.CloseExecutionSession(t.Name()) })
	release, err := acquireOrdinaryCodexTurn(t.Context(), session, "fixture", "grant")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if cancelledRelease, err := acquireOrdinaryCodexTurn(ctx, session, "fixture", "grant"); cancelledRelease != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled turn did not leave the original owner intact: %v", err)
	}
	release()
	nextRelease, err := acquireOrdinaryCodexTurn(t.Context(), session, "fixture", "grant")
	if err != nil {
		t.Fatal(err)
	}
	nextRelease()
}
