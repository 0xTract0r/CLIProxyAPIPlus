package auth

import (
	"context"
	"math"
	"net/http"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func codexSnapshotAuth(id string, used float64, reset time.Time) *Auth {
	return &Auth{ID: id, Provider: "codex", Metadata: map[string]any{
		"quota_snapshot": map[string]any{"usage": map[string]any{
			"rate_limit": map[string]any{"primary_window": map[string]any{
				"used_percent": used, "reset_at": float64(reset.Unix()),
			}},
		}},
	}}
}

func codexSnapshotUsage(a *Auth) map[string]any {
	return a.Metadata["quota_snapshot"].(map[string]any)["usage"].(map[string]any)
}

func TestCodexQuotaSnapshot_ParsesSharedWindows(t *testing.T) {
	reset := time.Now().Add(72 * time.Hour).Truncate(time.Second)
	a := codexSnapshotAuth("codex-parse", 15, reset)
	usage := codexSnapshotUsage(a)
	usage["rate_limit"].(map[string]any)["secondary_window"] = map[string]any{"used_percent": "100", "reset_at": reset.Unix()}
	usage["code_review_rate_limit"] = map[string]any{"primary_window": map[string]any{"used_percent": 100}}
	utilization, ok := ParseAccountQuotaUtilization(a)
	if !ok || len(utilization.Windows) != 2 {
		t.Fatalf("shared windows: %+v, known=%v", utilization, ok)
	}
	secondary := utilization.Windows["codex_secondary"]
	if secondary.UtilizationPercent != 100 || !secondary.ResetsAt.Equal(reset) {
		t.Fatalf("secondary window: %+v", secondary)
	}
	if got, blocked := codexQuotaSnapshotCooldown(a, time.Now()); !blocked || !got.Equal(reset) {
		t.Fatalf("secondary-only exhaustion must block: %v %v", got, blocked)
	}
	headroom, ok := AccountQuotaHeadroom(a)
	if !ok || headroom.Headroom != 0 || headroom.Window != "codex_secondary" {
		t.Fatalf("tightest headroom: %+v, known=%v", headroom, ok)
	}
}

func TestCodexQuotaSnapshot_CooldownBoundaries(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	reset := now.Add(72 * time.Hour)
	for _, tc := range []struct {
		name    string
		mutate  func(*Auth)
		blocked bool
	}{
		{"exhausted", func(*Auth) {}, true},
		{"partial", func(a *Auth) {
			codexSnapshotUsage(a)["rate_limit"].(map[string]any)["primary_window"].(map[string]any)["used_percent"] = 99.0
		}, false},
		{"reset passed", func(a *Auth) {
			codexSnapshotUsage(a)["rate_limit"].(map[string]any)["primary_window"].(map[string]any)["reset_at"] = float64(now.Unix())
		}, false},
		{"missing reset", func(a *Auth) {
			delete(codexSnapshotUsage(a)["rate_limit"].(map[string]any)["primary_window"].(map[string]any), "reset_at")
		}, false},
		{"unknown snapshot", func(a *Auth) { a.Metadata = nil }, false},
		{"invalid percent", func(a *Auth) {
			codexSnapshotUsage(a)["rate_limit"].(map[string]any)["primary_window"].(map[string]any)["used_percent"] = "bad"
		}, false},
		{"NaN percent", func(a *Auth) {
			codexSnapshotUsage(a)["rate_limit"].(map[string]any)["primary_window"].(map[string]any)["used_percent"] = math.NaN()
		}, false},
		{"infinite percent", func(a *Auth) {
			codexSnapshotUsage(a)["rate_limit"].(map[string]any)["primary_window"].(map[string]any)["used_percent"] = math.Inf(1)
		}, false},
		{"other provider", func(a *Auth) { a.Provider = "claude" }, false},
		{"credit overflow", func(a *Auth) { codexSnapshotUsage(a)["credits"] = map[string]any{"has_credits": true, "balance": "5"} }, false},
		{"unlimited credits", func(a *Auth) {
			codexSnapshotUsage(a)["credits"] = map[string]any{"has_credits": true, "unlimited": true}
		}, false},
		{"empty credits", func(a *Auth) { codexSnapshotUsage(a)["credits"] = map[string]any{"has_credits": true, "balance": "0"} }, true},
		{"infinite credits", func(a *Auth) {
			codexSnapshotUsage(a)["credits"] = map[string]any{"has_credits": true, "balance": "Inf"}
		}, true},
		{"limit reached despite credits", func(a *Auth) {
			codexSnapshotUsage(a)["credits"] = map[string]any{"has_credits": true, "balance": "5"}
			codexSnapshotUsage(a)["rate_limit"].(map[string]any)["limit_reached"] = true
		}, true},
		{"upstream denies credits", func(a *Auth) {
			codexSnapshotUsage(a)["credits"] = map[string]any{"has_credits": true, "balance": "5"}
			codexSnapshotUsage(a)["rate_limit"].(map[string]any)["allowed"] = false
		}, true},
		// Explicit upstream allow signals (both present as booleans) take
		// precedence over used_percent. This only lifts the proactive snapshot
		// filter; the passive cooldown from exhausted success response headers
		// is a separate layer outside this change, covered by
		// TestManager_ExecuteSuccessCodexExhaustedHeaderCoolsAuth and
		// TestManager_StreamSuccessCodexExhaustedHeaderCoolsAuth.
		{"upstream explicitly allows", func(a *Auth) {
			limits := codexSnapshotUsage(a)["rate_limit"].(map[string]any)
			limits["allowed"], limits["limit_reached"] = true, false
		}, false},
		{"upstream explicitly allows without credits", func(a *Auth) {
			codexSnapshotUsage(a)["credits"] = map[string]any{"has_credits": false, "unlimited": false, "balance": "0"}
			limits := codexSnapshotUsage(a)["rate_limit"].(map[string]any)
			limits["allowed"], limits["limit_reached"] = true, false
		}, false},
		{"upstream explicitly allows exhausted secondary", func(a *Auth) {
			limits := codexSnapshotUsage(a)["rate_limit"].(map[string]any)
			limits["secondary_window"] = map[string]any{"used_percent": 100.0, "reset_at": float64(reset.Unix())}
			limits["allowed"], limits["limit_reached"] = true, false
		}, false},
		{"upstream denies", func(a *Auth) {
			limits := codexSnapshotUsage(a)["rate_limit"].(map[string]any)
			limits["allowed"], limits["limit_reached"] = false, false
		}, true},
		{"upstream limit reached", func(a *Auth) {
			limits := codexSnapshotUsage(a)["rate_limit"].(map[string]any)
			limits["allowed"], limits["limit_reached"] = true, true
		}, true},
		{"allow signals missing", func(a *Auth) {
			limits := codexSnapshotUsage(a)["rate_limit"].(map[string]any)
			delete(limits, "allowed")
			delete(limits, "limit_reached")
		}, true},
		{"only allowed present", func(a *Auth) { codexSnapshotUsage(a)["rate_limit"].(map[string]any)["allowed"] = true }, true},
		{"only limit_reached present", func(a *Auth) {
			codexSnapshotUsage(a)["rate_limit"].(map[string]any)["limit_reached"] = false
		}, true},
		{"non-boolean allow signals", func(a *Auth) {
			limits := codexSnapshotUsage(a)["rate_limit"].(map[string]any)
			limits["allowed"], limits["limit_reached"] = "true", "false"
		}, true},
		{"limit reached without exhausted window", func(a *Auth) {
			limits := codexSnapshotUsage(a)["rate_limit"].(map[string]any)
			limits["primary_window"].(map[string]any)["used_percent"] = 80.0
			limits["limit_reached"] = true
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := codexSnapshotAuth("codex-boundary", 100, reset)
			tc.mutate(a)
			blocked, reason, next := isAuthBlockedForModel(a, "gpt-other", now)
			if blocked != tc.blocked {
				t.Fatalf("blocked=%v, want %v", blocked, tc.blocked)
			}
			if blocked && (reason != blockReasonCooldown || !next.Equal(reset)) {
				t.Fatalf("reason=%v next=%v", reason, next)
			}
			if a.Disabled || a.Status == StatusDisabled {
				t.Fatal("quota filter disabled credential")
			}
		})
	}
	a := codexSnapshotAuth("two-exhausted", 100, now.Add(time.Hour))
	codexSnapshotUsage(a)["rate_limit"].(map[string]any)["secondary_window"] = map[string]any{"used_percent": 100.0, "reset_at": float64(reset.Unix())}
	if got, blocked := codexQuotaSnapshotCooldown(a, now); !blocked || !got.Equal(reset) {
		t.Fatalf("must wait for all exhausted windows: %v %v", got, blocked)
	}
	if _, blocked := codexQuotaSnapshotCooldown(a, reset); blocked {
		t.Fatal("must recover automatically at final reset")
	}
}

func TestCodexQuotaSnapshot_SelectorsSkipThreeExhaustedAccounts(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name     string
		selector Selector
	}{
		{"round-robin", &RoundRobinSelector{}},
		{"fill-first", &FillFirstSelector{}},
		{"session-affinity", NewSessionAffinitySelector(&RoundRobinSelector{})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			auths := []*Auth{
				codexSnapshotAuth("a-empty", 100, now.Add(24*time.Hour)),
				codexSnapshotAuth("b-empty", 100, now.Add(48*time.Hour)),
				codexSnapshotAuth("c-empty", 100, now.Add(72*time.Hour)),
				codexSnapshotAuth("d-ready", 75, now.Add(96*time.Hour)),
			}
			opts := cliproxyexecutor.Options{Headers: http.Header{"Session-Id": []string{"existing-session"}}}
			for _, route := range []struct{ provider, model string }{
				{"codex", "gpt-model-a"}, {"codex", "gpt-model-b"}, {"codex", "gpt-model-b(high)"},
				{"mixed", "gpt-model-b"},
			} {
				provider, model := route.provider, route.model
				if affinity, ok := tc.selector.(*SessionAffinitySelector); ok {
					affinity.cache.Set(provider+"::existing-session::"+model, "a-empty")
				}
				for i := 0; i < 12; i++ {
					got, err := tc.selector.Pick(context.Background(), provider, model, opts, auths)
					if err != nil || got == nil || got.ID != "d-ready" {
						t.Fatalf("model %s picked %+v, err=%v", model, got, err)
					}
				}
			}
		})
	}
}

