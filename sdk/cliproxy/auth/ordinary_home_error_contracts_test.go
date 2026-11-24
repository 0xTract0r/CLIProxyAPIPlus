package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	t            *testing.T
	err          error
	payload      []byte
	requestRetry *int
	calls        int
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
	if d.payload != nil {
		return d.payload, nil
	}
	if d.requestRetry != nil {
		return json.Marshal(homeAuthDispatchResponse{
			RequestRetry: d.requestRetry,
			Auth: Auth{ID: "home-auth", Provider: "home-execution", Status: StatusActive,
				Metadata: map[string]any{"request_retry": 3}},
		})
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

// Initial selection failures never reach an executor. This does not cover
// selection errors after an earlier execution or assert any retry semantics.
func TestOrdinaryHomeInitialSelectionFailures(t *testing.T) {
	for _, tc := range []struct {
		name         string
		selectionErr error
		payload      []byte
		status       int
	}{
		{name: "home_unavailable", selectionErr: errors.New("synthetic local selection unavailable"), status: http.StatusServiceUnavailable},
		{name: "invalid_auth", payload: []byte(`{}`), status: http.StatusBadGateway},
		{name: "invalid_home_concurrency", payload: []byte(`{"concurrency":"invalid","auth":{"id":"home-auth","provider":"home-execution","status":"active"}}`), status: http.StatusBadGateway},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, mode := range []string{"execute", "count", "stream"} {
				t.Run(mode, func(t *testing.T) {
					m, d, e := fixtureOrdinaryHomeErrorManager(t, tc.selectionErr, nil)
					d.payload = tc.payload
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
							t.Fatal("initial selection failure exposed a stream")
						}
					}
					failure, ok := err.(*Error)
					if !ok || failure.ErrorCode() != tc.name || failure.StatusCode() != tc.status {
						t.Fatalf("selection error = %T %v, want *Error %s (%d)", err, err, tc.name, tc.status)
					}
					if d.calls != 1 || e.calls != 0 || e.streamCalls != 0 {
						t.Fatalf("select calls=%d non-stream calls=%d stream calls=%d", d.calls, e.calls, e.streamCalls)
					}
				})
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
	started := time.Now()
	stream, err := m.ExecuteStream(context.Background(), []string{"home-execution"}, ce.Request{Model: "test"}, ce.Options{Stream: true})
	returned := time.Now()
	if stream != nil {
		t.Fatal("first-frame refusal exposed a stream")
	}
	var failure *ordinaryHomeDenied
	if !errors.As(err, &failure) || failure != refusal || statusCodeFromError(err) != http.StatusTooManyRequests || err.Error() != refusal.Error() {
		t.Fatalf("first-frame error = %T %v, want the original provider 429", err, err)
	}
	const providerDelay = 61 * time.Second
	if original := failure.RetryAfter(); original == nil || *original != providerDelay {
		t.Fatalf("original provider delay changed: %v", original)
	}
	var carrier interface{ RetryAfter() *time.Duration }
	if !errors.As(err, &carrier) {
		t.Fatal("public stream refusal lost the provider retry deadline")
	}
	sampledBefore := time.Now()
	remaining := carrier.RetryAfter()
	sampledAfter := time.Now()
	if remaining == nil || *remaining <= 0 {
		t.Fatalf("public stream refusal has no positive retry deadline: %T", carrier)
	}
	// RetryAfter reports the time remaining. The original deadline must
	// still be at least a full provider delay from this request's start and
	// no later than that delay measured from publication of the error.
	if sampledAfter.Add(*remaining).Before(started.Add(providerDelay)) ||
		sampledBefore.Add(*remaining).After(returned.Add(providerDelay)) {
		t.Fatalf("public %T RetryAfter = %s, outside the deadline bounds for provider delay %s", carrier, *remaining, providerDelay)
	}
	t.Logf("public error=%T first RetryAfter=%T remaining=%s original=%s public header=%q",
		err, carrier, *remaining, providerDelay, SafeResponseHeaders(err).Get("Retry-After"))
	var bootstrap *streamBootstrapError
	if errors.As(err, &bootstrap) || hasUpstreamExecutionAttempt(err) || isRequestStopError(err) {
		t.Fatalf("public error retained an internal execution wrapper: %T %v", err, err)
	}
	if d.calls != 1 || e.calls != 0 || e.streamCalls != 1 {
		t.Fatalf("select calls=%d non-stream calls=%d stream calls=%d", d.calls, e.calls, e.streamCalls)
	}
}

func TestOrdinaryPreferredExecutionErrorPriority(t *testing.T) {
	upstream := &Error{Code: "fixture_upstream", Message: "synthetic provider failure", HTTPStatus: http.StatusBadGateway}

	t.Run("original_provider_error_beats_internal_failure", func(t *testing.T) {
		internal := errors.New("synthetic internal failure")
		got := preferredExecutionAttemptError(internal, upstream)
		if unwrapExecutionBoundaryError(got) != upstream || statusCodeFromError(got) != http.StatusBadGateway ||
			errors.Is(got, internal) {
			t.Fatalf("preferred error = %v, want the original provider error and status", got)
		}
	})

	for _, tc := range []struct {
		name  string
		cause error
	}{
		{"cancellation_beats_provider_error", context.Canceled},
		{"deadline_beats_provider_error", context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fallback := fmt.Errorf("request ended: %w", tc.cause)
			got := preferredExecutionAttemptError(fallback, upstream)
			if got != fallback || !errors.Is(got, tc.cause) || errors.Is(got, upstream) {
				t.Fatalf("preferred error = %v, want the original wrapped context error %v", got, fallback)
			}
		})
	}
}

// The dispatcher returns a single synthetic grant with an explicit zero budget.
// A terminal failure remains visible at each public entry point with no second call.
func TestOrdinaryHomeSingleCredentialTerminalErrorWithZeroBudget(t *testing.T) {
	for _, mode := range []string{"execute", "count", "stream"} {
		t.Run(mode, func(t *testing.T) {
			failure := &Error{Code: "fixture_upstream_unavailable", Message: "synthetic provider failure", HTTPStatus: http.StatusBadGateway}
			m, d, e := fixtureOrdinaryHomeErrorManager(t, nil, failure)
			m.SetRetryConfig(3, time.Second, 1)
			noRetries := 0
			d.requestRetry = &noRetries
			ctx := context.Background()
			providers := []string{"home-execution"}
			req := ce.Request{Model: "test"}
			var err error
			wantCalls, wantStreamCalls := 1, 0
			switch mode {
			case "execute":
				_, err = m.Execute(ctx, providers, req, ce.Options{})
			case "count":
				_, err = m.ExecuteCount(ctx, providers, req, ce.Options{})
			case "stream":
				wantCalls, wantStreamCalls = 0, 1
				var stream *ce.StreamResult
				stream, err = m.ExecuteStream(ctx, providers, req, ce.Options{Stream: true})
				if stream != nil {
					t.Fatal("terminal first-frame failure exposed a stream")
				}
			}
			if !errors.Is(err, failure) || statusCodeFromError(err) != http.StatusBadGateway {
				t.Fatalf("execution error = %T %v; want the original 502", err, err)
			}
			if d.calls != 1 || e.calls != wantCalls || e.streamCalls != wantStreamCalls {
				t.Fatalf("select calls=%d non-stream calls=%d stream calls=%d", d.calls, e.calls, e.streamCalls)
			}
		})
	}
}
