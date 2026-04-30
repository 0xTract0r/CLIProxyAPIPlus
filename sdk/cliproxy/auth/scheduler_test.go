package auth

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type schedulerTestExecutor struct{}

func (schedulerTestExecutor) Identifier() string { return "test" }

func (schedulerTestExecutor) Execute(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (schedulerTestExecutor) ExecuteStream(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}

func (schedulerTestExecutor) Refresh(ctx context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (schedulerTestExecutor) CountTokens(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (schedulerTestExecutor) HttpRequest(ctx context.Context, auth *Auth, req *http.Request) (*http.Response, error) {
	return nil, nil
}

type trackingSelector struct {
	calls      int
	lastAuthID []string
}

func (s *trackingSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	s.calls++
	s.lastAuthID = s.lastAuthID[:0]
	for _, auth := range auths {
		s.lastAuthID = append(s.lastAuthID, auth.ID)
	}
	if len(auths) == 0 {
		return nil, nil
	}
	return auths[len(auths)-1], nil
}

func newSchedulerForTest(selector Selector, auths ...*Auth) *authScheduler {
	scheduler := newAuthScheduler(selector)
	scheduler.rebuild(auths)
	return scheduler
}

func registerSchedulerModels(t *testing.T, provider string, model string, authIDs ...string) {
	t.Helper()
	reg := registry.GetGlobalRegistry()
	for _, authID := range authIDs {
		reg.RegisterClient(authID, provider, []*registry.ModelInfo{{ID: model}})
	}
	t.Cleanup(func() {
		for _, authID := range authIDs {
			reg.UnregisterClient(authID)
		}
	})
}

func TestSchedulerPick_RoundRobinHighestPriority(t *testing.T) {
	t.Parallel()

	scheduler := newSchedulerForTest(
		&RoundRobinSelector{},
		&Auth{ID: "low", Provider: "gemini", Attributes: map[string]string{"priority": "0"}},
		&Auth{ID: "high-b", Provider: "gemini", Attributes: map[string]string{"priority": "10"}},
		&Auth{ID: "high-a", Provider: "gemini", Attributes: map[string]string{"priority": "10"}},
	)

	want := []string{"high-a", "high-b", "high-a"}
	for index, wantID := range want {
		got, errPick := scheduler.pickSingle(context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil)
		if errPick != nil {
			t.Fatalf("pickSingle() #%d error = %v", index, errPick)
		}
		if got == nil {
			t.Fatalf("pickSingle() #%d auth = nil", index)
		}
		if got.ID != wantID {
			t.Fatalf("pickSingle() #%d auth.ID = %q, want %q", index, got.ID, wantID)
		}
	}
}

func TestSchedulerPick_FillFirstSticksToFirstReady(t *testing.T) {
	t.Parallel()

	scheduler := newSchedulerForTest(
		&FillFirstSelector{},
		&Auth{ID: "b", Provider: "gemini"},
		&Auth{ID: "a", Provider: "gemini"},
		&Auth{ID: "c", Provider: "gemini"},
	)

	for index := 0; index < 3; index++ {
		got, errPick := scheduler.pickSingle(context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil)
		if errPick != nil {
			t.Fatalf("pickSingle() #%d error = %v", index, errPick)
		}
		if got == nil {
			t.Fatalf("pickSingle() #%d auth = nil", index)
		}
		if got.ID != "a" {
			t.Fatalf("pickSingle() #%d auth.ID = %q, want %q", index, got.ID, "a")
		}
	}
}

func TestSchedulerPick_PromotesExpiredCooldownBeforePick(t *testing.T) {
	t.Parallel()

	model := "gemini-2.5-pro"
	registerSchedulerModels(t, "gemini", model, "cooldown-expired")
	scheduler := newSchedulerForTest(
		&RoundRobinSelector{},
		&Auth{
			ID:       "cooldown-expired",
			Provider: "gemini",
			ModelStates: map[string]*ModelState{
				model: {
					Status:         StatusError,
					Unavailable:    true,
					NextRetryAfter: time.Now().Add(-1 * time.Second),
				},
			},
		},
	)

	got, errPick := scheduler.pickSingle(context.Background(), "gemini", model, cliproxyexecutor.Options{}, nil)
	if errPick != nil {
		t.Fatalf("pickSingle() error = %v", errPick)
	}
	if got == nil {
		t.Fatalf("pickSingle() auth = nil")
	}
	if got.ID != "cooldown-expired" {
		t.Fatalf("pickSingle() auth.ID = %q, want %q", got.ID, "cooldown-expired")
	}
}

func TestSchedulerPick_GeminiVirtualParentUsesTwoLevelRotation(t *testing.T) {
	t.Parallel()

	registerSchedulerModels(t, "gemini-cli", "gemini-2.5-pro", "cred-a::proj-1", "cred-a::proj-2", "cred-b::proj-1", "cred-b::proj-2")
	scheduler := newSchedulerForTest(
		&RoundRobinSelector{},
		&Auth{ID: "cred-a::proj-1", Provider: "gemini-cli", Attributes: map[string]string{"gemini_virtual_parent": "cred-a"}},
		&Auth{ID: "cred-a::proj-2", Provider: "gemini-cli", Attributes: map[string]string{"gemini_virtual_parent": "cred-a"}},
		&Auth{ID: "cred-b::proj-1", Provider: "gemini-cli", Attributes: map[string]string{"gemini_virtual_parent": "cred-b"}},
		&Auth{ID: "cred-b::proj-2", Provider: "gemini-cli", Attributes: map[string]string{"gemini_virtual_parent": "cred-b"}},
	)

	wantParents := []string{"cred-a", "cred-b", "cred-a", "cred-b"}
	wantIDs := []string{"cred-a::proj-1", "cred-b::proj-1", "cred-a::proj-2", "cred-b::proj-2"}
	for index := range wantIDs {
		got, errPick := scheduler.pickSingle(context.Background(), "gemini-cli", "gemini-2.5-pro", cliproxyexecutor.Options{}, nil)
		if errPick != nil {
			t.Fatalf("pickSingle() #%d error = %v", index, errPick)
		}
		if got == nil {
			t.Fatalf("pickSingle() #%d auth = nil", index)
		}
		if got.ID != wantIDs[index] {
			t.Fatalf("pickSingle() #%d auth.ID = %q, want %q", index, got.ID, wantIDs[index])
		}
		if got.Attributes["gemini_virtual_parent"] != wantParents[index] {
			t.Fatalf("pickSingle() #%d parent = %q, want %q", index, got.Attributes["gemini_virtual_parent"], wantParents[index])
		}
	}
}

func TestSchedulerPick_CodexWebsocketPrefersWebsocketEnabledSubset(t *testing.T) {
	t.Parallel()

	scheduler := newSchedulerForTest(
		&RoundRobinSelector{},
		&Auth{ID: "codex-http", Provider: "codex"},
		&Auth{ID: "codex-ws-a", Provider: "codex", Attributes: map[string]string{"websockets": "true"}},
		&Auth{ID: "codex-ws-b", Provider: "codex", Attributes: map[string]string{"websockets": "true"}},
	)

	ctx := cliproxyexecutor.WithDownstreamWebsocket(context.Background())
	want := []string{"codex-ws-a", "codex-ws-b", "codex-ws-a"}
	for index, wantID := range want {
		got, errPick := scheduler.pickSingle(ctx, "codex", "", cliproxyexecutor.Options{}, nil)
		if errPick != nil {
			t.Fatalf("pickSingle() #%d error = %v", index, errPick)
		}
		if got == nil {
			t.Fatalf("pickSingle() #%d auth = nil", index)
		}
		if got.ID != wantID {
			t.Fatalf("pickSingle() #%d auth.ID = %q, want %q", index, got.ID, wantID)
		}
	}
}

func TestSchedulerPick_CodexWebsocketPrefersWebsocketEnabledAcrossPriorities(t *testing.T) {
	t.Parallel()

	scheduler := newSchedulerForTest(
		&RoundRobinSelector{},
		&Auth{ID: "codex-http", Provider: "codex", Attributes: map[string]string{"priority": "10"}},
		&Auth{ID: "codex-ws-a", Provider: "codex", Attributes: map[string]string{"priority": "0", "websockets": "true"}},
		&Auth{ID: "codex-ws-b", Provider: "codex", Attributes: map[string]string{"priority": "0", "websockets": "true"}},
	)

	ctx := cliproxyexecutor.WithDownstreamWebsocket(context.Background())
	want := []string{"codex-ws-a", "codex-ws-b", "codex-ws-a"}
	for index, wantID := range want {
		got, errPick := scheduler.pickSingle(ctx, "codex", "", cliproxyexecutor.Options{}, nil)
		if errPick != nil {
			t.Fatalf("pickSingle() #%d error = %v", index, errPick)
		}
		if got == nil {
			t.Fatalf("pickSingle() #%d auth = nil", index)
		}
		if got.ID != wantID {
			t.Fatalf("pickSingle() #%d auth.ID = %q, want %q", index, got.ID, wantID)
		}
	}
}

func TestSchedulerPick_MixedProvidersUsesWeightedProviderRotationOverReadyCandidates(t *testing.T) {
	t.Parallel()

	scheduler := newSchedulerForTest(
		&RoundRobinSelector{},
		&Auth{ID: "gemini-a", Provider: "gemini"},
		&Auth{ID: "gemini-b", Provider: "gemini"},
		&Auth{ID: "claude-a", Provider: "claude"},
	)

	wantProviders := []string{"gemini", "gemini", "claude", "gemini"}
	wantIDs := []string{"gemini-a", "gemini-b", "claude-a", "gemini-a"}
	for index := range wantProviders {
		got, provider, errPick := scheduler.pickMixed(context.Background(), []string{"gemini", "claude"}, "", cliproxyexecutor.Options{}, nil)
		if errPick != nil {
			t.Fatalf("pickMixed() #%d error = %v", index, errPick)
		}
		if got == nil {
			t.Fatalf("pickMixed() #%d auth = nil", index)
		}
		if provider != wantProviders[index] {
			t.Fatalf("pickMixed() #%d provider = %q, want %q", index, provider, wantProviders[index])
		}
		if got.ID != wantIDs[index] {
			t.Fatalf("pickMixed() #%d auth.ID = %q, want %q", index, got.ID, wantIDs[index])
		}
	}
}

func TestSchedulerPick_MixedProvidersPrefersHighestPriorityTier(t *testing.T) {
	t.Parallel()

	model := "gpt-default"
	registerSchedulerModels(t, "provider-low", model, "low")
	registerSchedulerModels(t, "provider-high-a", model, "high-a")
	registerSchedulerModels(t, "provider-high-b", model, "high-b")

	scheduler := newSchedulerForTest(
		&RoundRobinSelector{},
		&Auth{ID: "low", Provider: "provider-low", Attributes: map[string]string{"priority": "4"}},
		&Auth{ID: "high-a", Provider: "provider-high-a", Attributes: map[string]string{"priority": "7"}},
		&Auth{ID: "high-b", Provider: "provider-high-b", Attributes: map[string]string{"priority": "7"}},
	)

	providers := []string{"provider-low", "provider-high-a", "provider-high-b"}
	wantProviders := []string{"provider-high-a", "provider-high-b", "provider-high-a", "provider-high-b"}
	wantIDs := []string{"high-a", "high-b", "high-a", "high-b"}
	for index := range wantProviders {
		got, provider, errPick := scheduler.pickMixed(context.Background(), providers, model, cliproxyexecutor.Options{}, nil)
		if errPick != nil {
			t.Fatalf("pickMixed() #%d error = %v", index, errPick)
		}
		if got == nil {
			t.Fatalf("pickMixed() #%d auth = nil", index)
		}
		if provider != wantProviders[index] {
			t.Fatalf("pickMixed() #%d provider = %q, want %q", index, provider, wantProviders[index])
		}
		if got.ID != wantIDs[index] {
			t.Fatalf("pickMixed() #%d auth.ID = %q, want %q", index, got.ID, wantIDs[index])
		}
	}
}

func TestManager_PickNextMixed_UsesWeightedProviderRotationBeforeCredentialRotation(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.executors["gemini"] = schedulerTestExecutor{}
	manager.executors["claude"] = schedulerTestExecutor{}
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "gemini-a", Provider: "gemini"}); errRegister != nil {
		t.Fatalf("Register(gemini-a) error = %v", errRegister)
	}
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "gemini-b", Provider: "gemini"}); errRegister != nil {
		t.Fatalf("Register(gemini-b) error = %v", errRegister)
	}
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "claude-a", Provider: "claude"}); errRegister != nil {
		t.Fatalf("Register(claude-a) error = %v", errRegister)
	}

	wantProviders := []string{"gemini", "gemini", "claude", "gemini"}
	wantIDs := []string{"gemini-a", "gemini-b", "claude-a", "gemini-a"}
	for index := range wantProviders {
		got, _, provider, errPick := manager.pickNextMixed(context.Background(), []string{"gemini", "claude"}, "", cliproxyexecutor.Options{}, map[string]struct{}{})
		if errPick != nil {
			t.Fatalf("pickNextMixed() #%d error = %v", index, errPick)
		}
		if got == nil {
			t.Fatalf("pickNextMixed() #%d auth = nil", index)
		}
		if provider != wantProviders[index] {
			t.Fatalf("pickNextMixed() #%d provider = %q, want %q", index, provider, wantProviders[index])
		}
		if got.ID != wantIDs[index] {
			t.Fatalf("pickNextMixed() #%d auth.ID = %q, want %q", index, got.ID, wantIDs[index])
		}
	}
}

