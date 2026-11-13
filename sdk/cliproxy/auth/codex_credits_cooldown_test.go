package auth

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// These tests guard the Codex credits-aware cooldown: a successful Codex response
// whose plan window is exhausted (used-percent >= 100) but whose credit headers
// report usable credits must NOT proactively cool the account, because the
// upstream keeps serving it from the credit balance.

func withCodexCredits(headers http.Header, hasCredits, balance, unlimited string) http.Header {
	if hasCredits != "" {
		headers.Set("X-Codex-Credits-Has-Credits", hasCredits)
	}
	if balance != "" {
		headers.Set("X-Codex-Credits-Balance", balance)
	}
	if unlimited != "" {
		headers.Set("X-Codex-Credits-Unlimited", unlimited)
	}
	return headers
}

func TestCodexCreditsUsable(t *testing.T) {
	cases := []struct {
		name       string
		hasCredits string
		balance    string
		unlimited  string
		want       bool
	}{
		{name: "production shape True with positive balance", hasCredits: "True", balance: "59897.4803850000", unlimited: "False", want: true},
		{name: "lowercase true with positive balance", hasCredits: "true", balance: "1", want: true},
		{name: "has credits False", hasCredits: "False", balance: "59897.48", unlimited: "False", want: false},
		{name: "zero balance not unlimited", hasCredits: "True", balance: "0", unlimited: "False", want: false},
		{name: "negative balance", hasCredits: "True", balance: "-5", want: false},
		{name: "unlimited without balance", hasCredits: "True", unlimited: "True", want: true},
		{name: "no credit headers", want: false},
		{name: "non-numeric balance", hasCredits: "True", balance: "lots", unlimited: "False", want: false},
		{name: "NaN balance", hasCredits: "True", balance: "NaN", want: false},
		{name: "has credits without balance or unlimited", hasCredits: "True", want: false},
		{name: "unlimited but has credits missing", unlimited: "True", balance: "10", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			headers := withCodexCredits(http.Header{}, tc.hasCredits, tc.balance, tc.unlimited)
			if got := codexCreditsUsable(headers); got != tc.want {
				t.Fatalf("codexCreditsUsable() = %v, want %v (headers=%v)", got, tc.want, headers)
			}
		})
	}
	if codexCreditsUsable(nil) {
		t.Fatal("codexCreditsUsable(nil) = true, want false")
	}
}

func TestQuotaRetryAfterFromHeaders_CreditsAware(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	resetAt := now.Add(5 * 24 * time.Hour)

	t.Run("exhausted with usable credits skips cooldown", func(t *testing.T) {
		headers := withCodexCredits(exhaustedCodexHeaders(resetAt), "True", "59897.4803850000", "False")
		if got := quotaRetryAfterFromHeaders("codex", headers, now); got != nil {
			t.Fatalf("quotaRetryAfterFromHeaders() = %v, want nil when credits are usable", *got)
		}
	})

	t.Run("exhausted without credit headers still cools", func(t *testing.T) {
		got := quotaRetryAfterFromHeaders("codex", exhaustedCodexHeaders(resetAt), now)
		if got == nil || *got != resetAt.Sub(now) {
			t.Fatalf("quotaRetryAfterFromHeaders() = %v, want %v", got, resetAt.Sub(now))
		}
	})

	t.Run("exhausted with has-credits False still cools", func(t *testing.T) {
		headers := withCodexCredits(exhaustedCodexHeaders(resetAt), "False", "59897.48", "False")
		got := quotaRetryAfterFromHeaders("codex", headers, now)
		if got == nil || *got != resetAt.Sub(now) {
			t.Fatalf("quotaRetryAfterFromHeaders() = %v, want %v", got, resetAt.Sub(now))
		}
	})

	t.Run("bengalfox exhausted with usable credits still cools", func(t *testing.T) {
		headers := http.Header{}
		headers.Set("X-Codex-Primary-Used-Percent", "40")
		headers.Set("X-Codex-Bengalfox-Primary-Used-Percent", "100")
		headers.Set("X-Codex-Bengalfox-Primary-Reset-At", strconv.FormatInt(resetAt.Unix(), 10))
		headers = withCodexCredits(headers, "True", "59897.48", "False")
		got := quotaRetryAfterFromHeaders("codex", headers, now)
		if got == nil || *got != resetAt.Sub(now) {
			t.Fatalf("quotaRetryAfterFromHeaders() = %v, want %v (credits do not cover the Bengalfox bucket)", got, resetAt.Sub(now))
		}
	})

	t.Run("main and bengalfox exhausted with credits keeps only the bengalfox reset", func(t *testing.T) {
		bengalfoxReset := now.Add(3 * time.Hour)
		headers := withCodexCredits(exhaustedCodexHeaders(resetAt), "True", "59897.48", "False")
		headers.Set("X-Codex-Bengalfox-Secondary-Used-Percent", "100")
		headers.Set("X-Codex-Bengalfox-Secondary-Reset-At", strconv.FormatInt(bengalfoxReset.Unix(), 10))
		got := quotaRetryAfterFromHeaders("codex", headers, now)
		if got == nil || *got != bengalfoxReset.Sub(now) {
			t.Fatalf("quotaRetryAfterFromHeaders() = %v, want %v (main bucket exempted, bengalfox kept)", got, bengalfoxReset.Sub(now))
		}
	})

	t.Run("exhausted with zero balance still cools", func(t *testing.T) {
		headers := withCodexCredits(exhaustedCodexHeaders(resetAt), "True", "0", "False")
		if got := quotaRetryAfterFromHeaders("codex", headers, now); got == nil {
			t.Fatal("quotaRetryAfterFromHeaders() = nil, want retry-after when balance is 0")
		}
	})
}

