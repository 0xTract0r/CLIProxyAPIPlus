package auth

import (
	"context"
	"math"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func pacingManagerFixture(t *testing.T) (*Manager, *AdaptiveSelector, *Auth, *internalconfig.Config, *time.Time) {
	t.Helper()
	cfg := &internalconfig.Config{AccountScheduling: internalconfig.DefaultAccountSchedulingConfig()}
	cfg.AuthDir = t.TempDir()
	cfg.AccountScheduling.WarmupTrafficPacing.Enabled = true
	for i := range cfg.AccountScheduling.WarmupCurve {
		cfg.AccountScheduling.WarmupCurve[i].DailyBudget = 200
		cfg.AccountScheduling.WarmupCurve[i].RPMLimit = 3
		cfg.AccountScheduling.WarmupCurve[i].ConcurrencyLimit = 1
	}
	now := time.Now()
	var m *Manager
	s := NewAdaptiveSelector(AdaptiveSelectorConfig{Scheduling: cfg.AccountScheduling, SessionAffinity: true}, WithAdaptiveClock(func() time.Time { return now }), WithAdaptiveSchedulingProvider(func() internalconfig.AccountSchedulingConfig { return m.accountSchedulingConfig() }))
	t.Cleanup(s.Stop)
	m = NewManager(nil, s, nil)
	m.SetConfig(cfg)
	a := newAdaptiveClaudeAuth("synthetic-history-"+t.Name(), "default_claude_max_5x", now.Add(-24*time.Hour))
	if _, err := m.Register(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { RegisterAccountPacingUsageSink(nil) })
	return m, s, a, cfg, &now
}

func TestWarmupPacingManagerHistoryOffAndRestart(t *testing.T) {
	m, s, a, cfg, now := pacingManagerFixture(t)
	p := m.pacing.pacer
	m.MarkResult(context.Background(), Result{AuthID: a.ID, Provider: "claude", Success: true})
	d, err := p.PeekConfigured(a.ID, WarmupPacingRequest{CountOnly: true, EstimateKnown: true})
	if err != nil || d.DayRequests != 1 {
		t.Fatalf("legacy result missing: %+v %v", d, err)
	}
	off := *cfg
	off.AccountScheduling.WarmupTrafficPacing.Enabled = false
	m.SetConfig(&off)
	m.MarkResult(context.Background(), Result{AuthID: a.ID, Provider: "claude", Success: true})
	if !m.recordPacingUsage(context.Background(), "claude", a.ID, 0) {
		t.Fatal("unknown zero usage not handled")
	}
	m.mu.RLock()
	snapshot := m.auths[a.ID].Clone()
	m.mu.RUnlock()
	s2 := NewAdaptiveSelector(AdaptiveSelectorConfig{Scheduling: off.AccountScheduling}, WithAdaptiveClock(func() time.Time { return *now }))
	t.Cleanup(s2.Stop)
	m2 := NewManager(nil, s2, nil)
	m2.SetConfig(&off)
	if _, err := m2.Register(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	if m2.pacing == nil || m2.pacing.pacer == nil {
		t.Fatal("disabled restart failed to restore history")
	}
	m2.MarkResult(context.Background(), Result{AuthID: a.ID, Provider: "claude", Success: true})
	d, err = m2.pacing.pacer.PeekConfigured(a.ID, WarmupPacingRequest{CountOnly: true, EstimateKnown: true})
	if err != nil || d.DayRequests != 3 {
		t.Fatalf("off-period or restart history lost: %+v %v", d, err)
	}
	if s.gate.DailyCount(a.ID) != 2 {
		t.Fatal("legacy result accounting changed")
	}
}

func TestWarmupPacingManagerOwnedResultAndUsage(t *testing.T) {
	m, s, a, cfg, now := pacingManagerFixture(t)
	*now = now.Add(time.Hour)
	l := pacingLimitsFor(s, a, cfg.AccountScheduling, *now)
	permit, d, err := m.pacing.pacer.Reserve(a.ID, l, WarmupPacingRequest{GroupID: "group", BindingID: "binding", EstimatedTokens: 100, EstimateKnown: true})
	if err != nil || !d.Allowed {
		t.Fatalf("reserve: %+v %v", d, err)
	}
	if err = m.pacing.pacer.MarkSent(permit); err != nil {
		t.Fatal(err)
	}
	if err = m.pacing.pacer.Finish(permit, true, 20); err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(context.Background(), pacingOwnershipKey{}, &pacingOwnership{latest: permit})
	m.MarkResult(ctx, Result{AuthID: a.ID, Provider: "claude", Success: true})
	if !m.recordPacingUsage(ctx, "claude", a.ID, 20) {
		t.Fatal("owned usage not handled")
	}
	d, err = m.pacing.pacer.PeekConfigured(a.ID, WarmupPacingRequest{CountOnly: true, EstimateKnown: true})
	if err != nil || d.DayRequests != 1 || d.Tokens != 20 {
		t.Fatalf("owned consumption counted twice: %+v %v", d, err)
	}
	if s.gate.DailyCount(a.ID) != 1 || s.gate.TokenCount(a.ID) != 20 {
		t.Fatal("old counters were skipped")
	}
	m.refreshWarmupPacing()
	d, _ = m.pacing.pacer.PeekConfigured(a.ID, WarmupPacingRequest{CountOnly: true, EstimateKnown: true})
	if d.DayRequests != 1 || d.Tokens != 20 {
		t.Fatal("owned watermark lost on policy refresh", d)
	}
}

func TestWarmupPacingManagerDirectoryChangeFailsClosed(t *testing.T) {
	m, _, _, cfg, _ := pacingManagerFixture(t)
	old := m.pacing.pacer
	changed := *cfg
	changed.AuthDir = t.TempDir()
	m.SetConfig(&changed)
	if m.pacing.fault == nil || m.pacing.pacer != old {
		t.Fatal("directory change replaced durable accounting")
	}
}

func TestWarmupPacingManagerReplacedSelectorLegacySlot(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		t.Run(map[bool]string{false: "result", true: "cancel"}[cancel], func(t *testing.T) {
			m, old, a, cfg, _ := pacingManagerFixture(t)
			slot := &accountExecutionSlot{gate: old.gate, authID: a.ID, target: true, dailyBudget: 200, manager: m}
			newSelector := NewAdaptiveSelector(AdaptiveSelectorConfig{Scheduling: cfg.AccountScheduling})
			t.Cleanup(newSelector.Stop)
			newSelector.gate.daily[a.ID] = &rollingWindow{}
			m.SetSelector(newSelector)
			if cancel {
				m.recordWarmupCancellation(context.Background(), slot)
			} else {
				m.MarkResult(withWarmupExecutionSlot(context.Background(), slot), Result{AuthID: a.ID, Provider: "claude", Success: true})
			}
			d, err := m.pacing.pacer.PeekConfigured(a.ID, WarmupPacingRequest{CountOnly: true, EstimateKnown: true})
			if err != nil || d.DayRequests != 1 || newSelector.gate.DailyCount(a.ID) != 1 {
				t.Fatalf("old slot result lost across selector replacement: %+v %v current=%d", d, err, newSelector.gate.DailyCount(a.ID))
			}
		})
	}
}

func TestWarmupPacingManagerUninitializedTransactionSerializesEnable(t *testing.T) {
	m := NewManager(nil, nil, nil)
	txn := m.beginPacingHistory("synthetic-uninitialized")
	if m.pacingMu.TryLock() {
		m.pacingMu.Unlock()
		t.Fatal("uninitialized result leaves a first-enable import gap")
	}
	m.finishPacingHistory(context.Background(), txn)
	if !m.pacingMu.TryLock() {
		t.Fatal("transaction leaked coordination lock")
	}
	m.pacingMu.Unlock()
}

func TestWarmupPacingManagerHealthDemotionChangesRefillAtResult(t *testing.T) {
	m, s, a, cfg, now := pacingManagerFixture(t)
	updated := *cfg
	updated.AccountScheduling.WarmupCurve = []internalconfig.AccountWarmupStage{
		{Name: "early", MinAgeDays: 0, MaxAgeDays: 1, DailyBudget: 100, RPMLimit: 3, ConcurrencyLimit: 1},
		{Name: "later", MinAgeDays: 1, MaxAgeDays: 60, DailyBudget: 200, RPMLimit: 3, ConcurrencyLimit: 1},
	}
	updated.AccountScheduling.HealthGate.Enabled = true
	updated.AccountScheduling.HealthGate.FailureClusterThreshold = 1
	m.SetConfig(&updated)
	if status := AccountWarmupStatusFor(a, *now, updated.AccountScheduling); status.DailyBudget != 200 {
		t.Fatal("fixture is not later stage", status)
	}
	*now = now.Add(432 * time.Second)
	m.MarkResult(context.Background(), Result{AuthID: a.ID, Provider: "claude", Success: false, Error: &Error{HTTPStatus: 401, Message: "synthetic unauthorized"}})
	m.mu.RLock()
	latest := m.auths[a.ID].Clone()
	m.mu.RUnlock()
	if status := AccountWarmupStatusFor(latest, *now, updated.AccountScheduling); status.DailyBudget != 100 {
		t.Fatal("result did not demote fixture", status)
	}
	*now = now.Add(432 * time.Second)
	d, err := m.pacing.pacer.PeekConfigured(a.ID, WarmupPacingRequest{CountOnly: true, EstimateKnown: true})
	if err != nil || math.Abs(d.Balance-1.5) > 1e-7 {
		t.Fatalf("idle used old refill after result demotion: %+v %v", d, err)
	}
	if got := pacingLimitsFor(s, latest, updated.AccountScheduling, *now).DailyRequests; got != 100 {
		t.Fatal("latest limits disagree", got)
	}
}
