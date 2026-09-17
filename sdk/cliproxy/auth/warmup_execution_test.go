package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type warmupFakeExecutor struct {
	schedulerTestExecutor
	execute func(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error)
	stream  func(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error)
	count   func(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error)
}

func (e *warmupFakeExecutor) Execute(ctx context.Context, a *Auth, r cliproxyexecutor.Request, o cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if e.execute != nil {
		return e.execute(ctx, a, r, o)
	}
	return cliproxyexecutor.Response{}, nil
}
func (e *warmupFakeExecutor) ExecuteStream(ctx context.Context, a *Auth, r cliproxyexecutor.Request, o cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return e.stream(ctx, a, r, o)
}
func (e *warmupFakeExecutor) CountTokens(ctx context.Context, a *Auth, r cliproxyexecutor.Request, o cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if e.count != nil {
		return e.count(ctx, a, r, o)
	}
	return cliproxyexecutor.Response{}, nil
}
func (e *warmupFakeExecutor) Refresh(_ context.Context, a *Auth) (*Auth, error) {
	updated := a.Clone()
	updated.Metadata["access_token"] = "synthetic-refreshed-token"
	return updated, nil
}

type warmupExecutionFixture struct {
	m      *Manager
	s      *AdaptiveSelector
	a      *Auth
	e      *warmupFakeExecutor
	store  *requestPrepareStore
	config *internalconfig.Config
	model  string
	ctx    context.Context
	state  *warmupRequestState
	now    *time.Time
}

func newWarmupExecutionFixture(t *testing.T, cold bool) warmupExecutionFixture {
	t.Helper()
	cfg := internalconfig.DefaultAccountSchedulingConfig()
	cfg.WarmupServingReserve = 0.5
	for i := range cfg.WarmupCurve {
		cfg.WarmupCurve[i].ConcurrencyLimit = 1
		cfg.WarmupCurve[i].DailyBudget = 2
		cfg.WarmupCurve[i].RPMLimit = 3
	}
	now := time.Now()
	var manager *Manager
	s := NewAdaptiveSelector(AdaptiveSelectorConfig{Scheduling: cfg, SessionAffinity: true, SessionTTL: time.Hour}, WithAdaptiveClock(func() time.Time { return now }), WithAdaptiveRand(func() float64 { return 0 }), WithAdaptiveSchedulingProvider(func() internalconfig.AccountSchedulingConfig {
		if manager == nil {
			return cfg
		}
		return manager.accountSchedulingConfig()
	}))
	t.Cleanup(s.Stop)
	store := &requestPrepareStore{}
	manager = NewManager(store, s, nil)
	config := &internalconfig.Config{AccountScheduling: cfg}
	config.ProxyURL = "http://proxy.invalid:8080"
	manager.SetConfig(config)
	e := &warmupFakeExecutor{schedulerTestExecutor: schedulerTestExecutor{provider: "claude"}}
	manager.executors["claude"] = e
	anchor := now.Add(-24 * time.Hour)
	if cold {
		anchor = time.Time{}
	}
	name := strings.ReplaceAll(t.Name(), "/", "-")
	a := newAdaptiveClaudeAuth("warmup-"+name, "default_claude_max_5x", anchor)
	a.Metadata["refresh_token"] = "synthetic-refresh-token"
	a.Metadata["access_token"] = "synthetic-access-token"
	if _, err := manager.Register(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	model := "claude-sonnet-warmup-" + name
	registry.GetGlobalRegistry().RegisterClient(a.ID, "claude", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(a.ID) })
	status := AccountWarmupStatusFor(a, time.Now(), cfg)
	if status.Mature || status.ConcurrencyLimit != 1 || status.RPMLimit != 3 || status.DailyBudget <= 0 {
		t.Fatalf("fixture is not gated warming: %+v", status)
	}
	ctx, state := virtualWarmupWait(&now)
	return warmupExecutionFixture{manager, s, a, e, store, config, model, ctx, state, &now}
}

func (f warmupExecutionFixture) request() (cliproxyexecutor.Request, cliproxyexecutor.Options) {
	opts := servingOptions("execution-root", "", "execution instructions", "Review cancellation behavior.")
	return cliproxyexecutor.Request{Model: f.model, Payload: opts.OriginalRequest}, opts
}
func (f warmupExecutionFixture) pending() int {
	f.s.gate.mu.Lock()
	defer f.s.gate.mu.Unlock()
	return f.s.gate.pending[f.a.ID]
}
func (f warmupExecutionFixture) snapshot() *Auth {
	f.m.mu.RLock()
	defer f.m.mu.RUnlock()
	return f.m.auths[f.a.ID].Clone()
}

