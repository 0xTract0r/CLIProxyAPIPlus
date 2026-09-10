package auth

import (
	"net/http"
	"strconv"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

// These tests cover the harden-account-scheduling-limiter SECOND batch: P1b
// upstream real-time rate headers (overlay + realtime-preferred headroom + 429
// reset cooldown), P3 token-sink routing + budget resolution + quota-aware pacing,
// and the ERR full-jitter plan-quota cooldown. They are written to run alongside
// the first-batch tests in this package (harden_scheduling_limiter_test.go).

// ---------------------------------------------------------------------------
// P1b: real-time rate-limit header overlay
// ---------------------------------------------------------------------------

// TestRealtimeHeadroomPreferredOverSnapshot proves AccountQuotaHeadroom prefers the
// freshly-harvested real-time window over the persisted quota snapshot, and falls
// back to the snapshot when no real-time overlay exists.
func TestRealtimeHeadroomPreferredOverSnapshot(t *testing.T) {
	// AccountQuotaHeadroom uses the real wall clock internally (time.Now(); it is
	// the production selection read and takes no injected clock), so anchor this
	// case to time.Now() -- as the test's own note below requires -- rather than a
	// fixed historical instant, which would age past the realtime TTL and wrongly
	// fall back to the snapshot at run time.
	now := time.Now().UTC()
	authID := "rt-pref-1"
	defer realtimeRateStore.Delete(authID)

	a := &Auth{
		ID:       authID,
		Provider: "claude",
		Metadata: map[string]any{
			"quota_snapshot": map[string]any{
				"usage": map[string]any{
					"five_hour": map[string]any{
						"utilization": 50.0,
						"resets_at":   now.Add(time.Hour).Format(time.RFC3339),
					},
				},
			},
		},
	}

	// Snapshot-only view: 1 - 50/100 = 0.5.
	if snap, ok := accountQuotaHeadroomSnapshot(a); !ok || snap.Headroom < 0.49 || snap.Headroom > 0.51 {
		t.Fatalf("snapshot headroom = %+v ok=%v, want ~0.5", snap, ok)
	}

	h := http.Header{}
	h.Set("anthropic-ratelimit-unified-5h-utilization", "90")
	h.Set("anthropic-ratelimit-unified-5h-status", "allowed")
	h.Set("anthropic-ratelimit-unified-5h-reset", strconv.FormatInt(now.Add(3*time.Hour).Unix(), 10))
	IngestServingRateHeaders("claude", authID, h, now)

	rt, ok := accountRealtimeHeadroom(authID, now)
	if !ok || rt.Headroom < 0.09 || rt.Headroom > 0.11 {
		t.Fatalf("realtime headroom = %+v ok=%v, want ~0.1", rt, ok)
	}
	// AccountQuotaHeadroom uses time.Now() internally; the ingested window's reset is
	// 3h out and observed "now-ish", so it stays fresh for the length of this test.
	full, ok := AccountQuotaHeadroom(a)
	if !ok || full.Headroom > 0.2 {
		t.Fatalf("AccountQuotaHeadroom = %+v ok=%v, want realtime-preferred (~0.1)", full, ok)
	}
}

// TestRealtimeRejectedResetClampedToOneHour proves the 429 cooldown target derived
// from an exhausted real-time window is clamped to at most one hour out even when
// the upstream reports a multi-hour reset.
func TestRealtimeRejectedResetClampedToOneHour(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	authID := "rt-reject-1"
	defer realtimeRateStore.Delete(authID)

	h := http.Header{}
	h.Set("anthropic-ratelimit-unified-7d-utilization", "100")
	h.Set("anthropic-ratelimit-unified-7d-status", "rejected")
	h.Set("anthropic-ratelimit-unified-7d-reset", strconv.FormatInt(now.Add(5*time.Hour).Unix(), 10))
	IngestServingRateHeaders("claude", authID, h, now)

	reset, ok := accountRealtimeRejectedReset(authID, now)
	if !ok {
		t.Fatalf("expected a rejected-window reset")
	}
	if !reset.Equal(now.Add(time.Hour)) {
		t.Fatalf("reset = %v, want clamped to now+1h (%v)", reset, now.Add(time.Hour))
	}
}

// TestRealtimeStaleWindowIgnored proves a real-time window older than the TTL is
// ignored so selection falls back to the snapshot.
func TestRealtimeStaleWindowIgnored(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	authID := "rt-stale-1"
	defer realtimeRateStore.Delete(authID)

	h := http.Header{}
	h.Set("anthropic-ratelimit-unified-5h-utilization", "90")
	h.Set("anthropic-ratelimit-unified-5h-reset", strconv.FormatInt(now.Add(5*time.Hour).Unix(), 10))
	IngestServingRateHeaders("claude", authID, h, now.Add(-30*time.Minute))

	if _, ok := accountRealtimeHeadroom(authID, now); ok {
		t.Fatalf("expected stale real-time window (>TTL) to be ignored")
	}
}

// TestCodexRealtimeHeadersIngested proves the symmetric Codex header parse feeds the
// same overlay.
func TestCodexRealtimeHeadersIngested(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	authID := "rt-codex-1"
	defer realtimeRateStore.Delete(authID)

	h := http.Header{}
	h.Set("X-Codex-Primary-Used-Percent", "80")
	h.Set("X-Codex-Primary-Reset-After-Seconds", "1800")
	IngestServingRateHeaders("codex", authID, h, now)

	rt, ok := accountRealtimeHeadroom(authID, now)
	if !ok || rt.Headroom < 0.19 || rt.Headroom > 0.21 {
		t.Fatalf("codex realtime headroom = %+v ok=%v, want ~0.2", rt, ok)
	}
}

// ---------------------------------------------------------------------------
// P3: token-sink routing + budget resolution + pacing
// ---------------------------------------------------------------------------

// TestRecordAccountBillableTokensRoutesToSink proves the package-level sink routes a
// positive count and no-ops a non-positive count or a missing sink.
func TestRecordAccountBillableTokensRoutesToSink(t *testing.T) {
	prev := accountBillableTokenSink.Load()
	defer accountBillableTokenSink.Store(prev)

	var gotID string
	var gotTokens int
	RegisterAccountBillableTokenSink(func(id string, tokens int) {
		gotID = id
		gotTokens = tokens
	})
	RecordAccountBillableTokens("acct-x", 1234)
	if gotID != "acct-x" || gotTokens != 1234 {
		t.Fatalf("sink got (%q,%d), want (acct-x,1234)", gotID, gotTokens)
	}

	gotID, gotTokens = "", 0
	RecordAccountBillableTokens("acct-x", 0)
	if gotID != "" || gotTokens != 0 {
		t.Fatalf("expected no-op for non-positive tokens, got (%q,%d)", gotID, gotTokens)
	}

	RegisterAccountBillableTokenSink(nil)
	RecordAccountBillableTokens("acct-y", 5) // no sink registered -> no panic, no-op
}

// TestResolveTokenDailyBudget covers stage / cold / mature resolution.
func TestResolveTokenDailyBudget(t *testing.T) {
	cfg := internalconfig.AccountSchedulingConfig{
		WarmupCurve: []internalconfig.AccountWarmupStage{
			{Name: "w1", TokenDailyBudget: 1000},
			{Name: "w2", TokenDailyBudget: 5000},
		},
		MatureLimits: internalconfig.AccountMatureLimitsConfig{TokenDailyBudget: 0},
	}
	if got := resolveTokenDailyBudget(cfg, AccountWarmupStatus{StageName: "w2"}); got != 5000 {
		t.Fatalf("w2 token budget = %d, want 5000", got)
	}
	if got := resolveTokenDailyBudget(cfg, AccountWarmupStatus{StageName: "cold"}); got != 1000 {
		t.Fatalf("cold token budget = %d, want curve[0] (1000)", got)
	}
	if got := resolveTokenDailyBudget(cfg, AccountWarmupStatus{Mature: true}); got != 0 {
		t.Fatalf("mature token budget = %d, want 0 (unbounded)", got)
	}
}

// TestPacingRPMMultiplier proves: no burn history -> 1 (no throttle); a stale burn
// sample (older than the staleness window) -> fail safe to the pacing floor.
func TestPacingRPMMultiplier(t *testing.T) {
	s := NewAdaptiveSelector(AdaptiveSelectorConfig{})
	defer s.Stop()

	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	// No burn state -> multiplier 1.
	fresh := &Auth{ID: "warm-fresh", Provider: "claude", Metadata: map[string]any{}}
	if m := s.pacingRPMMultiplier(fresh, now); m != 1 {
		t.Fatalf("pacing multiplier for no-burn account = %v, want 1", m)
	}

	// Stale burn sample (30min old > 15min staleness) -> pacing floor.
	stale := &Auth{ID: "warm-stale", Provider: "claude", Metadata: map[string]any{}}
	setAccountSchedulingValue(stale.Metadata, accountSchedulingBurnPrevWindowKey, "5h")
	setAccountSchedulingValue(stale.Metadata, accountSchedulingBurnPrevUtilKey, 20.0)
	setAccountSchedulingValue(stale.Metadata, accountSchedulingBurnPrevAtKey, now.Add(-30*time.Minute).UTC().Format(time.RFC3339))
	if m := s.pacingRPMMultiplier(stale, now); m != PacingFactorFloor {
		t.Fatalf("pacing multiplier for stale-snapshot account = %v, want floor %v", m, PacingFactorFloor)
	}
}

// ---------------------------------------------------------------------------
// ERR: plan-quota full-jitter cooldown
// ---------------------------------------------------------------------------

// TestPlanQuotaCooldownJitterBounds proves the jittered cooldown always lands in
// [0.5, 1.0] x base and never escalates to zero.
func TestPlanQuotaCooldownJitterBounds(t *testing.T) {
	base := 8 * time.Second
	for i := 0; i < 500; i++ {
		got := planQuotaCooldownJitter(base)
		if got < base/2 || got > base {
			t.Fatalf("planQuotaCooldownJitter(%v) = %v, want in [%v, %v]", base, got, base/2, base)
		}
	}
	if got := planQuotaCooldownJitter(0); got != 0 {
		t.Fatalf("planQuotaCooldownJitter(0) = %v, want 0", got)
	}
}