func TestManagerCustomSelector_FallsBackToLegacyPath(t *testing.T) {
	t.Parallel()

	selector := &trackingSelector{}
	manager := NewManager(nil, selector, nil)
	manager.executors["gemini"] = schedulerTestExecutor{}
	manager.auths["auth-a"] = &Auth{ID: "auth-a", Provider: "gemini"}
	manager.auths["auth-b"] = &Auth{ID: "auth-b", Provider: "gemini"}

	got, _, errPick := manager.pickNext(context.Background(), "gemini", "", cliproxyexecutor.Options{}, map[string]struct{}{})
	if errPick != nil {
		t.Fatalf("pickNext() error = %v", errPick)
	}
	if got == nil {
		t.Fatalf("pickNext() auth = nil")
	}
	if selector.calls != 1 {
		t.Fatalf("selector.calls = %d, want %d", selector.calls, 1)
	}
	if len(selector.lastAuthID) != 2 {
		t.Fatalf("len(selector.lastAuthID) = %d, want %d", len(selector.lastAuthID), 2)
	}
	if got.ID != selector.lastAuthID[len(selector.lastAuthID)-1] {
		t.Fatalf("pickNext() auth.ID = %q, want selector-picked %q", got.ID, selector.lastAuthID[len(selector.lastAuthID)-1])
	}
}

