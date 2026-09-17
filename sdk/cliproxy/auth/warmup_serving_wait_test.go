package auth

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
)

func virtualWarmupWait(now *time.Time) (context.Context, *warmupRequestState) {
	ctx := withWarmupRequestState(context.Background(), warmupPurposeServe)
	state := warmupRequestFromContext(ctx)
	state.now = func() time.Time { return *now }
	state.sleep = func(ctx context.Context, d time.Duration) error { *now = now.Add(d); return ctx.Err() }
	state.queue = &warmupWaitQueue{}
	return ctx, state
}

func TestWarmupServingLimiterDelay(t *testing.T) {
	now := adaptiveTestNow
	limiter := NewAccountRateLimiter(WithClock(func() time.Time { return now }))
	if ok, d := limiter.AllowOrDelay("warming", 3, 1); !ok || d != 0 {
		t.Fatal("initial token missing")
	}
	now = now.Add(11347087 * time.Microsecond)
	ok, delay := limiter.AllowOrDelay("warming", 3, 1)
	want := 8652913 * time.Microsecond
	if ok || delay < want || delay > want+time.Nanosecond {
		t.Fatalf("remaining delay=%v allowed=%v", delay, ok)
	}
	if ok, again := limiter.AllowOrDelay("warming", 3, 1); ok || again != delay {
		t.Fatal("denial spent a token")
	}
	now = now.Add(delay)
	if ok, _ := limiter.AllowOrDelay("warming", 3, 1); !ok {
		t.Fatal("token did not become available")
	}
	if ok, delay := limiter.AllowOrDelay("slow", 0.3, 1); !ok || delay != 0 {
		t.Fatal("slow initial token missing")
	}
	if ok, delay := limiter.AllowOrDelay("slow", 0.3, 1); ok || delay < 199*time.Second {
		t.Fatal("quota pacing was clamped to 3 RPM")
	}
}

func TestWarmupServingProtectedWaitKeepsBinding(t *testing.T) {
	s, _, now, auths := servingFixture(t)
	opts := servingOptions("root", "", "parent", "Task")
	servingPick(t, s, opts, auths)
	key := "claude::claude:root::"
	before, _ := s.servingEntry(key, *now)
	*now = now.Add(11347087 * time.Microsecond)
	ctx, request := virtualWarmupWait(now)
	waits := 0
	request.sleep = func(ctx context.Context, delay time.Duration) error {
		waits++
		current, _ := s.servingEntry(key, *now)
		if *current.serving != *before.serving || !current.expiresAt.Equal(before.expiresAt) {
			t.Fatal("wait refreshed binding")
		}
		if delay < 8652913*time.Microsecond || delay > 8652913*time.Microsecond+time.Nanosecond {
			t.Fatalf("delay=%v", delay)
		}
		*now = now.Add(delay)
		return nil
	}
	var picked *Auth
	err := runWarmupSelection(ctx, func(ctx context.Context) error {
		var err error
		picked, err = s.Pick(ctx, "claude", "", opts, auths)
		return err
	})
	if err != nil || picked.ID != "b-cold" || waits != 1 {
		t.Fatalf("pick=%v waits=%d err=%v", picked, waits, err)
	}
	after, _ := s.servingEntry(key, *now)
	if !after.serving.protected || after.serving.revision != before.serving.revision || !after.serving.assignedAt.Equal(before.serving.assignedAt) {
		t.Fatal("wait changed protected segment")
	}
	if again, err := s.Pick(ctx, "claude", "", opts, auths); err != nil || again.ID != picked.ID {
		t.Fatal("same admission attempt spent rate twice")
	}
	if !warmupTakeRateCharge(ctx, picked.ID) || warmupTakeRateCharge(ctx, picked.ID) {
		t.Fatal("rate receipt was not one-shot")
	}
	lease := warmupBindingFromContext(ctx)
	if !lease.protected || lease.authID != picked.ID || lease.revision != after.serving.revision || lease.epoch != s.servingEpoch {
		t.Fatal("admission lease did not follow selection")
	}
	if len(request.queue.accounts) != 0 {
		t.Fatal("wait slot leaked")
	}
}

