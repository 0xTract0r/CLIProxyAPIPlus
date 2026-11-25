package auth

import (
	"context"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	ex "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestBoundedHTTP500Replay(t *testing.T) {
	const provider, model = "offline", "fixture-model"
	type testCase struct {
		name                     string
		budget, status, cooldown int
		wait                     time.Duration
		calls                    int
	}
	for _, tc := range []testCase{
		{"no_budget", 0, 500, -1, 0, 1}, {"no_wait_for_positive_cooldown", 1, 500, 1, 0, 1},
		{"wait_too_short", 1, 500, 2, 50 * time.Millisecond, 1},
		{"bounded", 2, 500, -1, 0, 3}, {"other_status", 2, 503, -1, 0, 1},
		{"credential_replaced", 1, 500, -1, 0, 1}, {"original_removed", 1, 500, -1, 0, 1},
		{"changed_prepared_body", 1, 500, -1, 0, 1}, {"no_upstream_attempt", 1, 500, -1, 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			previous := transientErrorCooldownSeconds.Load()
			SetTransientErrorCooldownSeconds(tc.cooldown)
			t.Cleanup(func() { transientErrorCooldownSeconds.Store(previous) })
			manager := NewManager(nil, &FillFirstSelector{}, nil)
			manager.SetRetryConfig(tc.budget, tc.wait, 0)
			ctx := t.Context()
			first := "a-fixture-" + tc.name
			register := func(id string) {
				t.Helper()
				if _, err := manager.Register(ctx, &Auth{ID: id, Provider: provider}); err != nil {
					t.Fatal(err)
				}
				reg := registry.GetGlobalRegistry()
				reg.RegisterClient(id, provider, []*registry.ModelInfo{{ID: model}})
				t.Cleanup(func() { reg.UnregisterClient(id) })
			}
			register(first)
			// This spare is a sentinel. It must never receive a replay.
			register("z-fixture-" + tc.name)
			var attempts []string
			manager.RegisterExecutor(&callbackSchedulerTestExecutor{schedulerTestExecutor: schedulerTestExecutor{provider: provider},
				execute: func(ctx context.Context, auth *Auth, req ex.Request, opts ex.Options) (ex.Response, error) {
					if tc.name != "no_upstream_attempt" {
						ex.MarkUpstreamAttempt(ctx)
					}
					attempts = append(attempts, auth.ID)
					if auth.ID != first || req.Model != model || string(req.Payload) != "original-body" || opts.Headers.Get("X-Fixture") != "original-header" {
						t.Errorf("unexpected replay target, body or headers")
					}
					if len(attempts) == 1 && (tc.name == "original_removed" || tc.name == "credential_replaced") {
						manager.Remove(ctx, first)
						if tc.name == "credential_replaced" {
							register(first)
						}
					}
					return ex.Response{}, &Error{Code: "upstream_error", HTTPStatus: tc.status, Message: "fixture rejection"}
				},
			})
			opts := ex.Options{Headers: http.Header{"X-Fixture": []string{"original-header"}}}
			if tc.name == "changed_prepared_body" {
				prepared := 0
				opts.RequestAfterAuthInterceptor = func(_ context.Context, _ ex.RequestAfterAuthInterceptRequest) ex.RequestAfterAuthInterceptResponse {
					prepared++
					if prepared > 1 {
						return ex.RequestAfterAuthInterceptResponse{Body: []byte("other-body")}
					}
					return ex.RequestAfterAuthInterceptResponse{}
				}
			}
			_, err := manager.Execute(ctx, []string{provider}, ex.Request{Model: model, Payload: []byte("original-body")}, opts)
			if err == nil || len(attempts) != tc.calls || slices.ContainsFunc(attempts, func(id string) bool { return id != first }) {
				t.Fatalf("attempts=%v status=%d", attempts, statusCodeFromError(err))
			}
			if tc.name != "original_removed" && statusCodeFromError(err) != tc.status {
				t.Errorf("status=%d want=%d", statusCodeFromError(err), tc.status)
			}
			t.Logf("mode=%s attempts=%d model=%s first_only=true status=%d", tc.name, len(attempts), model, statusCodeFromError(err))
		})
	}
}