func TestManager_InitializesSchedulerForBuiltInSelector(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	if manager.scheduler == nil {
		t.Fatalf("manager.scheduler = nil")
	}
	if manager.scheduler.strategy != schedulerStrategyRoundRobin {
		t.Fatalf("manager.scheduler.strategy = %v, want %v", manager.scheduler.strategy, schedulerStrategyRoundRobin)
	}

	manager.SetSelector(&FillFirstSelector{})
	if manager.scheduler.strategy != schedulerStrategyFillFirst {
		t.Fatalf("manager.scheduler.strategy = %v, want %v", manager.scheduler.strategy, schedulerStrategyFillFirst)
	}
}

func TestManager_SchedulerTracksRegisterAndUpdate(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "auth-b", Provider: "gemini"}); errRegister != nil {
		t.Fatalf("Register(auth-b) error = %v", errRegister)
	}
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "auth-a", Provider: "gemini"}); errRegister != nil {
		t.Fatalf("Register(auth-a) error = %v", errRegister)
	}

	got, errPick := manager.scheduler.pickSingle(context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil)
	if errPick != nil {
		t.Fatalf("scheduler.pickSingle() error = %v", errPick)
	}
	if got == nil || got.ID != "auth-a" {
		t.Fatalf("scheduler.pickSingle() auth = %v, want auth-a", got)
	}

	if _, errUpdate := manager.Update(context.Background(), &Auth{ID: "auth-a", Provider: "gemini", Disabled: true}); errUpdate != nil {
		t.Fatalf("Update(auth-a) error = %v", errUpdate)
	}

	got, errPick = manager.scheduler.pickSingle(context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil)
	if errPick != nil {
		t.Fatalf("scheduler.pickSingle() after update error = %v", errPick)
	}
	if got == nil || got.ID != "auth-b" {
		t.Fatalf("scheduler.pickSingle() after update auth = %v, want auth-b", got)
	}
}

