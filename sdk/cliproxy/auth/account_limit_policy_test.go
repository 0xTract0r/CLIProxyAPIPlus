package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestAccountLimitPolicyExpiryReadOnlySnapshot(t *testing.T) {
	m, s, a, cfg, now := pacingManagerFixture(t)
	*now = now.Add(time.Hour)
	updated, err := m.UpdateAccountScheduling(context.Background(), a.ID, a.Index, func(a *Auth, c internalconfig.AccountSchedulingConfig) error {
		return ApplyAccountLimitPatch(a, c, nil, []byte(`{"duration_minutes":15,"overrides":{"rpm":8,"concurrency":2,"request_capacity":12,"max_active_bindings":2}}`), *now)
	})
	if err != nil {
		t.Fatal(err)
	}
	updated.temporaryLimits.started = time.Now().Add(-16 * time.Minute)
	key := pacingDigest(a.ID)
	before := clonePacingState(m.pacing.pacer.accounts[key].state)
	path := filepath.Join(cfg.AuthDir, key+".pacing")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	view := m.WarmupTrafficPacingSnapshot(a.ID)
	if *view.RequestCapacity != 8 || *view.RequestBalance != 8 || *view.RPMLimit != 3 || *view.ConcurrencyLimit != 1 || *view.MaxActiveBindingGroups != 1 {
		t.Fatal("expired snapshot shows stale temporary limits", view)
	}
	if AccountLimitPolicyFor(updated, *now, cfg.AccountScheduling).TemporaryStatus != "expired" {
		t.Fatal("policy and snapshot disagree")
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(data, after) || !reflect.DeepEqual(before, m.pacing.pacer.accounts[key].state) {
		t.Fatal("GET expiry changed durable or live ledger")
	}
	limits := pacingLimitsFor(s, updated, cfg.AccountScheduling, *now)
	lease, d, err := m.pacing.pacer.Reserve(a.ID, limits, WarmupPacingRequest{CountOnly: true, EstimateKnown: true})
	if err != nil || !d.Allowed || limits.Config.RequestBurst != *view.RequestCapacity || limits.Concurrency != *view.ConcurrencyLimit {
		t.Fatal("next admission disagrees with projection", d, err)
	}
	if err = m.pacing.pacer.CancelUnsent(lease); err != nil {
		t.Fatal(err)
	}
}

type blockingPolicyStore struct {
	requestPrepareStore
	block   atomic.Bool
	entered chan struct{}
	resume  chan struct{}
}

func (s *blockingPolicyStore) Save(ctx context.Context, a *Auth) (string, error) {
	if s.block.CompareAndSwap(true, false) {
		close(s.entered)
		<-s.resume
	}
	return s.requestPrepareStore.Save(ctx, a)
}

func TestAccountLimitPolicyLateGenericSaveCannotRevert(t *testing.T) {
	store := &blockingPolicyStore{entered: make(chan struct{}), resume: make(chan struct{})}
	m := NewManager(store, nil, nil)
	cfg := internalconfig.DefaultAccountSchedulingConfig()
	m.SetConfig(&internalconfig.Config{AccountScheduling: cfg})
	a := newAdaptiveClaudeAuth("late-save", "default_claude_max_5x", time.Now().Add(-24*time.Hour))
	if _, err := m.Register(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	stale := a.Clone()
	store.block.Store(true)
	oldDone := make(chan error, 1)
	go func() { _, err := m.Update(context.Background(), stale.Clone()); oldDone <- err }()
	<-store.entered
	newDone := make(chan error, 1)
	go func() {
		_, err := m.UpdateAccountScheduling(context.Background(), a.ID, "", func(a *Auth, cfg internalconfig.AccountSchedulingConfig) error {
			return ApplyAccountLimitPatch(a, cfg, []byte(`{"rpm":8}`), nil, time.Now())
		})
		newDone <- err
	}()
	close(store.resume)
	if err := <-oldDone; err != nil {
		t.Fatal(err)
	}
	if err := <-newDone; err != nil {
		t.Fatal(err)
	}
	// Also reproduce a snapshot whose Save starts only after PATCH succeeded.
	if err := m.persist(context.Background(), stale); err != nil {
		t.Fatal(err)
	}
	if got := storedLimitOverrides(store.lastAuth())["rpm"]; got != 8 {
		t.Fatal("late generic save reverted acknowledged policy", got)
	}
	beforeClear, _ := m.GetByID(a.ID)
	if _, err := m.UpdateAccountScheduling(context.Background(), a.ID, "", func(a *Auth, cfg internalconfig.AccountSchedulingConfig) error {
		return ApplyAccountLimitPatch(a, cfg, []byte(`null`), nil, time.Now())
	}); err != nil {
		t.Fatal(err)
	}
	if err := m.persist(context.Background(), beforeClear); err != nil {
		t.Fatal(err)
	}
	if len(storedLimitOverrides(store.lastAuth())) != 0 {
		t.Fatal("late save revived explicitly cleared fixed overrides")
	}
}

func TestAccountLimitPolicyPrecedenceExpiryAndRestart(t *testing.T) {
	now := time.Now()
	cfg := internalconfig.DefaultAccountSchedulingConfig()
	a := newAdaptiveClaudeAuth("limits", "default_claude_max_5x", now.Add(-24*time.Hour))
	if err := ApplyAccountLimitPatch(a, cfg, []byte(`{"rpm":6,"concurrency":2}`), []byte(`{"duration_minutes":15,"overrides":{"rpm":10}}`), now); err != nil {
		t.Fatal(err)
	}
	view := AccountLimitPolicyFor(a, now.Add(10*time.Minute), cfg)
	if view.Fields["rpm"].Effective != 10 || view.Fields["concurrency"].Effective != 2 || view.Fields["concurrency"].Source != "fixed" {
		t.Fatal("field precedence incorrect", view)
	}
	if err := ApplyAccountLimitPatch(a, cfg, []byte(`{"rpm":8}`), nil, now.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	// Wall time moves backwards inside the original interval while monotonic
	// elapsed time has reached expiry. The expired marker cannot revive.
	a.temporaryLimits.started = time.Now().Add(-16 * time.Minute)
	for _, at := range []time.Time{now.Add(5 * time.Minute), now.Add(time.Minute)} {
		view = AccountLimitPolicyFor(a, at, cfg)
		if view.TemporaryStatus != "expired" || view.Fields["rpm"].Effective != 8 || view.Fields["rpm"].Source != "fixed" {
			t.Fatal("rollback renewed expiry or restored stale fixed policy", view)
		}
	}
	if err := ApplyAccountLimitPatch(a, cfg, nil, []byte(`{"duration_minutes":15,"overrides":{"rpm":10}}`), now); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(a)
	var restarted Auth
	_ = json.Unmarshal(b, &restarted)
	view = AccountLimitPolicyFor(&restarted, now.Add(time.Minute), cfg)
	if view.TemporaryStatus != "restart_expired" || view.Fields["rpm"].Effective != 8 {
		t.Fatal("restart restored temporary boost", view)
	}
	if err := ApplyAccountLimitPatch(a, cfg, nil, []byte(`null`), now); err != nil {
		t.Fatal(err)
	}
	if view = AccountLimitPolicyFor(a, now, cfg); view.TemporaryStatus != "ended" || view.Fields["rpm"].Effective != 8 {
		t.Fatal("early termination lost fixed layer", view)
	}
}

func TestAccountLimitPolicyHealthAndMatureBaseline(t *testing.T) {
	now := time.Now()
	cfg := internalconfig.DefaultAccountSchedulingConfig()
	cfg.HealthGate.Enabled = true
	a := newAdaptiveClaudeAuth("health", "default_claude_max_5x", now.Add(-10*24*time.Hour))
	a.setHealthStageCap(0)
	if err := ApplyAccountLimitPatch(a, cfg, []byte(`{"rpm":60,"concurrency":4,"request_capacity":32,"max_active_bindings":8}`), nil, now); err != nil {
		t.Fatal(err)
	}
	view := AccountLimitPolicyFor(a, now, cfg)
	for key, f := range view.Fields {
		if f.Effective > f.Baseline || f.Source != "restricted" {
			t.Fatalf("%s escaped health ceiling: %+v", key, f)
		}
	}
	mature := newAdaptiveClaudeAuth("mature", "default_claude_max_5x", now.Add(-90*24*time.Hour))
	cfg.MatureLimits.RPMLimit = 90
	cfg.MatureLimits.ConcurrencyLimit = 8
	cfg.RateScale = 1.5
	view = AccountLimitPolicyFor(mature, now, cfg)
	if view.Fields["rpm"].Effective != 135 || view.Fields["concurrency"].Effective != 12 {
		t.Fatal("edit bounds clamped historical baseline", view)
	}
}

func TestAccountLimitPolicyExecutionAndNoMint(t *testing.T) {
	m, s, a, cfg, now := pacingManagerFixture(t)
	*now = now.Add(time.Hour)
	updated, err := m.UpdateAccountScheduling(context.Background(), a.ID, a.Index, func(a *Auth, c internalconfig.AccountSchedulingConfig) error {
		return ApplyAccountLimitPatch(a, c, []byte(`{"rpm":8,"concurrency":2,"request_capacity":12,"max_active_bindings":2}`), nil, *now)
	})
	if err != nil {
		t.Fatal(err)
	}
	view := m.WarmupTrafficPacingSnapshot(a.ID)
	if *view.RequestBalance != 8 || *view.RequestCapacity != 12 || *view.RPMLimit != 8 || *view.ConcurrencyLimit != 2 || *view.MaxActiveBindingGroups != 2 || *view.DailyRequestBudget != 200 {
		t.Fatal("limit patch minted credits or changed unrelated budget", view)
	}
	if rpm, _ := s.rateLimitParams(updated, cfg.AccountScheduling, *now); rpm != 8 {
		t.Fatal("selector ignored RPM override", rpm)
	}
	if !s.gate.reserveWarmup(a.ID, 2, 200, nil, "one") || !s.gate.reserveWarmup(a.ID, 2, 200, nil, "two") || s.gate.reserveWarmup(a.ID, 2, 200, nil, "one") {
		t.Fatal("different bindings cannot use two slots, or same binding overlapped")
	}
	s.gate.releaseWarmupReservation(a.ID, "one")
	s.gate.releaseWarmupReservation(a.ID, "two")
	if !s.hasConcurrencyHeadroom(updated, cfg.AccountScheduling, *now) {
		t.Fatal("gate retained released concurrency")
	}
}

type failingPolicyStore struct {
	requestPrepareStore
	fail bool
}

func (s *failingPolicyStore) Save(ctx context.Context, a *Auth) (string, error) {
	if s.fail {
		return "", errors.New("synthetic persistence failure")
	}
	return s.requestPrepareStore.Save(ctx, a)
}

func TestAccountLimitPolicyAtomicSaveAndRuntimePreservation(t *testing.T) {
	now := time.Now()
	cfg := internalconfig.DefaultAccountSchedulingConfig()
	store := &failingPolicyStore{}
	m := NewManager(store, nil, nil)
	m.SetConfig(&internalconfig.Config{AccountScheduling: cfg})
	a := newAdaptiveClaudeAuth("persist", "default_claude_max_5x", now.Add(-24*time.Hour))
	if _, err := m.Register(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	apply := func(a *Auth, cfg internalconfig.AccountSchedulingConfig) error {
		return ApplyAccountLimitPatch(a, cfg, []byte(`{"rpm":8}`), []byte(`{"duration_minutes":15,"overrides":{"rpm":10}}`), now)
	}
	store.fail = true
	if _, err := m.UpdateAccountScheduling(context.Background(), a.ID, "", apply); !errors.Is(err, ErrAccountSchedulingPersist) {
		t.Fatal("save failure ignored", err)
	}
	current, _ := m.GetByID(a.ID)
	if len(storedLimitOverrides(current)) != 0 || current.temporaryLimits != nil {
		t.Fatal("failed save published policy")
	}
	store.fail = false
	updated, err := m.UpdateAccountScheduling(context.Background(), a.ID, "", apply)
	if err != nil {
		t.Fatal(err)
	}
	proof := updated.temporaryLimits
	for _, register := range []bool{false, true} {
		b, _ := json.Marshal(updated)
		var reloaded Auth
		_ = json.Unmarshal(b, &reloaded)
		if register {
			updated, err = m.Register(context.Background(), &reloaded)
		} else {
			updated, err = m.Update(context.Background(), &reloaded)
		}
		if err != nil || updated.temporaryLimits != proof || AccountLimitPolicyFor(updated, now, cfg).TemporaryStatus != "active" {
			t.Fatal("same-instance watcher update lost monotonic proof", err)
		}
	}
	var wg sync.WaitGroup
	for _, raw := range []string{`{"concurrency":2}`, `{"max_active_bindings":2}`} {
		wg.Go(func() {
			_, err := m.UpdateAccountScheduling(context.Background(), a.ID, "", func(a *Auth, cfg internalconfig.AccountSchedulingConfig) error {
				return ApplyAccountLimitPatch(a, cfg, []byte(raw), nil, now)
			})
			if err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	current, _ = m.GetByID(a.ID)
	if overrides := storedLimitOverrides(current); overrides["concurrency"] != 2 || overrides["max_active_bindings"] != 2 {
		t.Fatal("concurrent field merge lost a change", overrides)
	}
}

func TestAccountLimitPolicyRejectInvalidValues(t *testing.T) {
	cfg := internalconfig.DefaultAccountSchedulingConfig()
	a := &Auth{}
	for _, raw := range []string{`{"rpm":1e99}`, `{"rpm":1.5}`, `{"concurrency":5}`, `{"request_capacity":3}`, `{"max_active_bindings":0}`, `{"oops":1}`} {
		if ApplyAccountLimitPatch(a, cfg, []byte(raw), nil, time.Now()) == nil {
			t.Fatal("invalid overrides accepted", raw)
		}
	}
	for _, v := range []any{math.Inf(1), math.NaN(), "Infinity", math.MaxFloat64} {
		if _, ok := ParseRateScaleValue(v); ok {
			t.Fatal("nonfinite/overflow scale accepted", v)
		}
	}
	a.SetAccountRateScale(1e18)
	if ApplyAccountLimitPatch(a, cfg, nil, nil, time.Now()) == nil {
		t.Fatal("finite scale with overflowing derived limits accepted")
	}
}