func TestWarmupExecutionAtomicLastReservation(t *testing.T) {
	for _, name := range []string{"concurrency", "daily"} {
		t.Run(name, func(t *testing.T) {
			f := newWarmupExecutionFixture(t, false)
			if name == "daily" {
				for i := range f.config.AccountScheduling.WarmupCurve {
					f.config.AccountScheduling.WarmupCurve[i].ConcurrencyLimit = 2
					f.config.AccountScheduling.WarmupCurve[i].DailyBudget = 1
				}
				f.m.SetConfig(f.config)
			}
			var wg sync.WaitGroup
			slots := make(chan *accountExecutionSlot, 8)
			for i := 0; i < 8; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					ctx := withWarmupRequestState(context.Background(), warmupPurposeServe)
					slot, target, _ := f.m.tryWarmupExecution(ctx, f.a, f.model)
					if slot != nil {
						if !target {
							t.Error("legacy admission falsely passed")
						}
						slots <- slot
					}
				}()
			}
			wg.Wait()
			close(slots)
			var winner *accountExecutionSlot
			count := 0
			for slot := range slots {
				winner = slot
				count++
			}
			if count != 1 || f.s.gate.InFlight(f.a.ID) != 1 || f.pending() != 1 || f.s.gate.DailyCount(f.a.ID) != 0 {
				t.Fatalf("reservations=%d inflight=%d pending=%d", count, f.s.gate.InFlight(f.a.ID), f.pending())
			}
			ctx := withWarmupExecutionSlot(context.Background(), winner)
			if err := warmupBeforeSend(ctx, winner); err != nil {
				t.Fatal(err)
			}
			f.m.MarkResult(ctx, Result{AuthID: f.a.ID, Provider: "claude", Model: f.model, Success: true})
			if f.pending() != 1 || f.s.gate.DailyCount(f.a.ID) != 1 {
				t.Fatal("pending released before result accounting")
			}
			winner.close(ctx)
			winner.close(ctx)
			if f.pending() != 0 || f.s.gate.InFlight(f.a.ID) != 0 {
				t.Fatal("reservation leaked")
			}
		})
	}
}

func TestWarmupExecutionRealExecuteAndCount(t *testing.T) {
	for _, name := range []string{"execute", "count", "count_404", "default_off_count"} {
		t.Run(name, func(t *testing.T) {
			f := newWarmupExecutionFixture(t, true)
			req, opts := f.request()
			if name == "default_off_count" {
				f.config.AccountScheduling.WarmupServingReserve = 0
				f.m.SetConfig(f.config)
			}
			calls := 0
			call := func(ctx context.Context, _ *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
				calls++
				if name != "default_off_count" && (f.s.gate.InFlight(f.a.ID) != 1 || f.pending() != 1) {
					t.Error("send occurred without strict slot")
				}
				if name == "count_404" {
					return cliproxyexecutor.Response{}, &Error{Message: "404 page not found", HTTPStatus: http.StatusNotFound}
				}
				return cliproxyexecutor.Response{}, nil
			}
			f.e.execute, f.e.count = call, call
			var err error
			if name == "execute" {
				_, err = f.m.Execute(f.ctx, []string{"claude"}, req, opts)
			} else {
				ctx := context.WithValue(f.ctx, warmupRequestContextKey{}, &warmupRequestState{purpose: warmupPurposeCount, now: f.state.now, sleep: f.state.sleep, queue: &warmupWaitQueue{}})
				_, err = f.m.ExecuteCount(ctx, []string{"claude"}, req, opts)
			}
			if name != "count_404" && err != nil {
				t.Fatal(err)
			}
			if calls != 1 || f.pending() != 0 || f.s.gate.InFlight(f.a.ID) != 0 {
				t.Fatalf("calls=%d pending=%d", calls, f.pending())
			}
			latest := f.snapshot()
			_, anchored := AuthFirstProductionAt(latest)
			if anchored != (name == "execute" || name == "default_off_count") {
				t.Fatalf("unexpected anchor=%v", anchored)
			}
			if name == "count_404" {
				if latest.Failed != 1 || f.s.gate.DailyCount(f.a.ID) != 0 {
					t.Fatal("count endpoint-404 accounting changed")
				}
			} else if latest.Success != 1 || f.s.gate.DailyCount(f.a.ID) != 1 {
				t.Fatal("result accounting changed")
			}
		})
	}
}