func TestManager_PickNextMixed_UsesSchedulerRotation(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.executors["gemini"] = schedulerTestExecutor{}
	manager.executors["claude"] = schedulerTestExecutor{}
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "gemini-a", Provider: "gemini"}); errRegister != nil {
		t.Fatalf("Register(gemini-a) error = %v", errRegister)
	}
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "gemini-b", Provider: "gemini"}); errRegister != nil {
		t.Fatalf("Register(gemini-b) error = %v", errRegister)
	}
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "claude-a", Provider: "claude"}); errRegister != nil {
		t.Fatalf("Register(claude-a) error = %v", errRegister)
	}

	wantProviders := []string{"gemini", "gemini", "claude", "gemini"}
	wantIDs := []string{"gemini-a", "gemini-b", "claude-a", "gemini-a"}
	for index := range wantProviders {
		got, _, provider, errPick := manager.pickNextMixed(context.Background(), []string{"gemini", "claude"}, "", cliproxyexecutor.Options{}, nil)
		if errPick != nil {
			t.Fatalf("pickNextMixed() #%d error = %v", index, errPick)
		}
		if got == nil {
			t.Fatalf("pickNextMixed() #%d auth = nil", index)
		}
		if provider != wantProviders[index] {
			t.Fatalf("pickNextMixed() #%d provider = %q, want %q", index, provider, wantProviders[index])
		}
		if got.ID != wantIDs[index] {
			t.Fatalf("pickNextMixed() #%d auth.ID = %q, want %q", index, got.ID, wantIDs[index])
		}
	}
}