func TestWarmupServingWeightedProtectionSource(t *testing.T) {
	s, _, now, auths := servingFixture(t)
	s.rng = func() float64 { return 0.999999 }
	opts := servingOptions("root", "", "parent", "Task")
	if got := servingPick(t, s, opts, auths); got != "c-warm" {
		t.Fatalf("weighted pick=%s", got)
	}
	entry, _ := s.servingEntry("claude::claude:root::", *now)
	if !entry.serving.protected || entry.serving.reserved || entry.serving.source != "weighted" {
		t.Fatalf("weighted provenance=%+v", entry.serving)
	}
	*now = now.Add(time.Minute)
	if got := servingPick(t, s, opts, auths); got != "c-warm" {
		t.Fatal("weighted protection was lost via D5")
	}
	*now = now.Add(time.Minute)
	fork := servingOptions("root", "fork", "parent", "<fork-boilerplate>Inherited task</fork-boilerplate>")
	if got := servingPick(t, s, fork, auths); got != "c-warm" {
		t.Fatal("fork lost weighted protection")
	}
	entry, _ = s.servingEntry(warmupChildKey("claude::claude:root::", "fork"), *now)
	if !entry.serving.protected || entry.serving.source != "inherited" || entry.serving.reserved {
		t.Fatal("inherited provenance incorrect")
	}
}

func TestWarmupServingWaitLimitsAndCancellation(t *testing.T) {
	for _, name := range []string{"timeout", "no_mature", "account_queue_full", "global_queue_full", "cancel", "deadline"} {
		t.Run(name, func(t *testing.T) {
			s, cfg, now, auths := servingFixture(t)
			// Serial RPM waits still hand off; actual overlap borrows instead.
			for i := range cfg.WarmupCurve {
				cfg.WarmupCurve[i].RPMLimit = 1
			}
			auths = []*Auth{auths[0], auths[2]}
			if rpm, _ := s.rateLimitParams(auths[1], *cfg, *now); rpm != 1 {
				t.Fatalf("timeout fixture RPM=%v", rpm)
			}
			opts := servingOptions("root", "", "parent", "Task")
			if got := servingPick(t, s, opts, auths); got != "c-warm" {
				t.Fatalf("timeout fixture selected %s", got)
			}
			key := "claude::claude:root::"
			before, _ := s.servingEntry(key, *now)
			ctx, state := virtualWarmupWait(now)
			if name == "no_mature" {
				auths = auths[1:]
			}
			if name == "account_queue_full" {
				state.queue.acquire("c-warm")
			}
			if name == "global_queue_full" {
				for i := 0; i < 64; i++ {
					if !state.queue.acquire(fmt.Sprint(i)) {
						t.Fatal("queue filled early")
					}
				}
			}
			if name == "cancel" {
				state.sleep = func(context.Context, time.Duration) error { return context.Canceled }
			}
			if name == "deadline" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithDeadline(ctx, time.Now().Add(-time.Second))
				defer cancel()
			}
			var picked *Auth
			err := runWarmupSelection(ctx, func(ctx context.Context) error {
				var err error
				picked, err = s.Pick(ctx, "claude", "", opts, auths)
				return err
			})
			after, _ := s.servingEntry(key, *now)
			if name == "cancel" || name == "deadline" {
				if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("cancellation=%v", err)
				}
				if *after.serving != *before.serving || !after.expiresAt.Equal(before.expiresAt) {
					t.Fatal("cancel changed binding")
				}
			} else if name == "no_mature" {
				var busy *warmupBusyError
				if !errors.As(err, &busy) || state.waited != warmupWaitLimit || *after.serving != *before.serving || !after.expiresAt.Equal(before.expiresAt) {
					t.Fatalf("busy state err=%v waited=%v", err, state.waited)
				}
			} else {
				if err != nil || picked.ID != "a-mature" || after.serving.protected {
					t.Fatalf("handoff pick=%v err=%v", picked, err)
				}
				if name == "timeout" && state.waited != warmupWaitLimit {
					t.Fatalf("wait budget=%v", state.waited)
				}
				if name != "timeout" && state.waited != 0 {
					t.Fatal("full queue slept")
				}
				*now = now.Add(time.Minute)
				if got := servingPick(t, s, opts, auths); got != "a-mature" {
					t.Fatal("handoff automatically reclaimed warming account")
				}
			}
		})
	}
}