func TestWarmupExecutionSilentStreamCancel(t *testing.T) {
	f := newWarmupExecutionFixture(t, true)
	req, opts := f.request()
	ctx, cancel := context.WithCancel(f.ctx)
	upstream := make(chan cliproxyexecutor.StreamChunk, 1)
	upstream <- cliproxyexecutor.StreamChunk{Payload: []byte("data: ready\n\n")}
	f.e.stream = func(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
		if f.s.gate.InFlight(f.a.ID) != 1 || f.pending() != 1 {
			t.Error("stream started without slot")
		}
		return &cliproxyexecutor.StreamResult{Chunks: upstream}, nil
	}
	result, err := f.m.ExecuteStream(ctx, []string{"claude"}, req, opts)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-result.Chunks:
	case <-time.After(time.Second):
		t.Fatal("bootstrap missing")
	}
	before := f.store.saveCount.Load()
	cancel()
	select {
	case _, open := <-result.Chunks:
		if open {
			t.Fatal("cancel forwarded unexpected chunk")
		}
	case <-time.After(time.Second):
		t.Fatal("silent source retained slot after cancellation")
	}
	if f.pending() != 0 || f.s.gate.InFlight(f.a.ID) != 0 || f.s.gate.DailyCount(f.a.ID) != 1 {
		t.Fatal("cancel did not close/account exactly once")
	}
	latest := f.snapshot()
	if latest.Success != 0 || latest.Failed != 0 {
		t.Fatal("cancel affected outcome counters")
	}
	if _, ok := AuthFirstProductionAt(latest); ok {
		t.Fatal("cancel minted anchor")
	}
	if f.store.saveCount.Load() != before+1 {
		t.Fatal("cancel budget was not persisted")
	}
	if len(readDailyWindowBuckets(f.store.lastAuth().Metadata, accountSchedulingDailyWindowKey)) == 0 {
		t.Fatal("persisted cancel lacks budget window")
	}
	*f.now = f.now.Add(20 * time.Second)
	slot, target, err := f.m.tryWarmupExecution(f.ctx, f.a, f.model)
	if err != nil || !target || slot == nil {
		t.Fatalf("slot not reusable: %v", err)
	}
	slot.close(f.ctx)
}

func TestWarmupExecutionNoSendCancelAndResultRace(t *testing.T) {
	f := newWarmupExecutionFixture(t, true)
	slot, target, err := f.m.tryWarmupExecution(f.ctx, f.a, f.model)
	if err != nil || !target {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(withWarmupExecutionSlot(f.ctx, slot))
	cancel()
	slot.close(ctx)
	if f.s.gate.DailyCount(f.a.ID) != 0 || f.pending() != 0 {
		t.Fatal("unsent cancel counted")
	}
	*f.now = f.now.Add(20 * time.Second)
	slot, target, err = f.m.tryWarmupExecution(f.ctx, f.a, f.model)
	if err != nil || !target {
		t.Fatal(err)
	}
	ctx, cancel = context.WithCancel(withWarmupExecutionSlot(f.ctx, slot))
	if err := warmupBeforeSend(ctx, slot); err != nil {
		t.Fatal(err)
	}
	cancel()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f.m.MarkResult(ctx, Result{AuthID: f.a.ID, Provider: "claude", Model: f.model, Success: false, Error: &Error{Message: "cancelled", HTTPStatus: 500}})
			slot.close(ctx)
		}()
	}
	wg.Wait()
	if f.s.gate.DailyCount(f.a.ID) != 1 || f.pending() != 0 || f.snapshot().Failed != 0 {
		t.Fatal("cancel/result race double counted or affected health")
	}
}

