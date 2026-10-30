package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func controlPatch(t *testing.T, a *Auth, raw string) {
	t.Helper()
	var req map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatal(err)
	}
	if err := ApplyAccountWarmupControlPatch(a, req); err != nil {
		t.Fatal(err)
	}
}

func TestAccountWarmupControlProfileStageParity(t *testing.T) {
	now := time.Now()
	cfg := internalconfig.DefaultAccountSchedulingConfig()
	cfg.WarmupTrafficPacing.Enabled = true
	for i := range cfg.WarmupCurve {
		cfg.WarmupCurve[i].TokenDailyBudget = 1000 + i
	}
	selector := NewAdaptiveSelector(AdaptiveSelectorConfig{Scheduling: cfg})
	t.Cleanup(selector.Stop)
	for _, age := range []int{-1, 0, 7, 14, 30, 45, 60, 90} {
		for i, id := range accountSpeedProfiles {
			t.Run(fmt.Sprintf("%s/%d", id, age), func(t *testing.T) {
				a := newAdaptiveClaudeAuth("profiles", "default_claude_max_5x", now.Add(-time.Duration(max(0, age))*24*time.Hour))
				if age < 0 {
					a.ClearAccountFirstProductionAt()
				}
				baseline := AccountWarmupStatusFor(a, now, cfg)
				a.SetAccountRateScale(3)
				if err := ApplyAccountLimitPatch(a, cfg, []byte(`{"rpm":40}`), []byte(`{"duration_minutes":15,"overrides":{"concurrency":4}}`), now); err != nil {
					t.Fatal(err)
				}
				controlPatch(t, a, fmt.Sprintf(`{"speed_profile":%q}`, id))
				if AccountRateScale(a, cfg) != 1 || len(storedLimitOverrides(a)) != 0 || storedTemporaryLimits(a) != nil {
					t.Fatal("profile did not clear old controls")
				}
				effective := AccountEffectiveLimits(a, now, cfg)
				live := pacingLimitsFor(selector, a, cfg, now)
				view := AccountWarmupControlFor(a, now, cfg)
				preview := view.Profiles[i]
				rpm, _ := selector.rateLimitParams(a, cfg, now)
				if preview.RPM != rpm || preview.DailyBudget != effective.DailyBudget || preview.Concurrency != effective.Concurrency || live.DailyRequests != effective.DailyBudget || live.Concurrency != effective.Concurrency || live.Config.RequestBurst != preview.RequestCapacity || live.Config.MaxActiveBindings != preview.MaxActiveBindings {
					t.Fatalf("projection diverged: preview=%+v effective=%+v live=%+v rpm=%v", preview, effective, live, rpm)
				}
				if !baseline.Mature {
					multiplier := []float64{.5, 1, 1.5, 2}[i]
					if effective.DailyBudget != scaleLimitInt(baseline.DailyBudget, multiplier) || effective.RPM != scaleLimitInt(baseline.RPMLimit, multiplier) || effective.Concurrency != max(1, min(4, int(math.Ceil(float64(baseline.ConcurrencyLimit)*multiplier)))) {
						t.Fatal("profile formula drift", effective, baseline)
					}
					if effective.TokenDailyBudget != resolveTokenDailyBudget(cfg, baseline) || live.DailyTokens != int64(effective.TokenDailyBudget) {
						t.Fatal("profile changed token budget")
					}
				} else {
					for _, p := range view.Profiles {
						if p.RPM != preview.RPM || p.DailyBudget != 0 || p.Concurrency != preview.Concurrency || p.RequestCapacity != preview.RequestCapacity || p.MaxActiveBindings != preview.MaxActiveBindings {
							t.Fatal("profile remained active after maturity", view)
						}
					}
				}
			})
		}
	}
}