// TestManager_ExecuteSuccessCodexExhaustedHeaderWithCreditsDoesNotCool is the
// credits counterpart of TestManager_ExecuteSuccessCodexExhaustedHeaderCoolsAuth:
// the same exhausted success headers plus usable credits must leave the account
// schedulable, and the next Execute must still route to it.
func TestManager_ExecuteSuccessCodexExhaustedHeaderWithCreditsDoesNotCool(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, &RoundRobinSelector{}, nil)

	manager.scheduler.setGlobalProxyConfigured(true)
	model := "gpt-5.5"
	authID := "codex-pro-execute-header-exhausted-credits"
	registerSchedulerModels(t, "codex", model, authID)
	headers := withCodexCredits(exhaustedCodexHeaders(time.Now().Add(5*24*time.Hour)), "True", "59897.4803850000", "False")
	manager.executors["codex"] = quotaHeaderExecutor{headers: headers}
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: authID, Provider: "codex", Attributes: map[string]string{"plan_type": "pro"}}); errRegister != nil {
		t.Fatalf("Register(%s) error = %v", authID, errRegister)
	}

	for i := 0; i < 2; i++ {
		if _, errExec := manager.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{}); errExec != nil {
			t.Fatalf("Execute() #%d error = %v, want the credit-backed account to stay routable", i, errExec)
		}
	}

	got, ok := manager.GetByID(authID)
	if !ok || got == nil {
		t.Fatalf("GetByID(%s) missing", authID)
	}
	if got.Unavailable || got.Quota.Exceeded {
		t.Fatalf("auth Unavailable=%v Quota.Exceeded=%v, want both false", got.Unavailable, got.Quota.Exceeded)
	}
	if state := got.ModelStates[model]; state != nil && (state.Unavailable || state.Quota.Exceeded) {
		t.Fatalf("ModelStates[%s] = %#v, want not unavailable / not quota exceeded", model, state)
	}

	picked, errPick := manager.scheduler.pickSingle(context.Background(), "codex", model, cliproxyexecutor.Options{}, nil)
	if errPick != nil {
		t.Fatalf("scheduler.pickSingle() error = %v", errPick)
	}
	if picked == nil || picked.ID != authID {
		t.Fatalf("scheduler.pickSingle() auth = %v, want %s", picked, authID)
	}
}