func TestWarmupExecutionReloadWithoutPickInvalidatesEpoch(t *testing.T) {
	f := newWarmupExecutionFixture(t, false)
	_, opts := f.request()
	if _, err := f.s.Pick(f.ctx, "claude", f.model, opts, []*Auth{f.a}); err != nil {
		t.Fatal(err)
	}
	epoch := f.s.servingEpoch
	disabled := *f.config
	disabled.AccountScheduling.WarmupServingReserve = 0
	f.m.SetConfig(&disabled)
	f.m.SetConfig(f.config)
	if f.s.servingEpoch <= epoch || f.s.servingActive.Load() {
		t.Fatal("reload failed to clear without a Pick")
	}
	for _, entry := range f.s.cache.entries {
		if entry.serving != nil && entry.serving.protected {
			t.Fatal("reload revived protection")
		}
	}
}

func TestWarmupExecutionBusyNeverOuterRetries(t *testing.T) {
	f := newWarmupExecutionFixture(t, false)
	if _, retry := f.m.shouldRetryAfterError(newWarmupBusyError(), 0, []string{"claude"}, f.model, time.Minute); retry {
		t.Fatal("outer loop would extend the 30s budget")
	}
}

func TestWarmupExecutionRefreshAndEmptyStreams(t *testing.T) {
	for _, name := range []string{"execute_401", "count_401", "stream_401", "bootstrap_401", "bootstrap_retry_nil", "initial_nil", "initial_empty", "mid_error_silent"} {
		t.Run(name, func(t *testing.T) {
			f := newWarmupExecutionFixture(t, true)
			req, opts := f.request()
			calls := 0
			check := func() {
				calls++
				if f.s.gate.InFlight(f.a.ID) != 1 || f.pending() != 1 {
					t.Error("retry sent without strict reservation")
				}
			}
			unauthorized := &Error{Message: "unauthorized", HTTPStatus: http.StatusUnauthorized}
			response := func(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
				check()
				if calls == 1 {
					return cliproxyexecutor.Response{}, unauthorized
				}
				return cliproxyexecutor.Response{}, nil
			}
			f.e.execute, f.e.count = response, response
			f.e.stream = func(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
				check()
				if name == "stream_401" && calls == 1 {
					return nil, unauthorized
				}
				if name == "initial_nil" || (name == "bootstrap_retry_nil" && calls == 2) {
					return nil, nil
				}
				ch := make(chan cliproxyexecutor.StreamChunk, 2)
				if (name == "bootstrap_401" || name == "bootstrap_retry_nil") && calls == 1 {
					ch <- cliproxyexecutor.StreamChunk{Err: unauthorized}
				} else if name != "initial_empty" {
					ch <- cliproxyexecutor.StreamChunk{Payload: []byte("data: ready\n\n")}
				}
				if name == "mid_error_silent" {
					ch <- cliproxyexecutor.StreamChunk{Err: &Error{Message: "upstream failure", HTTPStatus: 503}}
				} else {
					close(ch)
				}
				return &cliproxyexecutor.StreamResult{Chunks: ch}, nil
			}
			failed := false
			switch name {
			case "execute_401":
				_, err := f.m.Execute(f.ctx, []string{"claude"}, req, opts)
				failed = err != nil
			case "count_401":
				state := &warmupRequestState{purpose: warmupPurposeCount, now: f.state.now, sleep: f.state.sleep, queue: &warmupWaitQueue{}}
				ctx := context.WithValue(f.ctx, warmupRequestContextKey{}, state)
				_, err := f.m.ExecuteCount(ctx, []string{"claude"}, req, opts)
				failed = err != nil
			default:
				result, err := f.m.ExecuteStream(f.ctx, []string{"claude"}, req, opts)
				failed = err != nil
				if result != nil {
					draining := true
					for draining {
						select {
						case chunk, ok := <-result.Chunks:
							if !ok {
								draining = false
							} else if chunk.Err != nil {
								failed = true
							}
						case <-time.After(time.Second):
							t.Fatal("error/empty stream failed to terminate")
						}
					}
				}
			}
			wantFailure := name == "bootstrap_retry_nil" || name == "initial_nil" || name == "initial_empty" || name == "mid_error_silent"
			wantCalls := 2
			if name == "initial_nil" || name == "initial_empty" || name == "mid_error_silent" {
				wantCalls = 1
			}
			if failed != wantFailure || calls != wantCalls || f.s.gate.DailyCount(f.a.ID) != 1 || f.pending() != 0 || f.s.gate.InFlight(f.a.ID) != 0 {
				t.Fatalf("failed=%v calls=%d daily=%d pending=%d", failed, calls, f.s.gate.DailyCount(f.a.ID), f.pending())
			}
		})
	}
}