func TestAccountWarmupControlPeriodBoundariesAndUnbounded(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	for _, period := range []int{1, 30, 120, 365} {
		for _, unbounded := range []bool{false, true} {
			cfg := internalconfig.DefaultAccountSchedulingConfig()
			if unbounded {
				cfg.WarmupCurve[len(cfg.WarmupCurve)-1].MaxAgeDays = 0
			}
			a := newAdaptiveClaudeAuth("period", "default_claude_max_5x", now)
			original := append([]internalconfig.AccountWarmupStage(nil), cfg.WarmupCurve...)
			anchor, _ := AuthFirstProductionAt(a)
			controlPatch(t, a, fmt.Sprintf(`{"warmup_period_days":%d}`, period))
			before := AccountWarmupStatusFor(a, now.Add(time.Duration(period)*24*time.Hour-time.Nanosecond), cfg)
			after := AccountWarmupStatusFor(a, now.Add(time.Duration(period)*24*time.Hour), cfg)
			if before.Mature || !after.Mature {
				t.Fatalf("period=%d unbounded=%v before=%+v after=%+v", period, unbounded, before, after)
			}
			if got, _ := AuthFirstProductionAt(a); !got.Equal(anchor) {
				t.Fatal("period changed real first-production anchor")
			}
			if !reflect.DeepEqual(cfg.WarmupCurve, original) {
				t.Fatal("period rewrote global curve")
			}
			view := AccountWarmupControlFor(a, now, cfg)
			if view.EffectivePeriodDays == nil || *view.EffectivePeriodDays != period {
				t.Fatal("effective period missing")
			}
		}
	}
	cfg := internalconfig.DefaultAccountSchedulingConfig()
	short := newAdaptiveClaudeAuth("half-day", "default_claude_max_5x", now)
	controlPatch(t, short, `{"warmup_period_days":30}`)
	if got := AccountWarmupStatusFor(short, now.Add(84*time.Hour), cfg); got.StageName != "w2" {
		t.Fatal("normalization discarded fractional real days", got)
	}
	a := newAdaptiveClaudeAuth("mapped", "default_claude_max_5x", now.Add(-14*24*time.Hour))
	controlPatch(t, a, `{"warmup_period_days":120}`)
	if got := AccountWarmupStatusFor(a, now, cfg); got.StageName != "w2" {
		t.Fatal("timeline not normalized", got)
	}
	controlPatch(t, a, `{"warmup_period_days":null}`)
	if got := AccountWarmupStatusFor(a, now, cfg); got.StageName != "w3-4" {
		t.Fatal("clearing period did not restore original timeline", got)
	}
	cfg.WarmupCurve[len(cfg.WarmupCurve)-1].MaxAgeDays = 0
	if view := AccountWarmupControlFor(a, now, cfg); view.EffectivePeriodDays != nil {
		t.Fatal("legacy unbounded curve advertised finite period")
	}
}

func TestAccountWarmupControlManualAutoHealth(t *testing.T) {
	now := time.Now()
	cfg := internalconfig.DefaultAccountSchedulingConfig()
	a := newAdaptiveClaudeAuth("manual", "default_claude_max_5x", now.Add(-10*24*time.Hour))
	anchor, _ := AuthFirstProductionAt(a)
	controlPatch(t, a, `{"speed_profile":"extreme","warmup_mode":"mature","warmup_period_days":120}`)
	if !AccountWarmupStatusFor(a, now, cfg).Mature {
		t.Fatal("healthy manual maturity ignored")
	}
	a.setHealthStageCap(0)
	view := AccountWarmupControlFor(a, now, cfg)
	if AccountWarmupStatusFor(a, now, cfg).Mature || !view.ManualMature {
		t.Fatal("manual override bypassed health cap")
	}
	for _, p := range view.Profiles {
		if p.RPM > 3 || p.DailyBudget > 200 || p.Concurrency > 1 || p.RequestCapacity > 8 || p.MaxActiveBindings > 1 {
			t.Fatal("profile bypassed health baseline", p)
		}
	}
	controlPatch(t, a, `{"warmup_mode":"auto"}`)
	if got, _ := AuthFirstProductionAt(a); !got.Equal(anchor) {
		t.Fatal("auto reset history")
	}
	if cap, ok := AccountHealthStageCap(a); !ok || cap != 0 {
		t.Fatal("auto cleared health history")
	}
	cold := &Auth{Provider: "claude"}
	controlPatch(t, cold, `{"warmup_mode":"mature"}`)
	if !AccountWarmupStatusFor(cold, now, cfg).Mature {
		t.Fatal("manual mature requires fabricated anchor")
	}
	cold.Quota.BackoffLevel = cfg.HealthGate.BackoffLevelThreshold
	if AccountWarmupStatusFor(cold, now, cfg).Mature {
		t.Fatal("manual mature bypassed distress without anchor")
	}
	evaluateAccountHealthGate(cold, false, now, cfg)
	if _, ok := AccountHealthStageCap(cold); !ok {
		t.Fatal("manual maturity lost new health distress")
	}
	cold.Quota.BackoffLevel = 0
	for i := 1; i <= len(cfg.WarmupCurve); i++ {
		evaluateAccountHealthGate(cold, true, now.Add(time.Duration(i)*promoteCooldownWindow(cfg)), cfg)
	}
	if !AccountWarmupStatusFor(cold, now.Add(24*time.Hour), cfg).Mature {
		t.Fatal("healthy manual maturity cannot recover")
	}
	controlPatch(t, cold, `{"warmup_mode":null}`)
	if AccountWarmupStatusFor(cold, now, cfg).Mature {
		t.Fatal("auto did not restore cold state")
	}
}

