package auth

import (
	"context"
	"errors"
	"net"
	"net/http"
	"syscall"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	ce "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestOrdinaryPreDispatchSelectionErrorPrecedence(t *testing.T) {
	earlierPreparation := errors.New("local preparation failed before any request")
	current := &Error{Code: "home_unavailable", Message: "selection service unavailable", HTTPStatus: http.StatusServiceUnavailable}
	if shouldReturnLastErrorOnPickFailure(true, earlierPreparation, current) {
		t.Fatal("the prior local error hid the specific Home selection failure")
	}

	invalid := &Error{Code: "fixture_invalid_input", Message: "invalid request input", HTTPStatus: http.StatusBadRequest}
	for _, homeMode := range []bool{false, true} {
		if shouldReturnLastErrorOnPickFailure(homeMode, earlierPreparation, wrapRequestStopError(invalid)) {
			t.Fatalf("the prior local error hid a terminal selection failure (home=%t)", homeMode)
		}
	}
}

func TestOrdinaryPreDispatchWaitBounds(t *testing.T) {
	const (
		id       = "ordinary-predispatch-wait"
		model    = "ordinary-predispatch-model"
		provider = "codex"
		maxWait  = 2 * time.Second
	)
	ctx := context.Background()
	m := NewManager(nil, nil, nil)
	m.SetRetryConfig(1, maxWait, 1)
	recoverAt := time.Now().Add(400 * time.Millisecond)
	grant := &Auth{ID: id, Provider: provider, Status: StatusActive, ModelStates: map[string]*ModelState{
		model: {Status: StatusActive, Unavailable: true, NextRetryAfter: recoverAt,
			LastError: &Error{Message: "synthetic local availability deadline", HTTPStatus: http.StatusServiceUnavailable}},
	}}
	models := registry.GetGlobalRegistry()
	models.RegisterClient(id, provider, []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { models.UnregisterClient(id) })
	if _, err := m.Register(ctx, grant); err != nil {
		t.Fatal(err)
	}

	refusedDial := &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	options := ce.Options{Metadata: map[string]any{ce.PinnedAuthMetadataKey: id}}
	decide := func(err error, round int, ceiling time.Duration) (time.Duration, bool) {
		return m.shouldRetryAfterErrorWithAttempted(ctx, options, err, round, []string{provider}, model, ceiling, -1, 1, nil)
	}
	// A local positive deadline cannot be shortened to fit the caller's cap.
	if wait, ok := decide(refusedDial, 0, 5*time.Millisecond); ok || wait != 0 {
		t.Fatalf("over-limit local wait was accepted: (%s, %t)", wait, ok)
	}
	if wait, ok := decide(wrapRequestStopError(&Error{HTTPStatus: http.StatusBadRequest}), 0, maxWait); ok || wait != 0 {
		t.Fatalf("terminal prevalidation failure consumed retry budget: (%s, %t)", wait, ok)
	}
	if wait, ok := decide(refusedDial, 1, maxWait); ok || wait != 0 {
		t.Fatalf("an exhausted budget admitted another round: (%s, %t)", wait, ok)
	}

	before := time.Now()
	wait, ok := decide(refusedDial, 0, maxWait)
	after := time.Now()
	if !ok || wait <= 0 || before.Add(wait).After(recoverAt) || after.Add(wait).Before(recoverAt) {
		t.Fatalf("single-grant wait = (%s, %t), want remaining local deadline", wait, ok)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := waitForCooldown(cancelled, wait, maxWait); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled wait = %v", err)
	}
	if err := waitForCooldown(ctx, wait, maxWait); err != nil {
		t.Fatal(err)
	}
	if time.Now().Before(recoverAt) {
		t.Fatal("the real timer returned before the grant's local deadline")
	}
}