func TestWarmupExecutionPoolReacquiresSlot(t *testing.T) {
	f := newWarmupExecutionFixture(t, false)
	req, opts := f.request()
	calls := 0
	var previousAttempt context.Context
	f.e.stream = func(current context.Context, _ *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
		calls++
		if previousAttempt != nil && previousAttempt.Err() == nil {
			t.Error("new model sent before previous attempt cancellation")
		}
		previousAttempt = current
		if f.s.gate.InFlight(f.a.ID) != 1 || f.pending() != 1 {
			t.Error("pool send without reservation")
		}
		if calls == 1 {
			*f.now = f.now.Add(20 * time.Second)
			return nil, &Error{Message: "temporary overload", HTTPStatus: 503}
		}
		if f.s.gate.DailyCount(f.a.ID) != 1 {
			t.Error("pool retry outran previous result accounting")
		}
		ch := make(chan cliproxyexecutor.StreamChunk, 1)
		ch <- cliproxyexecutor.StreamChunk{Payload: []byte("data: ready\n\n")}
		close(ch)
		return &cliproxyexecutor.StreamResult{Chunks: ch}, nil
	}
	result, err := f.m.executeStreamWithModelPool(f.ctx, f.e, f.a, "claude", req, opts, f.model, "", []string{"pool-first", "pool-second"}, true, OAuthModelAliasResult{}, true, false)
	if err != nil {
		t.Fatal(err)
	}
	for range result.Chunks {
	}
	if calls != 2 || f.s.gate.DailyCount(f.a.ID) != 2 || f.pending() != 0 || f.s.gate.InFlight(f.a.ID) != 0 {
		t.Fatalf("pool calls=%d daily=%d pending=%d", calls, f.s.gate.DailyCount(f.a.ID), f.pending())
	}
}

func TestWarmupExecutionLocalBusyDoesNotSpendCredentialRetry(t *testing.T) {
	f := newWarmupExecutionFixture(t, true)
	selectedAt := *f.now
	req, opts := f.request()
	mature := newAdaptiveClaudeAuth(f.a.ID+"-mature", "default_claude_max_20x", time.Now().Add(-365*24*time.Hour))
	if _, err := f.m.Register(context.Background(), mature); err != nil {
		t.Fatal(err)
	}
	registry.GetGlobalRegistry().RegisterClient(mature.ID, "claude", []*registry.ModelInfo{{ID: f.model}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(mature.ID) })
	var injected atomic.Bool
	f.s.limiter.now = func() time.Time {
		if injected.CompareAndSwap(false, true) {
			f.s.gate.Acquire(f.a.ID, 1)
		}
		return *f.now
	}
	defer f.s.gate.Release(f.a.ID)
	calls := 0
	sentTo := ""
	f.e.execute = func(_ context.Context, a *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
		calls++
		sentTo = a.ID
		return cliproxyexecutor.Response{}, nil
	}
	_, err := f.m.executeMixedOnce(f.ctx, []string{"claude"}, req, opts, 1)
	if err != nil || calls != 1 || sentTo != mature.ID || f.state.waited != 100*time.Millisecond || f.s.gate.DailyCount(f.a.ID) != 0 {
		t.Fatalf("busy spent retry or sent: err=%v calls=%d sent=%s waited=%v", err, calls, sentTo, f.state.waited)
	}
	entry, ok := f.s.servingEntry("mixed::claude:execution-root::"+f.model, *f.now)
	if !ok || entry.authID != f.a.ID || !entry.serving.protected || !entry.serving.lastSeen.Equal(selectedAt) {
		t.Fatal("admission overlap stole the protected binding")
	}
}

