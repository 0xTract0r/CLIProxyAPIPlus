package auth

import (
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

// nsRateScale returns an account_scheduling metadata object carrying only a
// rate_scale override, for the per-account override tests below.
func nsRateScale(v any) map[string]any {
	return map[string]any{AccountSchedulingMetadataKey: map[string]any{accountSchedulingRateScaleKey: v}}
}

func TestAccountRateScale_Resolution(t *testing.T) {
	cfgDefault := internalconfig.DefaultAccountSchedulingConfig() // RateScale 1.0
	cfgHalf := cfgDefault
	cfgHalf.RateScale = 0.5

	tests := []struct {
		name string
		auth *Auth
		cfg  internalconfig.AccountSchedulingConfig
		want float64
	}{
		{name: "nil auth, default config -> 1.0", auth: nil, cfg: cfgDefault, want: 1.0},
		{name: "nil auth, zero config -> 1.0", auth: nil, cfg: internalconfig.AccountSchedulingConfig{}, want: 1.0},
		{name: "config default only", auth: &Auth{}, cfg: cfgHalf, want: 0.5},
		{
			name: "metadata override (float) beats config default",
			auth: &Auth{Metadata: nsRateScale(0.25)},
			cfg:  cfgHalf,
			want: 0.25,
		},
		{
			name: "metadata override as numeric string",
			auth: &Auth{Metadata: nsRateScale("0.75")},
			cfg:  cfgHalf,
			want: 0.75,
		},
		{
			name: "metadata override as int",
			auth: &Auth{Metadata: nsRateScale(2)},
			cfg:  cfgDefault,
			want: 2,
		},
		{
			name: "legacy bare rate_scale key honored (dual-read)",
			auth: &Auth{Metadata: map[string]any{accountSchedulingRateScaleKey: 0.3}},
			cfg:  cfgDefault,
			want: 0.3,
		},
		{
			name: "invalid metadata string falls back to config default",
			auth: &Auth{Metadata: nsRateScale("abc")},
			cfg:  cfgHalf,
			want: 0.5,
		},
		{
			name: "non-positive metadata value ignored, falls back to config default",
			auth: &Auth{Metadata: nsRateScale(0)},
			cfg:  cfgHalf,
			want: 0.5,
		},
		{
			name: "negative metadata value ignored, falls back to 1.0 when config unset",
			auth: &Auth{Metadata: nsRateScale(-1.0)},
			cfg:  internalconfig.AccountSchedulingConfig{},
			want: 1.0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := AccountRateScale(tc.auth, tc.cfg); got != tc.want {
				t.Fatalf("AccountRateScale() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestScaleLimitInt(t *testing.T) {
	tests := []struct {
		limit int
		scale float64
		want  int
	}{
		{limit: 0, scale: 0.5, want: 0},   // 0 = unbounded/unset, preserved
		{limit: -3, scale: 0.5, want: -3}, // non-positive preserved
		{limit: 4, scale: 1.0, want: 4},   // no-op
		{limit: 4, scale: 0.5, want: 2},
		{limit: 200, scale: 0.5, want: 100},
		{limit: 1, scale: 0.5, want: 1}, // round(0.5)=1, floor keeps 1
		{limit: 1, scale: 0.4, want: 1}, // round(0.4)=0 -> floored to 1
		{limit: 3, scale: 0.1, want: 1}, // round(0.3)=0 -> floored to 1
		{limit: 2, scale: 2.0, want: 4}, // >1 scale lifts
		{limit: 3, scale: 0.5, want: 2}, // round(1.5)=2
	}
	for _, tc := range tests {
		if got := scaleLimitInt(tc.limit, tc.scale); got != tc.want {
			t.Fatalf("scaleLimitInt(%d, %v) = %d, want %d", tc.limit, tc.scale, got, tc.want)
		}
	}
}

func TestScaleLimitRPM(t *testing.T) {
	tests := []struct {
		rpm   float64
		scale float64
		want  float64
	}{
		{rpm: 0, scale: 0.5, want: 0},     // no ceiling preserved
		{rpm: -1, scale: 0.5, want: -1},   // non-positive preserved
		{rpm: 45, scale: 1.0, want: 45},   // no-op
		{rpm: 45, scale: 0.5, want: 22.5}, // halves
		{rpm: 3, scale: 0.5, want: 1.5},
		{rpm: 10, scale: 2.0, want: 20}, // >1 lifts
	}
	for _, tc := range tests {
		if got := scaleLimitRPM(tc.rpm, tc.scale); got != tc.want {
			t.Fatalf("scaleLimitRPM(%v, %v) = %v, want %v", tc.rpm, tc.scale, got, tc.want)
		}
	}
}

// TestRateLimitParams_RateScale asserts the selector's derived rpm/burst are
// scaled by the per-account rate_scale AFTER the tier/warm-up derivation (§8.3):
// a mature account's (45,10) halves to (22.5,5), and a warming w1 account's
// (3,1) drops to (1.5,1) with the burst floored at 1.
func TestRateLimitParams_RateScale(t *testing.T) {
	cfg := internalconfig.DefaultAccountSchedulingConfig()
	s := NewAdaptiveSelector(AdaptiveSelectorConfig{Scheduling: cfg}, WithAdaptiveClock(fixedClock()))
	defer s.Stop()

	withScale := func(a *Auth, v float64) *Auth {
		a.Metadata[AccountSchedulingMetadataKey] = map[string]any{accountSchedulingRateScaleKey: v}
		return a
	}

	t.Run("mature unscaled", func(t *testing.T) {
		a := newAdaptiveClaudeAuth("m-1", "default_claude_max_20x", matureFirstProd())
		rpm, burst := s.rateLimitParams(a, cfg, adaptiveTestNow)
		if rpm != 45 || burst != 10 {
			t.Fatalf("(rpm,burst) = (%v,%d), want (45,10)", rpm, burst)
		}
	})
	t.Run("mature scaled 0.5", func(t *testing.T) {
		a := withScale(newAdaptiveClaudeAuth("m-2", "default_claude_max_20x", matureFirstProd()), 0.5)
		rpm, burst := s.rateLimitParams(a, cfg, adaptiveTestNow)
		if rpm != 22.5 || burst != 5 {
			t.Fatalf("(rpm,burst) = (%v,%d), want (22.5,5)", rpm, burst)
		}
	})
	t.Run("warming w1 scaled 0.5 floors burst at 1", func(t *testing.T) {
		a := withScale(newAdaptiveClaudeAuth("w-1", "default_claude_max_20x", warmupFirstProd()), 0.5)
		rpm, burst := s.rateLimitParams(a, cfg, adaptiveTestNow)
		if rpm != 1.5 || burst != 1 {
			t.Fatalf("(rpm,burst) = (%v,%d), want (1.5,1)", rpm, burst)
		}
	})
}

// TestOverDailyBudget_RateScale asserts the warm-up daily budget is scaled by
// rate_scale: a w1 account's 200/day budget at rate_scale 0.5 trips at 100
// recorded requests, not 200.
func TestOverDailyBudget_RateScale(t *testing.T) {
	cfg := internalconfig.DefaultAccountSchedulingConfig()
	gate := NewAccountConcurrencyGate(WithGateClock(fixedClock()))
	s := NewAdaptiveSelector(
		AdaptiveSelectorConfig{Scheduling: cfg},
		WithAdaptiveClock(fixedClock()),
		WithAdaptiveAccountGate(gate),
	)
	defer s.Stop()

	a := newAdaptiveClaudeAuth("w-budget", "default_claude_max_20x", warmupFirstProd())
	a.Metadata[AccountSchedulingMetadataKey] = map[string]any{accountSchedulingRateScaleKey: 0.5}

	// Scaled budget = 200 * 0.5 = 100. 99 recorded -> not over; the 100th -> over.
	for i := 0; i < 99; i++ {
		gate.RecordRequest(a.ID)
	}
	if s.overDailyBudget(a, cfg, adaptiveTestNow) {
		t.Fatalf("over daily budget at 99 recorded, want under scaled budget 100")
	}
	gate.RecordRequest(a.ID) // 100th
	if !s.overDailyBudget(a, cfg, adaptiveTestNow) {
		t.Fatalf("not over daily budget at 100 recorded, want over scaled budget 100")
	}

	// A companion account with the DEFAULT rate_scale (1.0) is NOT over budget at
	// 100 -- confirming the scaling is per-account, not global.
	b := newAdaptiveClaudeAuth("w-budget-unscaled", "default_claude_max_20x", warmupFirstProd())
	for i := 0; i < 100; i++ {
		gate.RecordRequest(b.ID)
	}
	if s.overDailyBudget(b, cfg, adaptiveTestNow) {
		t.Fatalf("unscaled account over budget at 100, want under default budget 200")
	}
}