func TestManager_PickNextMixed_SkipsProvidersWithoutExecutors(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.executors["claude"] = schedulerTestExecutor{}
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "gemini-a", Provider: "gemini"}); errRegister != nil {
		t.Fatalf("Register(gemini-a) error = %v", errRegister)
	}
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "claude-a", Provider: "claude"}); errRegister != nil {
		t.Fatalf("Register(claude-a) error = %v", errRegister)
	}

	got, _, provider, errPick := manager.pickNextMixed(context.Background(), []string{"gemini", "claude"}, "", cliproxyexecutor.Options{}, nil)
	if errPick != nil {
		t.Fatalf("pickNextMixed() error = %v", errPick)
	}
	if got == nil {
		t.Fatalf("pickNextMixed() auth = nil")
	}
	if provider != "claude" {
		t.Fatalf("pickNextMixed() provider = %q, want %q", provider, "claude")
	}
	if got.ID != "claude-a" {
		t.Fatalf("pickNextMixed() auth.ID = %q, want %q", got.ID, "claude-a")
	}
}

func TestManager_SchedulerTracksMarkResultCooldownAndRecovery(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient("auth-a", "gemini", []*registry.ModelInfo{{ID: "test-model"}})
	reg.RegisterClient("auth-b", "gemini", []*registry.ModelInfo{{ID: "test-model"}})
	t.Cleanup(func() {
		reg.UnregisterClient("auth-a")
		reg.UnregisterClient("auth-b")
	})
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "auth-a", Provider: "gemini"}); errRegister != nil {
		t.Fatalf("Register(auth-a) error = %v", errRegister)
	}
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "auth-b", Provider: "gemini"}); errRegister != nil {
		t.Fatalf("Register(auth-b) error = %v", errRegister)
	}

	manager.MarkResult(context.Background(), Result{
		AuthID:   "auth-a",
		Provider: "gemini",
		Model:    "test-model",
		Success:  false,
		Error:    &Error{HTTPStatus: 429, Message: "quota"},
	})

	got, errPick := manager.scheduler.pickSingle(context.Background(), "gemini", "test-model", cliproxyexecutor.Options{}, nil)
	if errPick != nil {
		t.Fatalf("scheduler.pickSingle() after cooldown error = %v", errPick)
	}
	if got == nil || got.ID != "auth-b" {
		t.Fatalf("scheduler.pickSingle() after cooldown auth = %v, want auth-b", got)
	}

	manager.MarkResult(context.Background(), Result{
		AuthID:   "auth-a",
		Provider: "gemini",
		Model:    "test-model",
		Success:  true,
	})

	seen := make(map[string]struct{}, 2)
	for index := 0; index < 2; index++ {
		got, errPick = manager.scheduler.pickSingle(context.Background(), "gemini", "test-model", cliproxyexecutor.Options{}, nil)
		if errPick != nil {
			t.Fatalf("scheduler.pickSingle() after recovery #%d error = %v", index, errPick)
		}
		if got == nil {
			t.Fatalf("scheduler.pickSingle() after recovery #%d auth = nil", index)
		}
		seen[got.ID] = struct{}{}
	}
	if len(seen) != 2 {
		t.Fatalf("len(seen) = %d, want %d", len(seen), 2)
	}
}