func TestWarmupExecutionMatureAndHomeStayLegacy(t *testing.T) {
	for _, name := range []string{"mature", "home_ephemeral"} {
		t.Run(name, func(t *testing.T) {
			f := newWarmupExecutionFixture(t, false)
			req, opts := f.request()
			if name == "mature" {
				mature := newAdaptiveClaudeAuth(f.a.ID, "default_claude_max_20x", time.Now().Add(-365*24*time.Hour))
				f.m.mu.Lock()
				f.m.auths[f.a.ID] = mature
				f.m.mu.Unlock()
				f.a = mature
			}
			f.e.stream = func(ctx context.Context, _ *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
				if slot := warmupExecutionSlotFromContext(ctx); slot != nil && slot.target {
					t.Error("legacy/Home used strict slot")
				}
				if f.pending() != 0 {
					t.Error("legacy/Home reserved pending budget")
				}
				ch := make(chan cliproxyexecutor.StreamChunk, 1)
				ch <- cliproxyexecutor.StreamChunk{Payload: []byte("data: ready\n\n")}
				close(ch)
				return &cliproxyexecutor.StreamResult{Chunks: ch}, nil
			}
			var result *cliproxyexecutor.StreamResult
			var err error
			if name == "mature" {
				result, err = f.m.ExecuteStream(f.ctx, []string{"claude"}, req, opts)
			} else {
				result, err = f.m.executeStreamWithModelPool(f.ctx, f.e, f.a, "claude", req, opts, f.model, "", []string{f.model}, false, OAuthModelAliasResult{}, false, true)
			}
			if err != nil {
				t.Fatal(err)
			}
			for range result.Chunks {
			}
			if f.pending() != 0 || f.s.gate.DailyCount(f.a.ID) != 0 {
				t.Fatal("legacy/Home budget changed")
			}
		})
	}
}

func TestWarmupExecutionCancelledNonstreamIsNeutral(t *testing.T) {
	f := newWarmupExecutionFixture(t, true)
	req, opts := f.request()
	ctx, cancel := context.WithCancel(f.ctx)
	f.e.execute = func(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
		cancel()
		return cliproxyexecutor.Response{}, context.Canceled
	}
	_, err := f.m.Execute(ctx, []string{"claude"}, req, opts)
	if !errors.Is(err, context.Canceled) || f.s.gate.DailyCount(f.a.ID) != 1 || f.pending() != 0 || f.snapshot().Failed != 0 {
		t.Fatalf("nonstream cancel=%v", err)
	}
}

func TestWarmupExecutionAdmissionRechecksRegistryAndContextPlan(t *testing.T) {
	for _, kind := range []string{"registry", "context_1m"} {
		t.Run(kind, func(t *testing.T) {
			f := newWarmupExecutionFixture(t, false)
			_, opts := f.request()
			model := f.model
			if kind == "context_1m" {
				model = "claude-opus-4-8"
				registry.GetGlobalRegistry().RegisterClient(f.a.ID, "claude", []*registry.ModelInfo{{ID: model}})
				opts.Headers = http.Header{"Anthropic-Beta": []string{"context-1m-2025-08-07"}}
				f.m.mu.Lock()
				if f.m.auths[f.a.ID].Attributes == nil {
					f.m.auths[f.a.ID].Attributes = make(map[string]string)
				}
				f.m.auths[f.a.ID].Metadata["plan_type"] = "max"
				f.m.mu.Unlock()
				if !f.m.authAllowsClaudeContextRequest(f.snapshot(), model, opts) {
					t.Fatal("fixture did not start with 1M permission")
				}
			}
			f.s.gate.Acquire(f.a.ID, 1)
			defer f.s.gate.Release(f.a.ID)
			f.state.sleep = func(context.Context, time.Duration) error {
				*f.now = f.now.Add(100 * time.Millisecond)
				if kind == "registry" {
					registry.GetGlobalRegistry().UnregisterClient(f.a.ID)
				} else {
					f.m.mu.Lock()
					f.m.auths[f.a.ID].Metadata["plan_type"] = "pro"
					f.m.mu.Unlock()
					if f.m.authAllowsClaudeContextRequest(f.snapshot(), model, opts) {
						t.Fatal("fixture did not revoke 1M permission")
					}
				}
				return nil
			}
			slot, target, err := f.m.admitWarmupExecution(f.ctx, f.a, model, opts)
			if slot != nil || !target || !isWarmupAdmissionReselect(err) || f.pending() != 0 || f.s.gate.DailyCount(f.a.ID) != 0 {
				t.Fatalf("stale route admitted: slot=%v target=%v err=%v", slot, target, err)
			}
		})
	}
}

type warmupCancelDuringSaveStore struct {
	requestPrepareStore
	cancel    context.CancelFunc
	gate      *AccountConcurrencyGate
	authID    string
	protected bool
}

func (s *warmupCancelDuringSaveStore) Save(ctx context.Context, a *Auth) (string, error) {
	s.cancel()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	s.gate.mu.Lock()
	s.protected = s.gate.pending[s.authID] == 1
	s.gate.mu.Unlock()
	return s.requestPrepareStore.Save(ctx, a)
}

