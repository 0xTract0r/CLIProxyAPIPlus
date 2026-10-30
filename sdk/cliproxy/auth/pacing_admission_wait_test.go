package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestPacingAdmissionProtectedBindingsRespectOlderWaiter(t *testing.T) {
	cfg := &internalconfig.Config{AuthDir: t.TempDir(), AccountScheduling: internalconfig.DefaultAccountSchedulingConfig()}
	cfg.ProxyURL = "http://proxy.invalid:8080"
	cfg.AccountScheduling.WarmupTrafficPacing.Enabled = true
	cfg.AccountScheduling.WarmupTrafficPacing.MaxActiveBindings = 2
	for i := range cfg.AccountScheduling.WarmupCurve {
		cfg.AccountScheduling.WarmupCurve[i].DailyBudget = 500
		cfg.AccountScheduling.WarmupCurve[i].RPMLimit = 5
		cfg.AccountScheduling.WarmupCurve[i].ConcurrencyLimit = 1
	}
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	now := func() time.Time { return time.Unix(0, clock.Load()) }
	var m *Manager
	s := NewAdaptiveSelector(AdaptiveSelectorConfig{Scheduling: cfg.AccountScheduling, SessionAffinity: true}, WithAdaptiveClock(now), WithAdaptiveSchedulingProvider(func() internalconfig.AccountSchedulingConfig { return m.accountSchedulingConfig() }))
	t.Cleanup(s.Stop)
	m = NewManager(nil, s, nil)
	m.SetConfig(cfg)
	t.Cleanup(func() { RegisterAccountPacingUsageSink(nil) })
	a := newAdaptiveClaudeAuth("protected-fairness", "default_claude_max_5x", now().Add(-24*time.Hour))
	if _, err := m.Register(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	model := "claude-protected-fairness"
	registry.GetGlobalRegistry().RegisterClient(a.ID, "claude", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(a.ID) })
	type actorKey struct{}
	entered := make(chan string, 8)
	e := &pacingAwareExecutor{warmupFakeExecutor: warmupFakeExecutor{schedulerTestExecutor: schedulerTestExecutor{provider: "claude"}}}
	e.before = func(ctx context.Context, _ *Auth) {
		if actor, _ := ctx.Value(actorKey{}).(string); actor != "" {
			entered <- actor
		}
	}
	m.RegisterExecutor(e)
	parent := servingOptions("fair-parent", "", "parent system", "Continue the parent task.")
	child := servingOptions("fair-child", "", "child system", "Continue the independent child task.")
	invoke := func(ctx context.Context, opts cliproxyexecutor.Options) error {
		_, err := m.Execute(ctx, []string{"claude"}, cliproxyexecutor.Request{Model: model, Payload: opts.OriginalRequest}, opts)
		return err
	}
	clock.Add(int64(time.Hour))
	if err := invoke(context.Background(), parent); err != nil {
		t.Fatal(err)
	}
	clock.Add(int64(12 * time.Second))
	if err := invoke(context.Background(), child); err != nil {
		t.Fatal(err)
	}
	if got := m.WarmupTrafficPacingSnapshot(a.ID); *got.ActiveBindingGroups != 2 || *got.RPMLimit != 5 || *got.ConcurrencyLimit != 1 {
		t.Fatal("fixture must have two committed groups at original limits", got)
	}
	clock.Add(int64(time.Second))
	var wg sync.WaitGroup
	var cancels []context.CancelFunc
	t.Cleanup(func() {
		for _, cancel := range cancels {
			cancel()
		}
		wg.Wait()
	})
	start := func(actor string, opts cliproxyexecutor.Options) (<-chan struct{}, chan struct{}, <-chan error) {
		base, cancel := context.WithCancel(context.Background())
		cancels = append(cancels, cancel)
		ctx := withWarmupRequestState(context.WithValue(base, actorKey{}, actor), warmupPurposeServe)
		queued := make(chan struct{})
		resume := make(chan struct{})
		var queuedOnce sync.Once
		warmupRequestFromContext(ctx).admissionSleep = func(ctx context.Context, _ <-chan struct{}, _ time.Duration) error {
			queuedOnce.Do(func() { close(queued) })
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-resume:
				return nil
			}
		}
		done := make(chan error, 1)
		wg.Go(func() { done <- invoke(ctx, opts) })
		return queued, resume, done
	}
	parentQueued, resumeParent, parentDone := start("parent", parent)
	select {
	case <-parentQueued:
	case <-time.After(2 * time.Second):
		t.Fatal("parent did not queue")
	}
	// The parent is older and already queued when the next 5-RPM token arrives.
	clock.Add(int64(11 * time.Second))
	childQueued, resumeChild, childDone := start("child", child)
	select {
	case <-childQueued:
	case actor := <-entered:
		t.Fatalf("later %s bypassed the older protected parent and took its token", actor)
	case <-time.After(2 * time.Second):
		t.Fatal("child neither queued nor executed")
	}
	close(resumeParent)
	if err := <-parentDone; err != nil {
		t.Fatal("older parent failed", err)
	}
	if actor := <-entered; actor != "parent" {
		t.Fatal("wrong first continuation", actor)
	}
	clock.Add(int64(12 * time.Second))
	close(resumeChild)
	if err := <-childDone; err != nil {
		t.Fatal("child failed after next refill", err)
	}
	if actor := <-entered; actor != "child" {
		t.Fatal("wrong second continuation", actor)
	}
	if e.sends.Load() != 4 || s.gate.InFlight(a.ID) != 0 {
		t.Fatal("unexpected sends or leaked slots", e.sends.Load())
	}
}

