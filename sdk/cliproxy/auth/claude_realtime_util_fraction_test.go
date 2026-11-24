package auth

import (
	"math"
	"net/http"
	"strconv"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

// Regression tests for the Claude real-time utilization unit bug: the upstream
// anthropic-ratelimit-unified-<window>-utilization header is a 0-1 FRACTION
// (production: one account returned 0.62-0.66 while its quota snapshot read
// five_hour=62%; another returned 0.02). It used to be stored verbatim as a
// percentage, so 0.66 read as 0.66% and a busy account looked ~99% free, which
// made adaptive weighting favour the accounts already carrying traffic.

func approxWithin(got, want, eps float64) bool {
	return math.Abs(got-want) < eps
}

func ingestClaude5hUtilization(authID, value string, now time.Time) {
	h := http.Header{}
	h.Set("anthropic-ratelimit-unified-5h-utilization", value)
	h.Set("anthropic-ratelimit-unified-5h-status", "allowed")
	h.Set("anthropic-ratelimit-unified-5h-reset", strconv.FormatInt(now.Add(3*time.Hour).Unix(), 10))
	IngestServingRateHeaders("claude", authID, h, now)
}

// TestClaudeRealtimeUtilizationParsedAsFraction pins the production-observed
// header shapes to the expected percent and headroom.
func TestClaudeRealtimeUtilizationParsedAsFraction(t *testing.T) {
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		raw          string
		wantPercent  float64
		wantHeadroom float64
	}{
		{"0.66", 66, 0.34},
		{"0.02", 2, 0.98},
		{"1.0", 100, 0},
		{"0", 0, 1},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			authID := "rt-fraction-" + tc.raw
			defer realtimeRateStore.Delete(authID)

			ingestClaude5hUtilization(authID, tc.raw, now)

			store := realtimeRateFor(authID)
			store.mu.RLock()
			win, ok := store.windows["5h"]
			store.mu.RUnlock()
			if !ok {
				t.Fatalf("5h window not ingested")
			}
			if !approxWithin(win.UtilizationPercent, tc.wantPercent, 1e-9) {
				t.Fatalf("UtilizationPercent = %v, want %v (0-1 fraction scaled x100)", win.UtilizationPercent, tc.wantPercent)
			}

			rt, ok := accountRealtimeHeadroom(authID, now)
			if !ok {
				t.Fatalf("expected a fresh real-time headroom")
			}
			if !approxWithin(rt.Headroom, tc.wantHeadroom, 1e-9) {
				t.Fatalf("realtime headroom = %v, want ~%v", rt.Headroom, tc.wantHeadroom)
			}
			if rt.Window != "5h" {
				t.Fatalf("binding window = %q, want 5h", rt.Window)
			}
		})
	}
}

// TestClaudeRealtimeFractionPreferredByAccountQuotaHeadroom proves the selection
// read (AccountQuotaHeadroom) adopts the fraction-derived real-time value over a
// more optimistic snapshot.
func TestClaudeRealtimeFractionPreferredByAccountQuotaHeadroom(t *testing.T) {
	// AccountQuotaHeadroom reads time.Now() internally, so anchor to the wall clock.
	now := time.Now().UTC()
	authID := "rt-fraction-pref"
	defer realtimeRateStore.Delete(authID)

	a := &Auth{
		ID:       authID,
		Provider: "claude",
		Metadata: map[string]any{
			"quota_snapshot": map[string]any{
				"usage": map[string]any{
					"five_hour": map[string]any{
						"utilization": 10.0,
						"resets_at":   now.Add(time.Hour).Format(time.RFC3339),
					},
				},
			},
		},
	}
	if snap, ok := accountQuotaHeadroomSnapshot(a); !ok || !approxWithin(snap.Headroom, 0.9, 1e-9) {
		t.Fatalf("snapshot headroom = %+v ok=%v, want ~0.9", snap, ok)
	}

	ingestClaude5hUtilization(authID, "0.66", now)

	got, ok := AccountQuotaHeadroom(a)
	if !ok {
		t.Fatalf("AccountQuotaHeadroom ok=false")
	}
	if !approxWithin(got.Headroom, 0.34, 1e-9) || got.Window != "5h" {
		t.Fatalf("AccountQuotaHeadroom = %+v, want real-time 5h headroom ~0.34", got)
	}
}

// TestClaudeRealtimeFractionLowersSelectionWeight proves the weight layer now
// ranks a busy account (5h at 0.66) well below an equally-tiered quiet one (5h at
// 0.02), and below an idle account that only has a 10% snapshot -- the exact
// production inversion the unit bug caused (busy read ~0.993 headroom and
// out-weighted the snapshot-only idle account at 0.9).
func TestClaudeRealtimeFractionLowersSelectionWeight(t *testing.T) {
	cfg := internalconfig.DefaultAccountSchedulingConfig()
	// The overlay freshness check inside AccountQuotaHeadroom uses time.Now(), so
	// ingest at the wall clock; the freshness axis uses accountWeightTestNow.
	ingestAt := time.Now().UTC()

	busy := matureAuth("default_claude_max_20x", 10)
	busy.ID = "rt-fraction-weight-busy"
	quiet := matureAuth("default_claude_max_20x", 10)
	quiet.ID = "rt-fraction-weight-quiet"
	idle := matureAuth("default_claude_max_20x", 10)
	idle.ID = "rt-fraction-weight-idle" // snapshot only, no real-time overlay
	defer realtimeRateStore.Delete(busy.ID)
	defer realtimeRateStore.Delete(quiet.ID)
	defer realtimeRateStore.Delete(idle.ID)

	ingestClaude5hUtilization(busy.ID, "0.66", ingestAt)
	ingestClaude5hUtilization(quiet.ID, "0.02", ingestAt)

	busyW := AccountSelectionWeight(busy, cfg, accountWeightTestNow)
	quietW := AccountSelectionWeight(quiet, cfg, accountWeightTestNow)
	idleW := AccountSelectionWeight(idle, cfg, accountWeightTestNow)

	base := internalconfig.DefaultAccountTierWeightClaudeMax20x
	if !approxWithin(busyW, base*0.34, 1e-9) {
		t.Fatalf("busy weight = %v, want ~%v", busyW, base*0.34)
	}
	if !approxWithin(quietW, base*0.98, 1e-9) {
		t.Fatalf("quiet weight = %v, want ~%v", quietW, base*0.98)
	}
	if !(busyW < quietW*0.5) {
		t.Fatalf("busy weight %v should be well below quiet weight %v", busyW, quietW)
	}
	if !(busyW < idleW) {
		t.Fatalf("busy weight %v should be below snapshot-only idle weight %v", busyW, idleW)
	}
}