func TestCodexQuotaSnapshot_SchedulerRefreshAndRecovery(t *testing.T) {
	model := "gpt-codex-quota-snapshot"
	registerSchedulerModels(t, "codex", model, "snapshot-empty", "snapshot-ready")
	now := time.Now().Truncate(time.Second)
	reset := now.Add(72 * time.Hour)
	empty := codexSnapshotAuth("snapshot-empty", 100, reset)
	ready := codexSnapshotAuth("snapshot-ready", 75, reset)
	scheduler := newSchedulerForTest(&RoundRobinSelector{}, empty, ready)
	for i := 0; i < 8; i++ {
		got, err := scheduler.pickSingle(context.Background(), "codex", model, cliproxyexecutor.Options{}, nil)
		if err != nil || got == nil || got.ID != ready.ID {
			t.Fatalf("scheduler picked %+v, err=%v", got, err)
		}
	}
	shard := scheduler.providers["codex"].modelShards[model]
	if shard.entries[empty.ID].state != scheduledStateCooldown {
		t.Fatal("exhausted account is not in cooldown queue")
	}
	shard.promoteExpiredLocked(reset)
	if shard.entries[empty.ID].state != scheduledStateReady {
		t.Fatal("scheduler did not restore account at reset")
	}
	// A quota refresh must invalidate the cached ready list immediately.
	scheduler.upsertAuth(codexSnapshotAuth(ready.ID, 100, reset))
	if shard.entries[ready.ID].state != scheduledStateCooldown {
		t.Fatal("fresh exhaustion did not invalidate ready entry")
	}
	scheduler.upsertAuth(codexSnapshotAuth(ready.ID, 10, reset))
	if shard.entries[ready.ID].state != scheduledStateReady {
		t.Fatal("fresh headroom did not restore entry")
	}
}

func TestCodexQuotaSnapshot_AllExhaustedReturnsCooldown(t *testing.T) {
	reset := time.Now().Add(72 * time.Hour)
	_, err := (&RoundRobinSelector{}).Pick(context.Background(), "codex", "new-model", cliproxyexecutor.Options{}, []*Auth{codexSnapshotAuth("all-empty", 100, reset)})
	cooldown, ok := err.(*modelCooldownError)
	if !ok || cooldown.StatusCode() != http.StatusTooManyRequests || cooldown.Headers().Get("Retry-After") == "" {
		t.Fatalf("expected retryable cooldown, got %T %v", err, err)
	}
}