// TestManager_MarkResult_429_NoRetryAfter_TreatedAsTransient verifies that a 429
// without an upstream RetryAfter hint is classified as a transient rate-limit
// (model capacity / TPM burst) instead of a plan-level quota exhaustion: the
// model state's Quota.Exceeded must remain false, and NextRetryAfter must fall
// in a brief cooldown window around now+transientRateLimitCooldown.
func TestManager_MarkResult_429_NoRetryAfter_TreatedAsTransient(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient("transient-auth", "gemini", []*registry.ModelInfo{{ID: "transient-model"}})
	t.Cleanup(func() {
		reg.UnregisterClient("transient-auth")
	})
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "transient-auth", Provider: "gemini"}); errRegister != nil {
		t.Fatalf("Register(transient-auth) error = %v", errRegister)
	}

	before := time.Now()
	manager.MarkResult(context.Background(), Result{
		AuthID:   "transient-auth",
		Provider: "gemini",
		Model:    "transient-model",
		Success:  false,
		Error:    &Error{HTTPStatus: http.StatusTooManyRequests, Message: "rate_limit"},
	})
	after := time.Now()

	got, ok := manager.GetByID("transient-auth")
	if !ok || got == nil {
		t.Fatalf("GetByID(transient-auth) = (%v, %v), want auth", got, ok)
	}
	state, exists := got.ModelStates["transient-model"]
	if !exists || state == nil {
		t.Fatalf("ModelStates[transient-model] = (%v, %v), want state", state, exists)
	}
	if state.Quota.Exceeded {
		t.Fatalf("Quota.Exceeded = true, want false (transient 429 must not flip plan-level quota)")
	}
	expectedMin := before.Add(transientRateLimitCooldown).Add(-5 * time.Second)
	expectedMax := after.Add(transientRateLimitCooldown).Add(5 * time.Second)
	if state.NextRetryAfter.Before(expectedMin) || state.NextRetryAfter.After(expectedMax) {
		t.Fatalf("NextRetryAfter = %v, want within [%v, %v]", state.NextRetryAfter, expectedMin, expectedMax)
	}
}