func TestWarmupExecutionResultWinnerPersistsAfterCancel(t *testing.T) {
	f := newWarmupExecutionFixture(t, true)
	req, opts := f.request()
	ctx, cancel := context.WithCancel(f.ctx)
	store := &warmupCancelDuringSaveStore{cancel: cancel, gate: f.s.gate, authID: f.a.ID}
	f.m.SetStore(store)
	_, err := f.m.Execute(ctx, []string{"claude"}, req, opts)
	if err != nil || store.saveCount.Load() != 1 || !store.protected || f.s.gate.DailyCount(f.a.ID) != 1 || f.pending() != 0 {
		t.Fatalf("result persistence/cancel race err=%v saves=%d protected=%v", err, store.saveCount.Load(), store.protected)
	}
	if stored := store.lastAuth(); stored == nil || stored.Success != 1 || len(readDailyWindowBuckets(stored.Metadata, accountSchedulingDailyWindowKey)) == 0 {
		t.Fatal("result winner was not persisted")
	}
}

func TestWarmupExecutionReadyAdmissionAfterWaitBudget(t *testing.T) {
	f := newWarmupExecutionFixture(t, false)
	f.state.waited = warmupWaitLimit
	f.state.matureOnly = true
	slot, target, err := f.m.tryWarmupExecution(f.ctx, f.a, f.model)
	if err != nil || !target || slot == nil || f.state.waited != warmupWaitLimit {
		t.Fatalf("ready replacement incorrectly rejected: %v", err)
	}
	slot.close(f.ctx)
}

func TestWarmupExecutionWaitRefreshesActualCredentialAndProxy(t *testing.T) {
	f := newWarmupExecutionFixture(t, true)
	req, opts := f.request()
	var injected, held atomic.Bool
	f.s.limiter.now = func() time.Time {
		if injected.CompareAndSwap(false, true) {
			f.s.gate.Acquire(f.a.ID, 1)
			held.Store(true)
		}
		return *f.now
	}
	defer func() {
		if held.CompareAndSwap(true, false) {
			f.s.gate.Release(f.a.ID)
		}
	}()
	waits := 0
	f.state.sleep = func(context.Context, time.Duration) error {
		waits++
		*f.now = f.now.Add(100 * time.Millisecond)
		f.m.mu.Lock()
		f.m.auths[f.a.ID].Metadata["access_token"] = "rotated-before-send"
		f.m.auths[f.a.ID].ProxyURL = "http://updated.proxy.invalid:8080"
		f.m.mu.Unlock()
		if held.CompareAndSwap(true, false) {
			f.s.gate.Release(f.a.ID)
		}
		return nil
	}
	calls := 0
	f.e.execute = func(_ context.Context, a *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
		calls++
		if a.Metadata["access_token"] != "rotated-before-send" || a.ProxyURL != "http://updated.proxy.invalid:8080" {
			t.Error("executor received pre-wait auth snapshot")
		}
		return cliproxyexecutor.Response{}, nil
	}
	_, err := f.m.Execute(f.ctx, []string{"claude"}, req, opts)
	if err != nil || calls != 1 || waits != 1 || f.state.waited != 100*time.Millisecond {
		t.Fatalf("refresh after wait err=%v calls=%d waits=%d waited=%v", err, calls, waits, f.state.waited)
	}
}

func TestWarmupExecutionModelResumeIsOneShot(t *testing.T) {
	ctx := withWarmupRequestState(context.Background(), warmupPurposeServe)
	warmupSetModelResume(ctx, "same", "second")
	if !warmupCanResumeCredential(ctx, map[string]struct{}{"same": {}}) {
		t.Fatal("same credential continuation lost")
	}
	models, err := warmupResumeModels(ctx, "same", []string{"first", "second"})
	if err != nil || len(models) != 1 || models[0] != "second" {
		t.Fatal("wrong continuation model")
	}
	models, err = warmupResumeModels(ctx, "same", []string{"first", "second"})
	if err != nil || len(models) != 2 {
		t.Fatal("continuation was reused")
	}
	warmupSetModelResume(ctx, "same", "removed")
	_, err = warmupResumeModels(ctx, "same", []string{"current"})
	var busy *warmupBusyError
	if !errors.As(err, &busy) {
		t.Fatal("removed model was silently replayed")
	}
	warmupSetModelResume(ctx, "old", "old-model")
	models, err = warmupResumeModels(ctx, "new", []string{"new-model"})
	if err != nil || models[0] != "new-model" {
		t.Fatal("new credential inherited stale model")
	}
}

