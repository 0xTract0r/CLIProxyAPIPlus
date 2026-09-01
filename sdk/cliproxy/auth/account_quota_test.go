package auth

import (
	"testing"
	"time"
)

func TestParseAccountQuotaUtilization_NilAndMissing(t *testing.T) {
	if _, ok := ParseAccountQuotaUtilization(nil); ok {
		t.Fatal("nil auth should report unknown (ok=false)")
	}

	if _, ok := ParseAccountQuotaUtilization(&Auth{Provider: "claude"}); ok {
		t.Fatal("auth with no Metadata should report unknown (ok=false)")
	}

	a := &Auth{Provider: "claude", Metadata: map[string]any{"other_key": "value"}}
	if _, ok := ParseAccountQuotaUtilization(a); ok {
		t.Fatal("auth with no quota_snapshot key should report unknown (ok=false)")
	}

	a = &Auth{Provider: "claude", Metadata: map[string]any{
		"quota_snapshot": map[string]any{"profile": map[string]any{"foo": "bar"}},
	}}
	if _, ok := ParseAccountQuotaUtilization(a); ok {
		t.Fatal("quota_snapshot with no usage key should report unknown (ok=false)")
	}

	a = &Auth{Provider: "claude", Metadata: map[string]any{
		"quota_snapshot": map[string]any{"usage": map[string]any{}},
	}}
	if _, ok := ParseAccountQuotaUtilization(a); ok {
		t.Fatal("empty usage object should report unknown (ok=false), not zero windows as known")
	}

	a = &Auth{Provider: "claude", Metadata: map[string]any{
		"quota_snapshot": map[string]any{
			"usage": map[string]any{
				"extra_usage": map[string]any{"is_enabled": false},
			},
		},
	}}
	if _, ok := ParseAccountQuotaUtilization(a); ok {
		t.Fatal("usage object with only a non-window sibling (extra_usage, no utilization field) should report unknown (ok=false)")
	}
}

func TestParseAccountQuotaUtilization_ClaudeShape(t *testing.T) {
	a := &Auth{
		Provider: "claude",
		Metadata: map[string]any{
			"quota_snapshot": map[string]any{
				"profile": map[string]any{"organization": map[string]any{"rate_limit_tier": "default_claude_max_20x"}},
				"usage": map[string]any{
					"five_hour":        map[string]any{"utilization": 8.0, "resets_at": "2026-01-22T09:00:00Z"},
					"seven_day":        map[string]any{"utilization": 77.0, "resets_at": "2026-01-22T19:00:00Z"},
					"seven_day_sonnet": map[string]any{"utilization": 0.0, "resets_at": "2026-01-25T00:00:00Z"},
					"extra_usage":      map[string]any{"is_enabled": false},
				},
			},
		},
	}

	util, ok := ParseAccountQuotaUtilization(a)
	if !ok {
		t.Fatal("expected ok=true for a well-formed Claude usage snapshot")
	}
	if util.Provider != "claude" {
		t.Fatalf("Provider = %q, want claude", util.Provider)
	}
	if len(util.Windows) != 3 {
		t.Fatalf("Windows = %d entries, want 3 (extra_usage must be excluded); got %+v", len(util.Windows), util.Windows)
	}

	fiveHour, ok := util.Windows["five_hour"]
	if !ok {
		t.Fatal("missing five_hour window")
	}
	if fiveHour.UtilizationPercent != 8.0 {
		t.Fatalf("five_hour utilization = %v, want 8.0", fiveHour.UtilizationPercent)
	}
	wantReset := time.Date(2026, 1, 22, 9, 0, 0, 0, time.UTC)
	if !fiveHour.ResetsAt.Equal(wantReset) {
		t.Fatalf("five_hour resets_at = %v, want %v", fiveHour.ResetsAt, wantReset)
	}
	wantHeadroom := 0.92
	if got := fiveHour.Headroom(); got < wantHeadroom-1e-9 || got > wantHeadroom+1e-9 {
		t.Fatalf("five_hour Headroom() = %v, want %v", got, wantHeadroom)
	}

	sevenDay, ok := util.Windows["seven_day"]
	if !ok {
		t.Fatal("missing seven_day window")
	}
	if sevenDay.UtilizationPercent != 77.0 {
		t.Fatalf("seven_day utilization = %v, want 77.0", sevenDay.UtilizationPercent)
	}

	if _, ok := util.Windows["extra_usage"]; ok {
		t.Fatal("extra_usage must not be parsed as a window (no utilization field)")
	}
}