// TestManager_AfterTransient429AllAuths_RecoversWithinShortWindow simulates the
// production bug we just patched: a single client request fans out across N
// auths under the same provider/model and every upstream call returns 429 with
// no RetryAfter (the canonical transient signature).
//
// Pre-fix: each auth would be classified as plan-quota, Quota.Exceeded would
// flip to true, and once all N were tagged, the selector raised
// *modelCooldownError, surfacing 429 model_cooldown to the user even though
// upstream had only exhibited a brief TPM/capacity burst.
//
// Post-fix: every state must keep Quota.Exceeded == false (no plan-quota
// false-positive), and the pool must be merely "auth_unavailable" for a short
// window (~transientRateLimitCooldown == 1 minute), recovering as soon as the
// per-state NextRetryAfter elapses.
func TestManager_AfterTransient429AllAuths_RecoversWithinShortWindow(t *testing.T) {
	t.Parallel()

	provider := "gemini"
	model := "transient-pool-model"
	authIDs := []string{"transient-pool-a", "transient-pool-b", "transient-pool-c"}

	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	registerSchedulerModels(t, provider, model, authIDs...)
	for _, id := range authIDs {
		if _, errRegister := manager.Register(context.Background(), &Auth{ID: id, Provider: provider}); errRegister != nil {
			t.Fatalf("Register(%s) error = %v", id, errRegister)
		}
	}

	// Fan-out 429 with nil RetryAfter on every auth (mimics conductor's
	// inner per-auth retry loop hitting transient rate-limits across the pool).
	for _, id := range authIDs {
		manager.MarkResult(context.Background(), Result{
			AuthID:   id,
			Provider: provider,
			Model:    model,
			Success:  false,
			Error:    &Error{HTTPStatus: http.StatusTooManyRequests, Message: "rate_limit"},
		})
	}

	// (a) plan-quota must NOT be flipped on any auth.
	for _, id := range authIDs {
		got, ok := manager.GetByID(id)
		if !ok || got == nil {
			t.Fatalf("GetByID(%s) = (%v, %v), want auth", id, got, ok)
		}
		state, exists := got.ModelStates[model]
		if !exists || state == nil {
			t.Fatalf("%s: ModelStates[%s] missing, want state", id, model)
		}
		if state.Quota.Exceeded {
			t.Fatalf("%s: Quota.Exceeded = true, want false (transient must not flip plan-quota)", id)
		}
		if state.Quota.Reason != "transient" {
			t.Fatalf("%s: Quota.Reason = %q, want %q", id, state.Quota.Reason, "transient")
		}
	}

	// (b) within the cooldown window, the entire pool is unavailable but NOT
	// classified as model_cooldown (because Quota.Exceeded is false on every
	// state, so the cooldownCount path in getAvailableAuths is not triggered).
	got, errPick := manager.scheduler.pickSingle(context.Background(), provider, model, cliproxyexecutor.Options{}, nil)
	if got != nil {
		t.Fatalf("pickSingle() during transient window auth = %v, want nil", got)
	}
	var cooldownErr *modelCooldownError
	if errors.As(errPick, &cooldownErr) {
		t.Fatalf("pickSingle() during transient window error = %T (model_cooldown), want non-cooldown auth_unavailable", errPick)
	}
	var authErr *Error
	if !errors.As(errPick, &authErr) {
		t.Fatalf("pickSingle() during transient window error = %v, want *Error", errPick)
	}
	if authErr.Code != "auth_unavailable" {
		t.Fatalf("pickSingle() during transient window error.Code = %q, want %q", authErr.Code, "auth_unavailable")
	}

	// (c) simulate the transient cooldown elapsing by rewinding each state's
	// NextRetryAfter into the past. We mutate the manager's internal auth
	// pointers (same package access) and re-publish the snapshots into the
	// scheduler so the scheduledAuth's cached nextRetryAt is also rewound;
	// this mirrors how the conductor itself propagates state updates without
	// sleeping in tests.
	pastTime := time.Now().Add(-1 * time.Second)
	manager.mu.Lock()
	for _, id := range authIDs {
		auth, ok := manager.auths[id]
		if !ok || auth == nil {
			manager.mu.Unlock()
			t.Fatalf("manager.auths[%s] missing, want present", id)
		}
		state := auth.ModelStates[model]
		state.NextRetryAfter = pastTime
		state.Quota.NextRecoverAt = pastTime
		auth.NextRetryAfter = pastTime
		auth.Quota.NextRecoverAt = pastTime
	}
	snapshots := make([]*Auth, 0, len(authIDs))
	for _, id := range authIDs {
		snapshots = append(snapshots, manager.auths[id].Clone())
	}
	manager.mu.Unlock()
	for _, snapshot := range snapshots {
		manager.scheduler.upsertAuth(snapshot)
	}

	// After the window, the pool recovers without manual intervention.
	got, errPick = manager.scheduler.pickSingle(context.Background(), provider, model, cliproxyexecutor.Options{}, nil)
	if errPick != nil {
		t.Fatalf("pickSingle() after transient window error = %v, want nil", errPick)
	}
	if got == nil {
		t.Fatalf("pickSingle() after transient window auth = nil, want one of %v", authIDs)
	}
	matched := false
	for _, id := range authIDs {
		if got.ID == id {
			matched = true
			break
		}
	}
	if !matched {
		t.Fatalf("pickSingle() after transient window auth.ID = %q, want one of %v", got.ID, authIDs)
	}
}