// This is an SDK construction, not a production Claude subscription shape.
// Standard Claude accounts have no multi-model compatibility pool.
func TestWarmupExecutionSDKCompatPoolContinuation(t *testing.T) {
	f := newWarmupExecutionFixture(t, false)
	req, opts := f.request()
	f.config.OpenAICompatibility = []internalconfig.OpenAICompatibility{{Name: "warmup-pool", Models: []internalconfig.OpenAICompatibilityModel{{Name: "up-a", Alias: f.model}, {Name: "up-b", Alias: f.model}}}}
	f.m.SetConfig(f.config)
	f.a.Attributes = map[string]string{"api_key": "synthetic-api-key", "compat_name": "warmup-pool", "provider_key": "warmup-pool"}
	provider := executorKeyFromAuth(f.a)
	f.m.executors[provider] = f.e
	if _, err := f.m.Register(context.Background(), f.a); err != nil {
		t.Fatal(err)
	}
	registry.GetGlobalRegistry().RegisterClient(f.a.ID, provider, []*registry.ModelInfo{{ID: f.model}})
	calls := 0
	firstModel := ""
	f.e.stream = func(_ context.Context, _ *Auth, r cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
		calls++
		if f.pending() != 1 || f.s.gate.InFlight(f.a.ID) != 1 {
			t.Error("SDK pool bypassed strict admission")
		}
		if calls == 1 {
			firstModel = r.Model
			return nil, &Error{Message: "temporary overload", HTTPStatus: 503}
		}
		if r.Model == firstModel {
			t.Error("continuation replayed completed model")
		}
		ch := make(chan cliproxyexecutor.StreamChunk, 1)
		ch <- cliproxyexecutor.StreamChunk{Payload: []byte("data: ready\n\n")}
		close(ch)
		return &cliproxyexecutor.StreamResult{Chunks: ch}, nil
	}
	result, err := f.m.executeStreamMixedOnce(f.ctx, []string{provider}, req, opts, 1)
	if err != nil {
		t.Fatal(err)
	}
	for range result.Chunks {
	}
	if calls != 2 || f.s.gate.DailyCount(f.a.ID) != 2 || f.pending() != 0 || f.state.waited != 20*time.Second {
		t.Fatalf("SDK continuation calls=%d daily=%d waited=%v", calls, f.s.gate.DailyCount(f.a.ID), f.state.waited)
	}
}

func TestWarmupExecutionCountPreservesProtectedRootAndChild(t *testing.T) {
	f := newWarmupExecutionFixture(t, true)
	req, opts := f.request()
	key := "mixed::claude:execution-root::" + f.model
	state := warmupServingSession{protected: true, reserved: true, source: "reserve", summary: summarizeWarmupRequest(opts.OriginalRequest)}
	f.s.servingMu.Lock()
	f.s.setServingEntry(key, f.a.ID, state, *f.now)
	state.child = true
	state.source = "inherited"
	f.s.setServingEntry(warmupChildKey(key, "existing-child"), f.a.ID, state, *f.now)
	f.s.servingMu.Unlock()
	f.s.servingActive.Store(true)
	epoch := f.s.servingEpoch
	before := make(map[string]sessionEntry)
	for key, entry := range f.s.cache.entries {
		before[key] = entry
	}
	count := &warmupRequestState{purpose: warmupPurposeCount, now: f.state.now, sleep: f.state.sleep, queue: &warmupWaitQueue{}}
	ctx := context.WithValue(f.ctx, warmupRequestContextKey{}, count)
	if _, err := f.m.ExecuteCount(ctx, []string{"claude"}, req, opts); err != nil {
		t.Fatal(err)
	}
	if f.s.servingEpoch != epoch || len(f.s.cache.entries) != len(before) {
		t.Fatal("Count cleared protected entries")
	}
	for key, entry := range before {
		if f.s.cache.entries[key] != entry {
			t.Fatal("Count changed a protected binding")
		}
	}
	if _, anchored := AuthFirstProductionAt(f.snapshot()); anchored {
		t.Fatal("Count minted first production")
	}
}