func TestWarmupServingWaitRevalidatesCurrentBinding(t *testing.T) {
	for _, name := range []string{"rebind", "invalidate", "disable_enable", "ttl"} {
		t.Run(name, func(t *testing.T) {
			s, cfg, now, auths := servingFixture(t)
			opts := servingOptions("root", "", "parent", "Task")
			servingPick(t, s, opts, auths)
			key := "claude::claude:root::"
			old, _ := s.servingEntry(key, *now)
			ctx, state := virtualWarmupWait(now)
			changed := false
			state.sleep = func(context.Context, time.Duration) error {
				if changed {
					t.Fatal("unexpected second wait")
				}
				changed = true
				switch name {
				case "rebind":
					s.servingMu.Lock()
					s.setServingEntry(key, "c-warm", warmupServingSession{protected: true, source: "weighted", summary: old.serving.summary}, *now)
					s.servingMu.Unlock()
				case "invalidate":
					s.InvalidateAuth("b-cold")
				case "disable_enable":
					cfg.WarmupServingReserve = 0
					s.clearWarmupServing()
					cfg.WarmupServingReserve = 0.5
				case "ttl":
					*now = now.Add(3 * time.Hour)
				}
				return nil
			}
			var picked *Auth
			err := runWarmupSelection(ctx, func(ctx context.Context) error {
				var err error
				picked, err = s.Pick(ctx, "claude", "", opts, auths)
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			entry, _ := s.servingEntry(key, *now)
			if name == "rebind" {
				if picked.ID != "c-warm" || entry.serving.revision == old.serving.revision {
					t.Fatal("stale binding overwrote concurrent rebind")
				}
			} else if picked.ID != "a-mature" || entry.serving.protected {
				t.Fatal("stale wait revived old warming pin")
			}
		})
	}
}

func TestWarmupServingCountDoesNotTouchAffinity(t *testing.T) {
	s, _, now, auths := servingFixture(t)
	opts := servingOptions("root", "", "parent", "Task")
	servingPick(t, s, opts, auths)
	*now = now.Add(time.Minute)
	servingPick(t, s, servingOptions("root", "active-child", "independent", "Independent task"), auths)
	epoch := s.servingEpoch
	beforeEntries := make(map[string]sessionEntry)
	for key, entry := range s.cache.entries {
		beforeEntries[key] = entry
	}
	before, _ := s.servingEntry("claude::claude:root::", *now)
	ctx := withWarmupRequestState(context.Background(), warmupPurposeCount)
	_, err := s.Pick(ctx, "claude", "", opts, auths)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := s.servingEntry("claude::claude:root::", *now)
	if *before.serving != *after.serving || !before.expiresAt.Equal(after.expiresAt) || s.servingOrder != 2 || s.servingEpoch != epoch {
		t.Fatal("count mutated affinity/reserve")
	}
	_, err = s.Pick(ctx, "claude", "", servingOptions("other", "child", "independent", "Task"), auths)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.cache.entries) != 2 {
		t.Fatal("count created a binding")
	}
	_, err = s.Pick(context.Background(), "codex", "", cliproxyexecutor.Options{}, []*Auth{{ID: "codex", Provider: "codex", Status: StatusActive, Metadata: map[string]any{}}})
	if err != nil {
		t.Fatal(err)
	}
	if s.servingEpoch != epoch || len(s.cache.entries) != len(beforeEntries) {
		t.Fatal("count/non-Claude cleared Claude protection")
	}
	for key, before := range beforeEntries {
		if after := s.cache.entries[key]; after != before {
			t.Fatal("count/non-Claude changed a binding")
		}
	}
}

func TestWarmupServingManagerWaitOutsideLocks(t *testing.T) {
	s, _, now, auths := servingFixture(t)
	manager := NewManager(nil, s, nil)
	manager.scheduler.setGlobalProxyConfigured(true)
	manager.executors["claude"] = schedulerTestExecutor{provider: "claude"}
	for _, a := range auths {
		if _, err := manager.Register(context.Background(), a); err != nil {
			t.Fatal(err)
		}
	}
	opts := servingOptions("root", "", "parent", "Task")
	if _, _, err := manager.pickNext(context.Background(), "claude", "", opts, nil); err != nil {
		t.Fatal(err)
	}
	ctx, state := virtualWarmupWait(now)
	state.sleep = func(context.Context, time.Duration) error {
		manager.mu.Lock()
		manager.auths["b-cold"].Disabled = true
		manager.mu.Unlock()
		s.servingMu.Lock()
		s.servingMu.Unlock()
		return nil
	}
	picked, _, err := manager.pickNext(ctx, "claude", "", opts, nil)
	if err != nil || picked.ID != "a-mature" {
		t.Fatalf("manager reused stale candidates: %v %v", picked, err)
	}
}

func TestWarmupServingInvalidationRetainsNoRedrawMarker(t *testing.T) {
	for _, name := range []string{"session", "auth", "disable"} {
		t.Run(name, func(t *testing.T) {
			s, _, now, auths := servingFixture(t)
			parent := servingOptions("root", "", "parent", "Task")
			servingPick(t, s, parent, auths)
			child := servingOptions("root", "child", "independent", "Independent task")
			servingPick(t, s, child, auths)
			key := warmupChildKey("claude::claude:root::", "child")
			entry, _ := s.servingEntry(key, *now)
			invalidate := func() {
				switch name {
				case "session":
					s.cache.Invalidate(key)
				case "auth":
					s.InvalidateAuth(entry.authID)
				case "disable":
					s.clearWarmupServing()
				}
			}
			invalidate()
			until, ok := s.cache.servingExpired[key]
			if !ok || !s.cache.recentServingExpiry(key) {
				t.Fatal("invalidation lost no-redraw marker")
			}
			invalidate()
			if s.cache.servingExpired[key] != until {
				t.Fatal("repeated invalidation extended marker")
			}
			order := s.servingOrder
			servingPick(t, s, child, auths)
			entry, _ = s.servingEntry(key, *now)
			if entry.serving.reserved || entry.serving.protected || s.servingOrder != order {
				t.Fatal("invalidation created a new reserve/protection")
			}
		})
	}
}

func TestWarmupServingRebindAtWaitLimitUsesCurrentBinding(t *testing.T) {
	for _, name := range []string{"warming_ready", "second_mature_ready", "warming_busy"} {
		t.Run(name, func(t *testing.T) {
			s, _, now, auths := servingFixture(t)
			auths = append(auths, newAdaptiveClaudeAuth("z-mature", "default_claude_max_20x", matureFirstProd()))
			opts := servingOptions("root", "", "parent", "Task")
			servingPick(t, s, opts, auths)
			key := "claude::claude:root::"
			previous, _ := s.servingEntry(key, *now)
			ctx, state := virtualWarmupWait(now)
			target := "c-warm"
			if name == "second_mature_ready" {
				target = "z-mature"
			}
			state.sleep = func(context.Context, time.Duration) error {
				*now = now.Add(warmupWaitLimit)
				s.servingMu.Lock()
				s.setServingEntry(key, target, warmupServingSession{protected: target == "c-warm", source: "weighted", summary: previous.serving.summary}, *now)
				s.servingMu.Unlock()
				if name == "warming_busy" {
					s.gate.mu.Lock()
					s.gate.inflight[target] = 1
					s.gate.mu.Unlock()
				}
				return nil
			}
			var picked *Auth
			err := runWarmupSelection(ctx, func(ctx context.Context) error {
				var err error
				picked, err = s.Pick(ctx, "claude", "", opts, auths)
				return err
			})
			want := target
			if name == "warming_busy" {
				want = "a-mature"
			}
			if err != nil || picked.ID != want || state.waited != warmupWaitLimit {
				t.Fatalf("current binding pick=%v err=%v waited=%v", picked, err, state.waited)
			}
		})
	}
}

func TestWarmupServingWaitEventsBounded(t *testing.T) {
	logger := log.StandardLogger()
	previous, level := logger.Out, logger.GetLevel()
	defer func() { logger.SetOutput(previous); logger.SetLevel(level) }()
	logger.SetLevel(log.InfoLevel)
	for _, outcome := range []string{"complete", "timeout", "queue-full", "cancel"} {
		t.Run(outcome, func(t *testing.T) {
			var output bytes.Buffer
			logger.SetOutput(&output)
			now := adaptiveTestNow
			ctx, state := virtualWarmupWait(&now)
			if outcome == "queue-full" {
				state.queue.acquire("test-auth")
			}
			if outcome == "cancel" {
				state.sleep = func(context.Context, time.Duration) error { return context.Canceled }
			}
			calls := 0
			_ = runWarmupSelection(ctx, func(current context.Context) error {
				calls++
				mature, _, _ := warmupSelectionPolicy(current)
				if mature || (outcome == "complete" && calls > 1) {
					return nil
				}
				return &warmupSelectionWait{authID: "test-auth", delay: time.Second}
			})
			text := output.String()
			if strings.Count(text, "warmup-wait-start") != 1 || strings.Count(text, "warmup-wait-") != 2 || !strings.Contains(text, "warmup-wait-"+outcome) || !strings.Contains(text, "waited-ms=") {
				t.Fatalf("unbounded/missing wait events: %s", text)
			}
		})
	}
}
