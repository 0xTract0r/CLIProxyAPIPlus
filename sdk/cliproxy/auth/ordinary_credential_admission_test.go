package auth

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

const ordinaryAdmissionFixtureID = "ordinary-admission-fixture"
const ordinaryAdmissionFixtureModel = "ordinary-test-model"

type ordinaryAdmissionFixtureExecutor struct {
	call   func(context.Context, *Auth, string) error
	stream func(context.Context, *Auth) (*cliproxyexecutor.StreamResult, error)
}

func (*ordinaryAdmissionFixtureExecutor) Identifier() string { return "ordinary-fixture" }
func (e *ordinaryAdmissionFixtureExecutor) Execute(ctx context.Context, a *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{Payload: []byte("ok")}, e.call(ctx, a, "execute")
}
func (e *ordinaryAdmissionFixtureExecutor) CountTokens(ctx context.Context, a *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{Payload: []byte("ok")}, e.call(ctx, a, "count")
}
func (e *ordinaryAdmissionFixtureExecutor) ExecuteStream(ctx context.Context, a *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return e.stream(ctx, a)
}
func (*ordinaryAdmissionFixtureExecutor) Refresh(_ context.Context, a *Auth) (*Auth, error) {
	return a, nil
}
func (*ordinaryAdmissionFixtureExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("unexpected raw HTTP fallback")
}

func ordinaryAdmissionFixtureManager(t *testing.T, selector Selector, executor *ordinaryAdmissionFixtureExecutor, limit int) *Manager {
	t.Helper()
	m := NewManager(nil, selector, nil)
	m.SetConfig(&internalconfig.Config{Routing: internalconfig.RoutingConfig{MaxConcurrentPerCredential: limit}})
	m.RegisterExecutor(executor)
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(ordinaryAdmissionFixtureID, executor.Identifier(), []*registry.ModelInfo{{ID: ordinaryAdmissionFixtureModel}})
	t.Cleanup(func() { reg.UnregisterClient(ordinaryAdmissionFixtureID) })
	if _, err := m.Register(WithSkipPersist(t.Context()), &Auth{ID: ordinaryAdmissionFixtureID, Provider: executor.Identifier()}); err != nil {
		t.Fatal(err)
	}
	return m
}

func waitOrdinaryAdmissionQueue(t *testing.T, gate *ordinaryCredentialAdmission, count int) {
	t.Helper()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	for {
		gate.mu.Lock()
		lane := gate.lanes[ordinaryAdmissionFixtureID]
		n := 0
		if lane != nil {
			n = lane.waiters.Len()
		}
		gate.mu.Unlock()
		if n == count {
			return
		}
		select {
		case <-tick.C:
		case <-ctx.Done():
			t.Fatalf("queued=%d want=%d", n, count)
		}
	}
}

