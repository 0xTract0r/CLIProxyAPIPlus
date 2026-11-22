package auth

import (
	"context"
	"math"
	"net/http"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func claudeSnapshotAuth(id string, used float64, reset time.Time) *Auth {
	return &Auth{ID: id, Provider: "claude", Metadata: map[string]any{
		"quota_snapshot": map[string]any{"usage": map[string]any{
			"seven_day": map[string]any{"utilization": used, "resets_at": reset.Format(time.RFC3339)},
		}},
	}}
}

func claudeSnapshotUsage(a *Auth) map[string]any {
	return a.Metadata["quota_snapshot"].(map[string]any)["usage"].(map[string]any)
}

func TestClaudeQuotaSnapshot_WindowScopeAndRecovery(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	reset := now.Add(72 * time.Hour)
	for _, tc := range []struct {
		window, model string
		blocked       bool
	}{
		{"five_hour", "claude-sonnet-4-6", true},
		{"seven_day", "claude-haiku-4-5", true},
		{"seven_day_oauth_apps", "claude-opus-4-6(high)", true},
		{"seven_day_sonnet", "claude-sonnet-4-6(high)", true},
		{"seven_day_sonnet", "claude-opus-4-6", false},
		{"seven_day_opus", "claude-opus-4-6[1m]", true},
		{"seven_day_opus", "claude-sonnet-4-6", false},
		{"seven_day_haiku", "claude-haiku-4-5", true},
		{"seven_day_unknown", "claude-sonnet-4-6", false},
		{"extra_usage", "claude-sonnet-4-6", false},
	} {
		t.Run(tc.window+"/"+tc.model, func(t *testing.T) {
			a := claudeSnapshotAuth("scope", 10, reset)
			claudeSnapshotUsage(a)[tc.window] = map[string]any{"utilization": 100.0, "resets_at": reset.Format(time.RFC3339)}
			blocked, reason, next := isAuthBlockedForModel(a, tc.model, now)
			if blocked != tc.blocked {
				t.Fatalf("blocked=%v want %v", blocked, tc.blocked)
			}
			if blocked && (reason != blockReasonCooldown || !next.Equal(reset)) {
				t.Fatalf("reason=%v next=%v", reason, next)
			}
			if blocked, _, _ := isAuthBlockedForModel(a, tc.model, reset); blocked {
				t.Fatal("account did not recover at reset")
			}
			if a.Disabled || a.Status == StatusDisabled {
				t.Fatal("quota filter disabled credential")
			}
		})
	}
	a := claudeSnapshotAuth("two-full", 100, reset)
	claudeSnapshotUsage(a)["five_hour"] = map[string]any{"utilization": 100.0, "resets_at": now.Add(time.Hour).Format(time.RFC3339)}
	if got, blocked := claudeQuotaSnapshotCooldown(a, "claude-sonnet-4-6", now); !blocked || !got.Equal(reset) {
		t.Fatalf("expected last exhausted reset: %v %v", got, blocked)
	}
	for _, bad := range []any{"invalid", math.NaN(), math.Inf(1)} {
		claudeSnapshotUsage(a)["seven_day"] = map[string]any{"utilization": bad, "resets_at": reset.Format(time.RFC3339)}
		delete(claudeSnapshotUsage(a), "five_hour")
		if _, blocked := claudeQuotaSnapshotCooldown(a, "claude-sonnet-4-6", now); blocked {
			t.Fatalf("invalid utilization blocked: %v", bad)
		}
	}
	a = claudeSnapshotAuth("no-reset", 100, reset)
	delete(claudeSnapshotUsage(a)["seven_day"].(map[string]any), "resets_at")
	if _, blocked := claudeQuotaSnapshotCooldown(a, "claude-sonnet-4-6", now); blocked {
		t.Fatal("unknown reset pinned account")
	}
}

func TestClaudeQuotaSnapshot_ExtraUsage(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name    string
		extra   map[string]any
		blocked bool
	}{
		{"disabled", map[string]any{"is_enabled": false}, true},
		{"available", map[string]any{"is_enabled": true, "monthly_limit": 1000.0, "used_credits": 100.0, "utilization": 10.0}, false},
		{"exhausted percent", map[string]any{"is_enabled": true, "utilization": 100.0}, true},
		{"exhausted balance", map[string]any{"is_enabled": true, "monthly_limit": 1000.0, "used_credits": 1000.0}, true},
		{"zero budget", map[string]any{"is_enabled": true, "monthly_limit": 0.0}, true},
		{"enabled unknown budget", map[string]any{"is_enabled": true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := claudeSnapshotAuth("extra", 100, now.Add(72*time.Hour))
			claudeSnapshotUsage(a)["extra_usage"] = tc.extra
			if _, blocked := claudeQuotaSnapshotCooldown(a, "claude-sonnet-4-6", now); blocked != tc.blocked {
				t.Fatalf("blocked=%v want %v", blocked, tc.blocked)
			}
		})
	}
}