func TestHTTP500StreamDelivery(t *testing.T) {
	const provider, model = "offline", "fixture-stream"
	before := transientErrorCooldownSeconds.Load()
	SetTransientErrorCooldownSeconds(-1)
	t.Cleanup(func() { transientErrorCooldownSeconds.Store(before) })
	for _, started := range []bool{false, true} {
		name := "immediate_status"
		if started {
			name = "already_delivered"
		}
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			manager := NewManager(nil, &FillFirstSelector{}, nil)
			manager.SetRetryConfig(1, 0, 1)
			id := "fixture-" + name
			reg := registry.GetGlobalRegistry()
			reg.RegisterClient(id, provider, []*registry.ModelInfo{{ID: model}})
			t.Cleanup(func() { reg.UnregisterClient(id) })
			if _, e := manager.Register(ctx, &Auth{ID: id, Provider: provider}); e != nil {
				t.Fatal(e)
			}
			calls := 0
			manager.RegisterExecutor(&callbackSchedulerTestExecutor{schedulerTestExecutor: schedulerTestExecutor{provider: provider},
				executeStream: func(ctx context.Context, auth *Auth, req ex.Request, opts ex.Options) (*ex.StreamResult, error) {
					ex.MarkUpstreamAttempt(ctx)
					calls++
					if auth.ID != id || req.Model != model || !opts.Stream {
						t.Fatal("changed stream destination")
					}
					fault := &Error{HTTPStatus: 500, Message: "synthetic 500"}
					if !started && calls == 1 {
						return nil, fault
					}
					ch := make(chan ex.StreamChunk, 2)
					ch <- ex.StreamChunk{Payload: []byte("data: original\n\n")}
					if started {
						ch <- ex.StreamChunk{Err: fault}
					}
					close(ch)
					return &ex.StreamResult{Chunks: ch}, nil
				},
			})
			res, err := manager.ExecuteStream(ctx, []string{provider}, ex.Request{Model: model}, ex.Options{Stream: true})
			if err != nil {
				t.Fatal(err)
			}
			var chunks []string
			for chunk := range res.Chunks {
				if chunk.Err != nil {
					if statusCodeFromError(chunk.Err) != 500 {
						t.Fatal(chunk.Err)
					}
					chunks = append(chunks, "500")
				} else {
					chunks = append(chunks, string(chunk.Payload))
				}
			}
			want := []string{"data: original\n\n"}
			wantCalls := 2
			if started {
				want = append(want, "500")
				wantCalls = 1
			}
			if calls != wantCalls || !slices.Equal(chunks, want) {
				t.Fatalf("calls=%d chunks=%v", calls, chunks)
			}
		})
	}
}

type http500PreparingExecutor struct {
	*callbackSchedulerTestExecutor
	prepared []uint64
}

func (e *http500PreparingExecutor) ShouldPrepareRequestAuth(*Auth) bool { return true }
func (e *http500PreparingExecutor) PrepareRequestAuth(_ context.Context, auth *Auth) (*Auth, error) {
	e.prepared = append(e.prepared, auth.RegistrationEpoch)
	return nil, nil
}

func TestHTTP500ReplayGuardsPrepareRegistration(t *testing.T) {
	const provider, model = "offline", "fixture-prepare-model"
	previous := transientErrorCooldownSeconds.Load()
	SetTransientErrorCooldownSeconds(-1)
	t.Cleanup(func() { transientErrorCooldownSeconds.Store(previous) })
	for _, mode := range []string{"execute", "count", "stream"} {
		t.Run(mode, func(t *testing.T) {
			id := "fixture-prepare-" + mode
			ctx := t.Context()
			manager := NewManager(nil, &FillFirstSelector{}, nil)
			manager.SetRetryConfig(1, 0, 1)
			reg := registry.GetGlobalRegistry()
			reg.RegisterClient(id, provider, []*registry.ModelInfo{{ID: model}})
			t.Cleanup(func() { reg.UnregisterClient(id) })
			register := func() *Auth {
				t.Helper()
				a, err := manager.Register(ctx, &Auth{ID: id, Provider: provider})
				if err != nil {
					t.Fatal(err)
				}
				return a
			}
			old := register()
			if old.RegistrationEpoch == 0 || old.CredentialVersion == 0 {
				t.Fatal("expected versioned registration")
			}
			var sent []uint64
			fault := func(ctx context.Context, auth *Auth) error {
				sent = append(sent, auth.RegistrationEpoch)
				ex.MarkUpstreamAttempt(ctx)
				return &Error{Code: "upstream_error", HTTPStatus: 500, Message: "first upstream 500"}
			}
			send := func(ctx context.Context, a *Auth, _ ex.Request, _ ex.Options) (ex.Response, error) {
				return ex.Response{}, fault(ctx, a)
			}
			executor := &http500PreparingExecutor{callbackSchedulerTestExecutor: &callbackSchedulerTestExecutor{
				schedulerTestExecutor: schedulerTestExecutor{provider: provider}, execute: send, countTokens: send,
				executeStream: func(ctx context.Context, a *Auth, _ ex.Request, _ ex.Options) (*ex.StreamResult, error) {
					return nil, fault(ctx, a)
				},
			}}
			manager.RegisterExecutor(executor)
			var replacement *Auth
			picks := 0
			// The public notification runs after selection and its generation check,
			// immediately before prepareRequestAuth reads the current registration.
			opts := ex.Options{Metadata: map[string]any{ex.SelectedAuthCallbackMetadataKey: func(selected string) {
				picks++
				if selected != id {
					t.Fatal("unexpected selection")
				}
				if picks == 2 {
					manager.Remove(ctx, id)
					replacement = register()
				}
			}}}
			req := ex.Request{Model: model}
			var err error
			switch mode {
			case "execute":
				_, err = manager.Execute(ctx, []string{provider}, req, opts)
			case "count":
				_, err = manager.ExecuteCount(ctx, []string{provider}, req, opts)
			case "stream":
				opts.Stream = true
				_, err = manager.ExecuteStream(ctx, []string{provider}, req, opts)
			}
			if replacement == nil || replacement.RegistrationEpoch == old.RegistrationEpoch || picks != 2 {
				t.Fatal("replacement did not occur in the replay prepare window")
			}
			want := []uint64{old.RegistrationEpoch}
			t.Logf("picked=%d original=%d replacement=%d prepared=%v sent=%v status=%d", picks, old.RegistrationEpoch, replacement.RegistrationEpoch, executor.prepared, sent, statusCodeFromError(err))
			if !slices.Equal(executor.prepared, want) || !slices.Equal(sent, want) || statusCodeFromError(err) != 500 {
				t.Fatal("replay prepared a replacement or changed original failure")
			}
		})
	}
}