func TestAccountWarmupControlAtomicRefreshAndNoMint(t *testing.T) {
	m, s, a, cfg, now := pacingManagerFixture(t)
	*now = now.Add(time.Hour)
	before := m.WarmupTrafficPacingSnapshot(a.ID)
	for _, id := range []string{"high", "low", "extreme", "medium"} {
		updated, err := m.UpdateAccountScheduling(context.Background(), a.ID, a.Index, func(a *Auth, c internalconfig.AccountSchedulingConfig) error {
			controlPatch(t, a, fmt.Sprintf(`{"speed_profile":%q,"warmup_period_days":30,"warmup_mode":"auto"}`, id))
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		after := m.WarmupTrafficPacingSnapshot(a.ID)
		if *after.RequestBalance > *before.RequestBalance {
			t.Fatal("profile switch minted request credit", before.RequestBalance, after.RequestBalance)
		}
		before = after
		live := pacingLimitsFor(s, updated, cfg.AccountScheduling, *now)
		if live.Config.RequestBurst != *after.RequestCapacity || live.DailyRequests != *after.DailyRequestBudget {
			t.Fatal("live profile differs from GET")
		}
	}
	stale := a.Clone()
	stale.SetAccountRateScale(3)
	for _, register := range []bool{false, true} {
		var updated *Auth
		var err error
		if register {
			updated, err = m.Register(context.Background(), stale.Clone())
		} else {
			updated, err = m.Update(context.Background(), stale.Clone())
		}
		if err != nil || accountSpeedProfile(updated) != "medium" || AccountRateScale(updated, cfg.AccountScheduling) != 1 || accountWarmupPeriod(updated) == nil {
			t.Fatal("refresh reverted control", err)
		}
	}
	store := &failingPolicyStore{}
	mm := NewManager(store, nil, nil)
	mm.SetConfig(cfg)
	if _, err := mm.Register(context.Background(), a.Clone()); err != nil {
		t.Fatal(err)
	}
	store.fail = true
	_, err := mm.UpdateAccountScheduling(context.Background(), a.ID, "", func(a *Auth, c internalconfig.AccountSchedulingConfig) error {
		controlPatch(t, a, `{"speed_profile":"high","warmup_mode":"mature"}`)
		return nil
	})
	if !errors.Is(err, ErrAccountSchedulingPersist) {
		t.Fatal("persistence failure hidden", err)
	}
	current, _ := mm.GetByID(a.ID)
	if accountSpeedProfile(current) != "" || accountManualMature(current) {
		t.Fatal("failed patch published new controls")
	}
}

func TestAccountWarmupControlRejectInvalid(t *testing.T) {
	for _, raw := range []string{`{"speed_profile":"fast"}`, `{"speed_profile":""}`, `{"speed_profile":1}`, `{"speed_profile":"high","rate_scale":null}`, `{"speed_profile":null,"limit_overrides":{}}`, `{"speed_profile":"low","temporary_limits":null}`, `{"warmup_period_days":0}`, `{"warmup_period_days":366}`, `{"warmup_period_days":1.5}`, `{"warmup_period_days":"30"}`, `{"warmup_mode":"automatic"}`, `{"warmup_mode":false}`} {
		a := &Auth{Metadata: map[string]any{}}
		var req map[string]json.RawMessage
		_ = json.Unmarshal([]byte(raw), &req)
		if ApplyAccountWarmupControlPatch(a, req) == nil {
			t.Fatal("invalid control accepted", raw)
		}
		if len(a.Metadata) != 0 {
			t.Fatal("invalid control mutated metadata", raw)
		}
	}
}