func TestAccountQuotaHeadroom_TakesTightestWindow(t *testing.T) {
	a := &Auth{
		Provider: "claude",
		Metadata: map[string]any{
			"quota_snapshot": map[string]any{
				"usage": map[string]any{
					// seven_day is the binding (tightest headroom) window at 77% used.
					"five_hour": map[string]any{"utilization": 8.0, "resets_at": "2026-01-22T09:00:00Z"},
					"seven_day": map[string]any{"utilization": 77.0, "resets_at": "2026-01-22T19:00:00Z"},
				},
			},
		},
	}

	result, ok := AccountQuotaHeadroom(a)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if result.Window != "seven_day" {
		t.Fatalf("binding window = %q, want seven_day (the lower-headroom window)", result.Window)
	}
	wantHeadroom := 0.23
	if got := result.Headroom; got < wantHeadroom-1e-9 || got > wantHeadroom+1e-9 {
		t.Fatalf("Headroom = %v, want %v", got, wantHeadroom)
	}
	wantReset := time.Date(2026, 1, 22, 19, 0, 0, 0, time.UTC)
	if !result.ResetsAt.Equal(wantReset) {
		t.Fatalf("ResetsAt = %v, want %v", result.ResetsAt, wantReset)
	}
}

func TestAccountQuotaHeadroom_UnknownWhenNoSnapshot(t *testing.T) {
	if _, ok := AccountQuotaHeadroom(nil); ok {
		t.Fatal("nil auth should be unknown")
	}
	if _, ok := AccountQuotaHeadroom(&Auth{Provider: "gemini"}); ok {
		t.Fatal("auth with no quota_snapshot (e.g. a provider core does not poll quota for) should be unknown, not assumed-full-headroom")
	}
}

func TestAccountQuotaWindow_HeadroomClamping(t *testing.T) {
	over := AccountQuotaWindow{Name: "x", UtilizationPercent: 150}
	if got := over.Headroom(); got != 0 {
		t.Fatalf("over-100%% utilization should clamp Headroom to 0, got %v", got)
	}
	under := AccountQuotaWindow{Name: "x", UtilizationPercent: -10}
	if got := under.Headroom(); got != 1 {
		t.Fatalf("negative utilization should clamp Headroom to 1, got %v", got)
	}
}

func TestParseAccountQuotaUtilization_MalformedUtilizationIgnored(t *testing.T) {
	a := &Auth{
		Provider: "claude",
		Metadata: map[string]any{
			"quota_snapshot": map[string]any{
				"usage": map[string]any{
					// A non-numeric, non-numeric-string utilization value must not be
					// misread (e.g. as 0) -- the whole window entry is skipped.
					"bogus_window": map[string]any{"utilization": true, "resets_at": "2026-01-22T09:00:00Z"},
					"good_window":  map[string]any{"utilization": "42.5", "resets_at": "not-a-time"},
				},
			},
		},
	}

	util, ok := ParseAccountQuotaUtilization(a)
	if !ok {
		t.Fatal("expected ok=true (good_window is parseable)")
	}
	if _, ok := util.Windows["bogus_window"]; ok {
		t.Fatal("a window with a bool utilization value must be rejected, not coerced")
	}
	good, ok := util.Windows["good_window"]
	if !ok {
		t.Fatal("good_window with a numeric-string utilization should still parse")
	}
	if good.UtilizationPercent != 42.5 {
		t.Fatalf("good_window utilization = %v, want 42.5 (numeric string must parse)", good.UtilizationPercent)
	}
	if !good.ResetsAt.IsZero() {
		t.Fatalf("good_window resets_at should be zero-value for an unparseable timestamp, got %v", good.ResetsAt)
	}
}

func TestParseAccountQuotaUtilization_UtilizationClampedToPercentRange(t *testing.T) {
	a := &Auth{
		Provider: "claude",
		Metadata: map[string]any{
			"quota_snapshot": map[string]any{
				"usage": map[string]any{
					"over":  map[string]any{"utilization": 250.0},
					"under": map[string]any{"utilization": -50.0},
				},
			},
		},
	}
	util, ok := ParseAccountQuotaUtilization(a)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if got := util.Windows["over"].UtilizationPercent; got != 100 {
		t.Fatalf("over-range utilization should clamp to 100, got %v", got)
	}
	if got := util.Windows["under"].UtilizationPercent; got != 0 {
		t.Fatalf("under-range utilization should clamp to 0, got %v", got)
	}
}