func TestOrdinaryCredentialAdmissionQueuesManagerAndCancels(t *testing.T) {
	started := make(chan string, 4)
	firstRelease := make(chan struct{})
	var calls atomic.Int32
	e := &ordinaryAdmissionFixtureExecutor{call: func(ctx context.Context, a *Auth, name string) error {
		if a.ID != ordinaryAdmissionFixtureID {
			return errors.New("selected credential changed")
		}
		n := calls.Add(1)
		started <- name
		if n == 1 {
			select {
			case <-firstRelease:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}}
	m := ordinaryAdmissionFixtureManager(t, nil, e, 1)
	req := cliproxyexecutor.Request{Model: ordinaryAdmissionFixtureModel}
	p := []string{e.Identifier()}
	ctx, stop := context.WithTimeout(t.Context(), 5*time.Second)
	defer stop()
	first := make(chan error, 1)
	go func() { _, err := m.Execute(ctx, p, req, cliproxyexecutor.Options{}); first <- err }()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}

	cancelCtx, cancel := context.WithCancel(ctx)
	canceled := make(chan error, 1)
	go func() { _, err := m.ExecuteCount(cancelCtx, p, req, cliproxyexecutor.Options{}); canceled <- err }()
	waitOrdinaryAdmissionQueue(t, &m.ordinaryCredentialAdmission, 1)
	second := make(chan error, 1)
	go func() { _, err := m.ExecuteCount(ctx, p, req, cliproxyexecutor.Options{}); second <- err }()
	waitOrdinaryAdmissionQueue(t, &m.ordinaryCredentialAdmission, 2)
	cancel()
	select {
	case err := <-canceled:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if calls.Load() != 1 {
		t.Fatalf("raced past admission: %d", calls.Load())
	}
	close(firstRelease)
	for _, done := range []<-chan error{first, second} {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("executed canceled waiter: %d", calls.Load())
	}
}

func TestOrdinaryCredentialAdmissionRejectsChangedGrant(t *testing.T) {
	for _, change := range []string{"operator disables", "credential replaced"} {
		t.Run(change, func(t *testing.T) { checkOrdinaryAdmissionChangedGrant(t, change) })
	}
}

func checkOrdinaryAdmissionChangedGrant(t *testing.T, change string) {
	t.Helper()
	var calls atomic.Int32
	e := &ordinaryAdmissionFixtureExecutor{call: func(context.Context, *Auth, string) error { calls.Add(1); return nil }, stream: func(ctx context.Context, a *Auth) (*cliproxyexecutor.StreamResult, error) {
		calls.Add(1)
		ch := make(chan cliproxyexecutor.StreamChunk, 1)
		ch <- cliproxyexecutor.StreamChunk{Payload: []byte("ordinary fixture")}
		go func() { <-ctx.Done(); close(ch) }()
		return &cliproxyexecutor.StreamResult{Chunks: ch}, nil
	}}
	m := ordinaryAdmissionFixtureManager(t, nil, e, 1)
	req := cliproxyexecutor.Request{Model: ordinaryAdmissionFixtureModel}
	p := []string{e.Identifier()}
	ctx, stop := context.WithTimeout(t.Context(), 5*time.Second)
	defer stop()
	streamCtx, endStream := context.WithCancel(ctx)
	defer endStream()
	stream, err := m.ExecuteStream(streamCtx, p, req, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-stream.Chunks:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	result := make(chan error, 1)
	go func() { _, e := m.Execute(ctx, p, req, cliproxyexecutor.Options{}); result <- e }()
	waitOrdinaryAdmissionQueue(t, &m.ordinaryCredentialAdmission, 1)
	a, ok := m.GetByID(ordinaryAdmissionFixtureID)
	if !ok {
		t.Fatal("credential not found")
	}
	oldVersion := a.CredentialVersion
	if change == "operator disables" {
		a.Disabled = true
	} else {
		a.Attributes = map[string]string{"api_key": "replacement-fixture"}
	}
	if _, err = m.Update(WithSkipPersist(ctx), a); err != nil {
		t.Fatal(err)
	}
	if change == "credential replaced" {
		updated, _ := m.GetByID(ordinaryAdmissionFixtureID)
		if updated == nil || updated.CredentialVersion == oldVersion {
			t.Fatal("fixture did not replace credential version")
		}
	}
	endStream()
	select {
	case err = <-result:
		if statusCodeFromError(err) != 503 || errorCodeFromError(err) != "queued_credential_changed" {
			t.Fatalf("queued revoked credential: %v", err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if calls.Load() != 1 {
		t.Fatalf("executed revoked credential: %d", calls.Load())
	}
	waitOrdinaryAdmissionQueue(t, &m.ordinaryCredentialAdmission, 0)
}

func TestOrdinaryCredentialAdmissionReleasesCompletedStream(t *testing.T) {
	finish := make(chan struct{})
	e := &ordinaryAdmissionFixtureExecutor{
		call: func(context.Context, *Auth, string) error { return nil },
		stream: func(ctx context.Context, _ *Auth) (*cliproxyexecutor.StreamResult, error) {
			ch := make(chan cliproxyexecutor.StreamChunk, 1)
			ch <- cliproxyexecutor.StreamChunk{Payload: []byte("fixture")}
			go func() {
				defer close(ch)
				select {
				case <-finish:
				case <-ctx.Done():
				}
			}()
			return &cliproxyexecutor.StreamResult{Chunks: ch}, nil
		},
	}
	m := ordinaryAdmissionFixtureManager(t, nil, e, 1)
	req := cliproxyexecutor.Request{Model: ordinaryAdmissionFixtureModel}
	providers := []string{e.Identifier()}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	out, err := m.ExecuteStream(ctx, providers, req, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for range out.Chunks {
		}
	}()
	waiter := make(chan error, 1)
	go func() { _, err := m.Execute(ctx, providers, req, cliproxyexecutor.Options{}); waiter <- err }()
	waitOrdinaryAdmissionQueue(t, &m.ordinaryCredentialAdmission, 1)
	close(finish)
	select {
	case err := <-waiter:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	m.ordinaryCredentialAdmission.mu.Lock()
	defer m.ordinaryCredentialAdmission.mu.Unlock()
	if len(m.ordinaryCredentialAdmission.lanes) != 0 {
		t.Fatal("ordinary admission retained a finished lane")
	}
}

func TestOrdinaryCredentialAdmissionFullQueueIsLocalBusy(t *testing.T) {
	var gate ordinaryCredentialAdmission
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	limit := func() int { return 1 }
	release, _, err := gate.acquire(ctx, ordinaryAdmissionFixtureID, limit)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	done := make(chan error, ordinaryCredentialQueueSize)
	for i := 0; i < ordinaryCredentialQueueSize; i++ {
		go func() {
			free, _, e := gate.acquire(ctx, ordinaryAdmissionFixtureID, limit)
			if free != nil {
				free()
			}
			done <- e
		}()
	}
	waitOrdinaryAdmissionQueue(t, &gate, ordinaryCredentialQueueSize)
	if _, _, err := gate.acquire(ctx, ordinaryAdmissionFixtureID, limit); statusCodeFromError(err) != 503 || !isRequestStopError(err) {
		t.Fatalf("queue full error: %v", err)
	}
	cancel()
	for i := 0; i < ordinaryCredentialQueueSize; i++ {
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("queue did not cancel")
		}
	}
}

func TestOrdinaryCredentialAdmissionOptionalScopedAffinity(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		name := "disabled"
		if enabled {
			name = "enabled"
		}
		t.Run(name, func(t *testing.T) {
			e := &ordinaryAdmissionFixtureExecutor{call: func(_ context.Context, a *Auth, _ string) error {
				if a.ID != ordinaryAdmissionFixtureID {
					return errors.New("unexpected credential")
				}
				return nil
			}}
			var selector Selector
			var affinity *SessionAffinitySelector
			if enabled {
				affinity = NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{TTL: time.Minute, RebindOn: SessionAffinityRebindOnNever})
				selector = affinity
				t.Cleanup(affinity.Stop)
			}
			m := ordinaryAdmissionFixtureManager(t, selector, e, 1)
			req := cliproxyexecutor.Request{Model: ordinaryAdmissionFixtureModel, Payload: []byte(`{"input":"fixture"}`)}
			p := []string{e.Identifier()}
			options := func(caller string) cliproxyexecutor.Options {
				return cliproxyexecutor.Options{Headers: http.Header{"X-Session-Id": {"ordinary-business-session"}},
					OriginalRequest: req.Payload, Metadata: map[string]any{cliproxyexecutor.CallerScopeMetadataKey: caller}}
			}
			for i := 0; i < 2; i++ {
				if _, err := m.Execute(t.Context(), p, req, options("fixture-caller")); err != nil {
					t.Fatal(err)
				}
			}
			if !enabled {
				if _, ok := m.Selector().(*SessionAffinitySelector); ok {
					t.Fatal("affinity enabled implicitly")
				}
				return
			}
			if id, ok := affinity.boundAuthForRequest(p, req.Model, options("fixture-caller")); !ok || id != ordinaryAdmissionFixtureID {
				t.Fatalf("session binding=%q found=%v", id, ok)
			}
			if id, ok := affinity.boundAuthForRequest(p, req.Model, options("different-caller")); ok {
				t.Fatalf("caller crossed binding: %q", id)
			}
		})
	}
}

type ordinaryAdmissionFixtureRefusal struct{}

func (ordinaryAdmissionFixtureRefusal) Error() string              { return "fixture provider refusal" }
func (ordinaryAdmissionFixtureRefusal) StatusCode() int            { return 429 }
func (ordinaryAdmissionFixtureRefusal) RetryAfter() *time.Duration { d := time.Minute; return &d }
func (ordinaryAdmissionFixtureRefusal) Headers() http.Header {
	return http.Header{"Retry-After": {"60"}}
}

func TestOrdinaryCredentialAdmissionPendingRequestHonorsProviderRefusal(t *testing.T) {
	var calls atomic.Int32
	started := make(chan struct{})
	resume := make(chan struct{})
	e := &ordinaryAdmissionFixtureExecutor{call: func(ctx context.Context, _ *Auth, _ string) error {
		if calls.Add(1) != 1 {
			return errors.New("queued request reached refused provider")
		}
		close(started)
		select {
		case <-resume:
		case <-ctx.Done():
			return ctx.Err()
		}
		cliproxyexecutor.MarkUpstreamAttempt(ctx)
		return ordinaryAdmissionFixtureRefusal{}
	}}
	m := ordinaryAdmissionFixtureManager(t, nil, e, 1)
	req := cliproxyexecutor.Request{Model: ordinaryAdmissionFixtureModel}
	p := []string{e.Identifier()}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	start := func() <-chan error {
		done := make(chan error, 1)
		go func() { _, err := m.Execute(ctx, p, req, cliproxyexecutor.Options{}); done <- err }()
		return done
	}
	first := start()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	second := start()
	waitOrdinaryAdmissionQueue(t, &m.ordinaryCredentialAdmission, 1)
	close(resume)
	for _, done := range []<-chan error{first, second} {
		select {
		case err := <-done:
			if statusCodeFromError(err) != 429 || SafeResponseHeaders(err).Get("Retry-After") == "" {
				t.Fatalf("refusal lost status or Retry-After: %v", err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("refused fixture called %d times", calls.Load())
	}
}
