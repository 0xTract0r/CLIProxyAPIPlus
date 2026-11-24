package auth

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/home"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executionregistry"
	ce "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

type ordinaryHomeErrorDispatch struct {
	homeExecutionDispatcher
	t     *testing.T
	err   error
	calls int
}

func (d *ordinaryHomeErrorDispatch) RPopAuth(ctx context.Context, model, session string, headers http.Header, count int) ([]byte, error) {
	d.t.Helper()
	d.calls++
	if d.calls != 1 || count != 1 {
		d.t.Fatalf("select calls=%d, requested grants=%d; want one selection for one grant", d.calls, count)
	}
	if d.err != nil {
		return nil, d.err
	}
	return d.homeExecutionDispatcher.RPopAuth(ctx, model, session, headers, count)
}

type ordinaryHomeErrorExecutor struct {
	ordinaryHomeExecutor
	t           *testing.T
	streamCalls int
}

func (e *ordinaryHomeErrorExecutor) ExecuteStream(_ context.Context, a *Auth, _ ce.Request, _ ce.Options) (*ce.StreamResult, error) {
	e.t.Helper()
	e.streamCalls++
	if e.streamCalls != 1 || a == nil || a.ID != "home-auth" {
		e.t.Fatalf("stream calls=%d; expected exactly one call using the synthetic grant", e.streamCalls)
	}
	chunks := make(chan ce.StreamChunk, 1)
	chunks <- ce.StreamChunk{Err: e.err}
	close(chunks)
	return &ce.StreamResult{Chunks: chunks}, nil
}

func fixtureOrdinaryHomeErrorManager(t *testing.T, selectionErr, providerErr error) (*Manager, *ordinaryHomeErrorDispatch, *ordinaryHomeErrorExecutor) {
	t.Helper()
	m := NewManager(nil, nil, nil)
	m.SetConfig(&config.Config{Home: config.HomeConfig{Enabled: true}})
	m.SetRetryConfig(0, 0, 1)
	d := &ordinaryHomeErrorDispatch{t: t, err: selectionErr}
	m.PublishHomeDispatch(d, executionregistry.New(), 1)
	e := &ordinaryHomeErrorExecutor{ordinaryHomeExecutor: ordinaryHomeExecutor{err: providerErr}, t: t}
	m.RegisterExecutor(e)
	return m, d, e
}

func TestOrdinaryHomeSelectionErrorContract(t *testing.T) {
	for _, mode := range []string{"execute", "count", "stream"} {
		t.Run(mode, func(t *testing.T) {
			m, d, e := fixtureOrdinaryHomeErrorManager(t, home.ErrAuthNotFound, nil)
			ctx := context.Background()
			providers := []string{"home-execution"}
			req := ce.Request{Model: "test"}
			var err error
			switch mode {
			case "execute":
				_, err = m.Execute(ctx, providers, req, ce.Options{})
			case "count":
				_, err = m.ExecuteCount(ctx, providers, req, ce.Options{})
			case "stream":
				var stream *ce.StreamResult
				stream, err = m.ExecuteStream(ctx, providers, req, ce.Options{Stream: true})
				if stream != nil {
					t.Fatal("selection failure exposed a stream")
				}
			}
			failure, ok := err.(*Error)
			if !ok || failure.ErrorCode() != "auth_not_found" || failure.StatusCode() != http.StatusServiceUnavailable {
				t.Fatalf("selection error = %T %v, want *Error auth_not_found (503)", err, err)
			}
			if d.calls != 1 || e.calls != 0 || e.streamCalls != 0 {
				t.Fatalf("select calls=%d non-stream calls=%d stream calls=%d", d.calls, e.calls, e.streamCalls)
			}
		})
	}
}

func TestOrdinaryHomeExecuteRefusalKeepsPublicIdentity(t *testing.T) {
	refusal := &ordinaryHomeDenied{code: http.StatusForbidden}
	m, d, e := fixtureOrdinaryHomeErrorManager(t, nil, refusal)
	resp, err := m.Execute(context.Background(), []string{"home-execution"}, ce.Request{Model: "test"}, ce.Options{})
	if err != refusal || len(resp.Payload) != 0 {
		t.Fatalf("execution error = %T %v, payload bytes=%d; want original provider refusal and no payload", err, err, len(resp.Payload))
	}
	if d.calls != 1 || e.calls != 1 || e.streamCalls != 0 {
		t.Fatalf("select calls=%d non-stream calls=%d stream calls=%d", d.calls, e.calls, e.streamCalls)
	}
}

func TestOrdinaryHomeFirstStreamFrameRefusalIsTerminal(t *testing.T) {
	refusal := &ordinaryHomeDenied{code: http.StatusTooManyRequests}
	m, d, e := fixtureOrdinaryHomeErrorManager(t, nil, refusal)
	stream, err := m.ExecuteStream(context.Background(), []string{"home-execution"}, ce.Request{Model: "test"}, ce.Options{Stream: true})
	if stream != nil {
		t.Fatal("first-frame refusal exposed a stream")
	}
	var failure *ordinaryHomeDenied
	if !errors.As(err, &failure) || failure != refusal || statusCodeFromError(err) != http.StatusTooManyRequests || err.Error() != refusal.Error() {
		t.Fatalf("first-frame error = %T %v, want the original provider 429", err, err)
	}
	var carrier interface{ RetryAfter() *time.Duration }
	if !errors.As(err, &carrier) || carrier.RetryAfter() == nil || *carrier.RetryAfter() != 61*time.Second {
		t.Fatal("public stream refusal lost the provider retry delay")
	}
	var bootstrap *streamBootstrapError
	if errors.As(err, &bootstrap) || hasUpstreamExecutionAttempt(err) || isRequestStopError(err) {
		t.Fatalf("public error retained an internal execution wrapper: %T %v", err, err)
	}
	if d.calls != 1 || e.calls != 0 || e.streamCalls != 1 {
		t.Fatalf("select calls=%d non-stream calls=%d stream calls=%d", d.calls, e.calls, e.streamCalls)
	}
}