// TestManager_StreamSuccessCodexExhaustedHeaderWithCreditsDoesNotCool is the
// streaming counterpart (wrapStreamResult success path).
func TestManager_StreamSuccessCodexExhaustedHeaderWithCreditsDoesNotCool(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, &RoundRobinSelector{}, nil)

	manager.scheduler.setGlobalProxyConfigured(true)
	model := "gpt-5.5"
	authID := "codex-pro-stream-header-exhausted-credits"
	registerSchedulerModels(t, "codex", model, authID)
	auth := &Auth{ID: authID, Provider: "codex", Attributes: map[string]string{"plan_type": "pro"}}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("Register(%s) error = %v", authID, errRegister)
	}

	headers := withCodexCredits(exhaustedCodexHeaders(time.Now().Add(5*24*time.Hour)), "True", "59897.4803850000", "False")
	remaining := make(chan cliproxyexecutor.StreamChunk)
	close(remaining)
	result := manager.wrapStreamResult(
		context.Background(),
		auth.Clone(),
		"codex",
		model,
		headers,
		[]cliproxyexecutor.StreamChunk{{Payload: []byte("data: done\n\n")}},
		remaining,
		OAuthModelAliasResult{},
		false,
	)
	for range result.Chunks {
	}

	got, ok := manager.GetByID(authID)
	if !ok || got == nil {
		t.Fatalf("GetByID(%s) missing", authID)
	}
	if state := got.ModelStates[model]; state != nil && (state.Unavailable || state.Quota.Exceeded) {
		t.Fatalf("ModelStates[%s] = %#v, want not unavailable / not quota exceeded", model, state)
	}
	picked, errPick := manager.scheduler.pickSingle(context.Background(), "codex", model, cliproxyexecutor.Options{}, nil)
	if errPick != nil {
		t.Fatalf("scheduler.pickSingle() error = %v", errPick)
	}
	if picked == nil || picked.ID != authID {
		t.Fatalf("scheduler.pickSingle() auth = %v, want %s", picked, authID)
	}
}

// TestCodexRealtimeOverlay_CreditsAware covers the selection overlay (C): an
// exhausted window with usable credits is not marked "rejected" (and overwrites a
// prior rejected reading), while the credit-less case keeps the old "rejected".
func TestCodexRealtimeOverlay_CreditsAware(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	resetAt := now.Add(5 * 24 * time.Hour)
	reset := strconv.FormatInt(resetAt.Unix(), 10)

	exhausted := func() http.Header {
		h := http.Header{}
		h.Set("X-Codex-Primary-Used-Percent", "100")
		h.Set("X-Codex-Primary-Reset-At", reset)
		h.Set("X-Codex-Secondary-Used-Percent", "0")
		return h
	}

	noCredits := parseCodexRealtimeWindows(exhausted(), now)
	if got := noCredits["codex_primary"]; got == nil || got.Status != "rejected" {
		t.Fatalf("no credits: codex_primary = %#v, want Status=rejected (unchanged behavior)", got)
	}
	falseCredits := parseCodexRealtimeWindows(withCodexCredits(exhausted(), "False", "100", "False"), now)
	if got := falseCredits["codex_primary"]; got == nil || got.Status != "rejected" {
		t.Fatalf("has-credits False: codex_primary = %#v, want Status=rejected", got)
	}

	authID := "rt-codex-credits-1"
	defer realtimeRateStore.Delete(authID)
	// An earlier credit-less reading marks the window rejected ...
	IngestServingRateHeaders("codex", authID, exhausted(), now)
	if _, ok := accountRealtimeRejectedReset(authID, now); !ok {
		t.Fatal("expected a rejected reset after the credit-less reading")
	}
	// ... and a later credit-backed reading clears that rejection.
	IngestServingRateHeaders("codex", authID, withCodexCredits(exhausted(), "True", "59897.4803850000", "False"), now)
	store := realtimeRateFor(authID)
	store.mu.RLock()
	primary := store.windows["codex_primary"]
	store.mu.RUnlock()
	if primary.Status == "rejected" {
		t.Fatalf("credits usable: codex_primary Status = %q, want not rejected", primary.Status)
	}
	if primary.UtilizationPercent != 100 {
		t.Fatalf("credits usable: UtilizationPercent = %v, want real upstream 100", primary.UtilizationPercent)
	}
}