func TestPacingAdmissionRPMRollbackDoesNotWakeWaiters(t *testing.T) {
	m, s, a, cfg, now := pacingManagerFixture(t)
	b := newAdaptiveClaudeAuth("rpm-rollback-second", "default_claude_max_5x", now.Add(-24*time.Hour))
	if _, err := m.Register(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	var clock atomic.Int64
	clock.Store(now.UnixNano())
	s.limiter.now = func() time.Time { return time.Unix(0, clock.Load()) }
	for _, account := range []*Auth{a, b} {
		rpm, burst := s.rateLimitParams(account, cfg.AccountScheduling, *now)
		if !s.limiter.Allow(account.ID, rpm, burst) {
			t.Fatal("could not consume initial token")
		}
	}
	var wg sync.WaitGroup
	var cancels []context.CancelFunc
	t.Cleanup(func() {
		for _, cancel := range cancels {
			cancel()
		}
		wg.Wait()
	})
	start := func(account *Auth, attempts *atomic.Int32) <-chan error {
		base, cancel := context.WithCancel(context.Background())
		cancels = append(cancels, cancel)
		ctx := withWarmupRequestState(base, warmupPurposeCount)
		configurePacingAdmission(ctx, cfg.AccountScheduling.WarmupTrafficPacing, 10)
		done := make(chan error, 1)
		wg.Go(func() {
			defer releasePacingAdmission(ctx)
			err := runWarmupSelection(ctx, func(current context.Context) error {
				attempts.Add(1)
				slot, _, err := m.tryWarmupExecution(current, account, "")
				if slot != nil {
					slot.release()
				}
				return err
			})
			done <- err
		})
		deadline := time.Now().Add(time.Second)
		for sharedPacingAdmissionQueue.waiting(s, account.ID) == 0 {
			if time.Now().After(deadline) {
				t.Fatal("request did not enter the real timer wait")
			}
			time.Sleep(time.Millisecond)
		}
		return done
	}
	var attemptsA, attemptsB atomic.Int32
	doneA := start(a, &attemptsA)
	doneB := start(b, &attemptsB)
	// Neither a token nor a real execution resource becomes available here.
	// A speculative slot rollback must not wake the other account's timer.
	time.Sleep(1100 * time.Millisecond)
	if attemptsA.Load() != 1 || attemptsB.Load() != 1 {
		t.Fatalf("RPM rollback caused eventless retries above 1 Hz: %d/%d", attemptsA.Load(), attemptsB.Load())
	}
	if s.gate.InFlight(a.ID) != 0 || s.gate.InFlight(b.ID) != 0 {
		t.Fatal("rejected RPM admission leaked an execution slot")
	}
	for _, cancel := range cancels {
		cancel()
	}
	if !errors.Is(<-doneA, context.Canceled) || !errors.Is(<-doneB, context.Canceled) {
		t.Fatal("timer cancellation did not finish both waiters")
	}
	clock.Add(int64(time.Minute))
	ownerCtx := withWarmupRequestState(context.Background(), warmupPurposeCount)
	owner, _, err := m.tryWarmupExecution(ownerCtx, a, "")
	if err != nil || owner == nil {
		t.Fatal("could not acquire a real execution slot", err)
	}
	t.Cleanup(owner.release)
	var releaseAttempts atomic.Int32
	done := start(a, &releaseAttempts)
	clock.Add(int64(time.Minute))
	owner.release()
	owner.release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal("real release did not admit the waiter", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("real release failed to wake before the 1-second fallback timer")
	}
	s.gate.mu.Lock()
	pending := s.gate.pending[a.ID] + s.gate.pending[b.ID]
	s.gate.mu.Unlock()
	if s.gate.InFlight(a.ID) != 0 || s.gate.InFlight(b.ID) != 0 || pending != 0 {
		t.Fatal("release was not exact once or leaked a pending reservation", pending)
	}
}

func TestPacingAdmissionDefaultGlobalQueueCancellation(t *testing.T) {
	q := &pacingAdmissionQueue{}
	s := &AdaptiveSelector{}
	cfg := internalconfig.DefaultWarmupTrafficPacingConfig()
	cfg.Enabled = true
	if cfg.WaitConfig().MaxPendingTotal != 32 {
		t.Fatal("fixture must exercise the default global bound")
	}
	var wg sync.WaitGroup
	var cancels []context.CancelFunc
	t.Cleanup(func() {
		for _, cancel := range cancels {
			cancel()
		}
		wg.Wait()
		q.mu.Lock()
		defer q.mu.Unlock()
		if len(q.tickets) != 0 || q.bytes != 0 {
			t.Errorf("cancellation leaked tickets=%d bytes=%d", len(q.tickets), q.bytes)
		}
	})
	start := func(account string, bodyBytes int) (context.CancelFunc, <-chan error) {
		base, cancel := context.WithCancel(context.Background())
		ctx := withWarmupRequestState(base, warmupPurposeServe)
		configurePacingAdmission(ctx, cfg, bodyBytes)
		state := warmupRequestFromContext(ctx)
		state.admission.queue = q
		entered := make(chan struct{})
		state.admissionSleep = func(ctx context.Context, _ <-chan struct{}, _ time.Duration) error {
			close(entered)
			<-ctx.Done()
			return ctx.Err()
		}
		done := make(chan error, 1)
		cancels = append(cancels, cancel)
		wg.Go(func() {
			done <- runWarmupSelection(ctx, func(context.Context) error {
				return &warmupSelectionWait{selector: s, authID: account, reason: "concurrency"}
			})
		})
		select {
		case <-entered:
		case err := <-done:
			t.Fatalf("waiter %s rejected before the bound: %v", account, err)
		case <-time.After(3 * time.Second):
			t.Fatalf("waiter %s did not enter queue", account)
		}
		return cancel, done
	}
	var cancelFirst context.CancelFunc
	var firstDone <-chan error
	for i := 0; i < 32; i++ {
		cancel, done := start(fmt.Sprintf("global-account-%02d", i), 1024)
		if i == 0 {
			cancelFirst, firstDone = cancel, done
		}
	}
	assertTotals := func(count, bytes int) {
		t.Helper()
		q.mu.Lock()
		defer q.mu.Unlock()
		if len(q.tickets) != count || q.bytes != bytes {
			t.Fatalf("queue totals tickets=%d bytes=%d, want %d/%d", len(q.tickets), q.bytes, count, bytes)
		}
	}
	assertTotals(32, 32*1024)
	ctx := withWarmupRequestState(context.Background(), warmupPurposeServe)
	configurePacingAdmission(ctx, cfg, 2048)
	warmupRequestFromContext(ctx).admission.queue = q
	err := runWarmupSelection(ctx, func(context.Context) error {
		return &warmupSelectionWait{selector: s, authID: "global-account-33", reason: "concurrency"}
	})
	var full *warmupBusyError
	if !errors.As(err, &full) || full.cause.Code != "queue_full" {
		t.Fatal("33rd distinct-account waiter was not rejected", err)
	}
	assertTotals(32, 32*1024)
	cancelFirst()
	if err := <-firstDone; !errors.Is(err, context.Canceled) {
		t.Fatal("first waiter did not cancel", err)
	}
	assertTotals(31, 31*1024)
	start("global-account-replacement", 2048)
	assertTotals(32, 33*1024)
}

func TestPacingAdmissionAbsoluteDeadlineAndDisabled(t *testing.T) {
	for _, seconds := range []int{0, 30} {
		now := time.Now()
		ctx, state := virtualWarmupWait(&now)
		cfg := internalconfig.DefaultWarmupTrafficPacingConfig()
		cfg.Enabled = true
		cfg.AdmissionWait = &internalconfig.WarmupAdmissionWaitConfig{MaxWaitSeconds: seconds, MaxPendingPerAccount: 4, MaxPendingTotal: 32}
		configurePacingAdmission(ctx, cfg, 1024)
		state.admission.queue = &pacingAdmissionQueue{}
		defer releasePacingAdmission(ctx)
		if err := runWarmupSelection(ctx, func(context.Context) error { return nil }); err != nil {
			t.Fatal("disabled wait rejected immediate admission", err)
		}
		start := now
		err := runWarmupSelection(ctx, func(context.Context) error { return &warmupSelectionWait{authID: "account", reason: "concurrency"} })
		if err == nil || !strings.Contains(err.Error(), "wait_timeout") || now.Sub(start) != time.Duration(seconds)*time.Second {
			t.Fatalf("deadline: %v %v", err, now.Sub(start))
		}
		configurePacingAdmission(ctx, cfg, 1024)
		if seconds > 0 {
			if err := runWarmupSelection(ctx, func(context.Context) error { return nil }); err == nil {
				t.Fatal("reselection reset absolute deadline")
			}
		}
		if len(state.admission.queue.tickets) != 0 {
			t.Fatal("queue leaked")
		}
	}
}

func TestPacingAdmissionQueueBoundsAndFairness(t *testing.T) {
	q := &pacingAdmissionQueue{}
	cfg := internalconfig.WarmupAdmissionWaitConfig{MaxWaitSeconds: 30, MaxPendingPerAccount: 4, MaxPendingTotal: 32}
	s := &AdaptiveSelector{}
	var states []*warmupRequestState
	for i := 0; i < 4; i++ {
		state := &warmupRequestState{}
		states = append(states, state)
		if _, ok := q.acquire(state, &warmupSelectionWait{selector: s, authID: "account", key: "blocked", ready: func() bool { return false }}, pacingAdmissionState{config: cfg, bodyBytes: 1}); !ok {
			t.Fatal("queue filled before four")
		}
	}
	if _, ok := q.acquire(&warmupRequestState{}, &warmupSelectionWait{selector: s, authID: "account"}, pacingAdmissionState{config: cfg}); ok {
		t.Fatal("fifth account waiter accepted")
	}
	ctx := withWarmupRequestState(context.Background(), warmupPurposeServe)
	state := warmupRequestFromContext(ctx)
	state.admission = &pacingAdmissionState{queue: q}
	if !pacingQueueTurn(ctx, s, "account", "continuation") {
		t.Fatal("ineligible head blocked another binding")
	}
	if pacingQueueTurn(ctx, s, "account", "blocked") {
		t.Fatal("same-binding FIFO bypassed")
	}
	q.tickets[1].wait.ready = func() bool { return true }
	if pacingQueueTurn(ctx, s, "account", "continuation") {
		t.Fatal("new request jumped a ready waiter")
	}
	for _, state := range states {
		q.release(state)
	}
	if _, ok := q.acquire(state, &warmupSelectionWait{authID: "large"}, pacingAdmissionState{config: cfg, bodyBytes: pacingPendingBytesLimit + 1}); ok {
		t.Fatal("byte ceiling bypassed")
	}
	if len(q.tickets) != 0 || q.bytes != 0 {
		t.Fatal("queue budget leaked")
	}
}

func TestPacingAdmissionTwoWarmingAccountsReleaseBeforeWait(t *testing.T) {
	m, s, a, e, now, model := pacingExecutionFixture(t)
	second := newAdaptiveClaudeAuth("second-warming", "default_claude_max_5x", now.Add(-24*time.Hour))
	if _, err := m.Register(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	registry.GetGlobalRegistry().RegisterClient(second.ID, "claude", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(second.ID) })
	*now = now.Add(time.Hour)
	s.gate.Acquire(a.ID, 1)
	s.gate.Acquire(second.ID, 1)
	ctx, state := virtualWarmupWait(now)
	waits := 0
	state.sleep = func(_ context.Context, d time.Duration) error {
		waits++
		if e.sends.Load() != 0 {
			t.Fatal("sent while concurrency-full")
		}
		s.gate.Release(a.ID)
		s.gate.Release(second.ID)
		*now = now.Add(d)
		return nil
	}
	opts := servingOptions("thin-pool", "", "system", "Review the local admission queue.")
	_, err := m.Execute(ctx, []string{"claude"}, cliproxyexecutor.Request{Model: model, Payload: opts.OriginalRequest}, opts)
	if err != nil || waits != 1 || e.sends.Load() != 1 {
		t.Fatalf("thin pool returned instead of waiting: waits=%d sends=%d err=%v", waits, e.sends.Load(), err)
	}
	if s.gate.InFlight(a.ID) != 0 || s.gate.InFlight(second.ID) != 0 {
		t.Fatal("execution slot leaked")
	}
}

func TestPacingAdmissionHookReleasesOuterSlot(t *testing.T) {
	m, s, a, e, now, model := pacingExecutionFixture(t)
	*now = now.Add(time.Hour)
	cfg := *m.runtimeConfig.Load().(*internalconfig.Config)
	cfg.AccountScheduling.WarmupCurve = append([]internalconfig.AccountWarmupStage(nil), cfg.AccountScheduling.WarmupCurve...)
	for i := range cfg.AccountScheduling.WarmupCurve {
		cfg.AccountScheduling.WarmupCurve[i].ConcurrencyLimit = 2
	}
	m.SetConfig(&cfg)
	s.gate.Acquire(a.ID, 2)
	ctx, state := virtualWarmupWait(now)
	waits := 0
	state.sleep = func(_ context.Context, d time.Duration) error {
		waits++
		if got := s.gate.InFlight(a.ID); got != 1 {
			t.Fatalf("HTTP hook retained its outer slot while waiting: %d", got)
		}
		if e.sends.Load() != 0 {
			t.Fatal("sent before legacy slot drained")
		}
		s.gate.Release(a.ID)
		*now = now.Add(d)
		return nil
	}
	opts := servingOptions("hook-wait", "", "system", "Review a pre-enable overlap.")
	_, err := m.Execute(ctx, []string{"claude"}, cliproxyexecutor.Request{Model: model, Payload: opts.OriginalRequest}, opts)
	if err != nil || waits != 1 || e.sends.Load() != 1 || s.gate.InFlight(a.ID) != 0 {
		t.Fatalf("hook wait failed: %v waits=%d sends=%d slots=%d", err, waits, e.sends.Load(), s.gate.InFlight(a.ID))
	}
}

func TestPacingAdmissionCancellation(t *testing.T) {
	now := time.Now()
	ctx, state := virtualWarmupWait(&now)
	cfg := internalconfig.DefaultWarmupTrafficPacingConfig()
	cfg.Enabled = true
	configurePacingAdmission(ctx, cfg, 4)
	state.admission.queue = &pacingAdmissionQueue{}
	state.sleep = func(context.Context, time.Duration) error { return context.Canceled }
	err := runWarmupSelection(ctx, func(context.Context) error { return &warmupSelectionWait{authID: "account", reason: "concurrency"} })
	if !errors.Is(err, context.Canceled) || len(state.admission.queue.tickets) != 0 {
		t.Fatal("cancellation leaked ticket", err)
	}
}

func TestPacingAdmissionInactivePreservesOtherProviderRetry(t *testing.T) {
	now := time.Now()
	ctx, state := virtualWarmupWait(&now)
	cfg := internalconfig.DefaultWarmupTrafficPacingConfig()
	cfg.Enabled = true
	configurePacingAdmission(ctx, cfg, 12)
	now = now.Add(time.Minute)
	if pacingAdmissionExpired(ctx) || !pacingRetryFits(ctx, 30*time.Second) {
		t.Fatal("non-warming request inherited admission deadline")
	}
	activatePacingAdmission(ctx)
	now = now.Add(31 * time.Second)
	if !pacingAdmissionExpired(ctx) || pacingRetryFits(ctx, time.Second) {
		t.Fatal("actual warming request escaped shared deadline")
	}
	if ctx.Err() != nil {
		t.Fatal("admission deadline canceled ongoing generation")
	}
	state.admission.queue.release(state)
}

func TestPacingAdmissionExecutionWaitPreventsBarging(t *testing.T) {
	m, s, a, _, now, model := pacingExecutionFixture(t)
	*now = now.Add(time.Hour)
	ctx, state := virtualWarmupWait(now)
	configurePacingAdmission(ctx, m.accountSchedulingConfig().WarmupTrafficPacing, 10)
	state.admission.queue = &pacingAdmissionQueue{}
	// Count requests have no binding: prove the execution-generated waiter
	// still receives a resource readiness probe rather than a nil callback.
	state.purpose = warmupPurposeCount
	s.gate.Acquire(a.ID, 1)
	_, _, err := m.tryWarmupExecution(ctx, a, model)
	var wait *warmupSelectionWait
	if !errors.As(err, &wait) || wait.ready == nil {
		t.Fatal("execution wait has no readiness probe", err)
	}
	if _, ok := state.admission.queue.acquire(state, wait, *state.admission); !ok {
		t.Fatal("could not queue waiter")
	}
	s.gate.Release(a.ID)
	newCtx, newState := virtualWarmupWait(now)
	configurePacingAdmission(newCtx, m.accountSchedulingConfig().WarmupTrafficPacing, 10)
	newState.admission.queue = state.admission.queue
	if pacingQueueTurn(newCtx, s, a.ID, "other") {
		t.Fatal("late binding jumped ready execution waiter")
	}
	state.admission.queue.release(state)
}

func TestPacingAdmissionConfigChangeAndAccountRemoval(t *testing.T) {
	for _, remove := range []bool{false, true} {
		t.Run(fmt.Sprint(remove), func(t *testing.T) {
			m, s, a, e, now, model := pacingExecutionFixture(t)
			*now = now.Add(time.Hour)
			s.gate.Acquire(a.ID, 1)
			defer s.gate.Release(a.ID)
			ctx, state := virtualWarmupWait(now)
			waits := 0
			state.sleep = func(context.Context, time.Duration) error {
				waits++
				if remove {
					m.Remove(context.Background(), a.ID)
				} else {
					cfg := *m.runtimeConfig.Load().(*internalconfig.Config)
					cfg.AccountScheduling.WarmupTrafficPacing.AdmissionWait = &internalconfig.WarmupAdmissionWaitConfig{MaxWaitSeconds: 0, MaxPendingPerAccount: 4, MaxPendingTotal: 32}
					m.SetConfig(&cfg)
				}
				return nil
			}
			opts := servingOptions("changed-account", "", "system", "Inspect local backpressure.")
			_, err := m.Execute(ctx, []string{"claude"}, cliproxyexecutor.Request{Model: model, Payload: opts.OriginalRequest}, opts)
			if err == nil || waits != 1 || e.sends.Load() != 0 {
				t.Fatalf("changed account remained waiting or sent: waits=%d err=%v", waits, err)
			}
			if sharedPacingAdmissionQueue.waiting(s, a.ID) != 0 {
				t.Fatal("changed account retained queue ticket")
			}
			if current, ok := m.GetByID(a.ID); ok && current.Failed != 0 {
				t.Fatal("local rejection affected account health")
			}
		})
	}
}

func TestPacingAdmissionDynamicThresholds(t *testing.T) {
	for _, threshold := range []int{1, 4, 6} {
		t.Run(fmt.Sprint(threshold), func(t *testing.T) {
			m, s, a, _, now, _ := pacingExecutionFixture(t)
			cfg := *m.runtimeConfig.Load().(*internalconfig.Config)
			cfg.AccountScheduling.WarmupTrafficPacing.MinAdmissionRequests = threshold
			m.SetConfig(&cfg)
			p := m.pacing.pacer
			account := p.accounts[pacingDigest(a.ID)]
			ledger := clonePacingState(account.state)
			ledger.Balance = float64(threshold) - 0.01
			if err := p.commit(account, ledger); err != nil {
				t.Fatal(err)
			}
			ctx, state := virtualWarmupWait(now)
			configurePacingAdmission(ctx, cfg.AccountScheduling.WarmupTrafficPacing, 1)
			ctx = context.WithValue(ctx, pacingSelectionKey{}, WarmupPacingRequest{GroupID: "group", BindingID: "binding"})
			start := *now
			err := runWarmupSelection(ctx, func(ctx context.Context) error {
				if wait := s.admissionWaitFor(ctx, a, cfg.AccountScheduling, *now, "binding", 0); wait != nil {
					return wait
				}
				if !s.pacingDecision(ctx, a, cfg.AccountScheduling, *now).Allowed {
					return errors.New("threshold did not recover")
				}
				return nil
			})
			releasePacingAdmission(ctx)
			if err != nil || now.Sub(start) < 4*time.Second || now.Sub(start) > 5*time.Second || state.waited >= warmupWaitLimit {
				t.Fatalf("threshold %d wait=%v err=%v", threshold, now.Sub(start), err)
			}
		})
	}
}

func TestPacingAdmissionSameBindingWaitsEvenWithMatureFallback(t *testing.T) {
	m, s, a, e, now, model := pacingExecutionFixture(t)
	s.rng = func() float64 { return 0 }
	pacingAddMature(t, m, model, *now)
	*now = now.Add(time.Hour)
	opts := servingOptions("serial-root", "", "system", "Continue one binding in order.")
	req := cliproxyexecutor.Request{Model: model, Payload: opts.OriginalRequest}
	picked := ""
	e.before = func(_ context.Context, current *Auth) { picked = current.ID }
	ctx, _ := virtualWarmupWait(now)
	if _, err := m.Execute(ctx, []string{"claude"}, req, opts); err != nil || picked != a.ID {
		t.Fatal("warming binding fixture failed", picked, err)
	}
	*now = now.Add(21 * time.Second)
	key := "mixed::claude:serial-root::" + model
	if _, ok := s.servingEntry(key, *now); !ok {
		key = "claude::claude:serial-root::" + model
	}
	if !s.gate.reserveWarmup(a.ID, 1, 200, nil, key) {
		t.Fatal("could not hold existing binding")
	}
	ctx, state := virtualWarmupWait(now)
	waits := 0
	state.sleep = func(_ context.Context, d time.Duration) error {
		waits++
		if e.sends.Load() != 1 {
			t.Fatal("same binding bypassed wait through mature fallback")
		}
		s.gate.releaseWarmupReservation(a.ID, key)
		*now = now.Add(d)
		return nil
	}
	if _, err := m.Execute(ctx, []string{"claude"}, req, opts); err != nil || waits != 1 || picked != a.ID {
		t.Fatalf("same binding reordered: picked=%s waits=%d err=%v", picked, waits, err)
	}
}

func TestPacingAdmissionBindingReleasedDuringReadinessProbe(t *testing.T) {
	m, s, a, _, now, model := pacingExecutionFixture(t)
	*now = now.Add(time.Hour)
	opts := servingOptions("release-between-probes", "", "system", "Continue after the previous stream releases.")
	req := cliproxyexecutor.Request{Model: model, Payload: opts.OriginalRequest}
	ctx, _ := virtualWarmupWait(now)
	if _, err := m.Execute(ctx, []string{"claude"}, req, opts); err != nil {
		t.Fatal("could not establish the committed binding", err)
	}
	*now = now.Add(21 * time.Second)
	key := "mixed::claude:release-between-probes::" + model
	if _, ok := s.servingEntry(key, *now); !ok {
		t.Fatal("committed binding missing")
	}
	if !s.gate.reserveWarmup(a.ID, 1, 200, nil, key) {
		t.Fatal("could not hold the existing binding")
	}
	defer s.gate.releaseWarmupReservation(a.ID, key)

	// PeekConfigured reads this clock after pickWithWarmupServing has observed
	// bindingInFlight, but before admissionWaitFor rechecks readiness. Release
	// synchronously here to cover the real stream-completion interleaving
	// without scheduler timing, longer waits or different account ceilings.
	pacer := s.pacer.Load()
	previousClock := pacer.now
	defer func() { pacer.now = previousClock }()
	released := false
	pacer.now = func() time.Time {
		if !released {
			if !s.gate.bindingInFlight(key) {
				t.Fatal("fixture did not reach the busy-binding probe")
			}
			released = true
			s.gate.releaseWarmupReservation(a.ID, key)
		}
		return *now
	}
	latest, _ := m.GetByID(a.ID)
	ctx, _ = virtualWarmupWait(now)
	picked, handled, err := s.pickWithWarmupServing(ctx, "mixed", model, opts, []*Auth{latest}, []*Auth{latest}, m.accountSchedulingConfig(), *now)
	if !released || err != nil || !handled || picked == nil || picked.ID != a.ID {
		t.Fatalf("released binding did not resume normal selection: released=%v handled=%v picked=%v err=%v", released, handled, picked, err)
	}
	if s.gate.InFlight(a.ID) != 0 || s.gate.bindingInFlight(key) {
		t.Fatal("release probe leaked a reservation")
	}
}