func TestClaudeQuotaSnapshot_SelectorsAndScheduler(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	reset := now.Add(72 * time.Hour)
	model := "claude-sonnet-4-6"
	auths := []*Auth{
		claudeSnapshotAuth("claude-empty-a", 100, reset),
		claudeSnapshotAuth("claude-empty-b", 100, reset),
		claudeSnapshotAuth("claude-empty-c", 100, reset),
		claudeSnapshotAuth("claude-ready", 20, reset),
	}
	affinity := NewSessionAffinitySelector(&RoundRobinSelector{})
	adaptive := NewAdaptiveSelector(AdaptiveSelectorConfig{Scheduling: internalconfig.DefaultAccountSchedulingConfig(), SessionAffinity: true})
	defer adaptive.Stop()
	for _, tc := range []struct {
		name     string
		selector Selector
	}{
		{"round-robin", &RoundRobinSelector{}}, {"fill-first", &FillFirstSelector{}}, {"affinity", affinity}, {"adaptive-affinity", adaptive},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := cliproxyexecutor.Options{Headers: http.Header{"Session-Id": []string{"quota-session"}}}
			for _, provider := range []string{"claude", "mixed"} {
				affinity.cache.Set(provider+"::quota-session::"+model, "claude-empty-a")
				adaptive.cache.Set(provider+"::quota-session::"+model, "claude-empty-a")
				for i := 0; i < 8; i++ {
					got, err := tc.selector.Pick(context.Background(), provider, model, opts, auths)
					if err != nil || got == nil || got.ID != "claude-ready" {
						t.Fatalf("provider=%s picked %+v, err=%v", provider, got, err)
					}
				}
			}
		})
	}
	registerSchedulerModels(t, "claude", model, "claude-empty-a", "claude-empty-b", "claude-empty-c", "claude-ready")
	scheduler := newSchedulerForTest(&RoundRobinSelector{}, auths...)
	got, err := scheduler.pickSingle(context.Background(), "claude", model, cliproxyexecutor.Options{}, nil)
	if err != nil || got == nil || got.ID != "claude-ready" {
		t.Fatalf("scheduler picked %+v err=%v", got, err)
	}
	shard := scheduler.providers["claude"].modelShards[model]
	if shard.entries["claude-empty-a"].state != scheduledStateCooldown {
		t.Fatal("missing cooldown entry")
	}
	shard.promoteExpiredLocked(reset)
	if shard.entries["claude-empty-a"].state != scheduledStateReady {
		t.Fatal("reset did not restore cached entry")
	}
	scheduler.upsertAuth(claudeSnapshotAuth("claude-ready", 100, reset))
	if shard.entries["claude-ready"].state != scheduledStateCooldown {
		t.Fatal("new snapshot did not invalidate ready entry")
	}
	_, err = (&RoundRobinSelector{}).Pick(context.Background(), "claude", model, cliproxyexecutor.Options{}, auths[:3])
	if _, ok := err.(*modelCooldownError); !ok {
		t.Fatalf("expected pool cooldown, got %T %v", err, err)
	}
}
