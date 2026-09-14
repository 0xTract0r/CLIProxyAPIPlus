package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	log "github.com/sirupsen/logrus"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func servingOptions(session, agent, system string, messages ...string) cliproxyexecutor.Options {
	items := make([]any, 0, len(messages))
	for i, text := range messages {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		items = append(items, map[string]any{"role": role, "content": []any{map[string]any{"type": "text", "text": text}}})
	}
	user, _ := json.Marshal(map[string]any{"session_id": session})
	body, _ := json.Marshal(map[string]any{
		"model": "claude-sonnet", "metadata": map[string]any{"user_id": string(user)},
		"system": []any{map[string]any{"type": "text", "text": "x-anthropic-billing-header: cc_version=2.1.260;"}, map[string]any{"type": "text", "text": system, "cache_control": map[string]any{"type": "ephemeral", "ttl": "5m"}}},
		"tools":  []any{}, "messages": items,
	})
	headers := http.Header{"X-Claude-Code-Session-Id": []string{session}}
	if agent != "" {
		headers.Set("X-Claude-Code-Agent-Id", agent)
	}
	return cliproxyexecutor.Options{Headers: headers, OriginalRequest: body}
}

func servingFixture(t *testing.T) (*AdaptiveSelector, *internalconfig.AccountSchedulingConfig, *time.Time, []*Auth) {
	t.Helper()
	now := adaptiveTestNow
	cfg := internalconfig.DefaultAccountSchedulingConfig()
	cfg.WarmupServingReserve = 0.5
	auths := []*Auth{
		newAdaptiveClaudeAuth("a-mature", "default_claude_max_20x", matureFirstProd()),
		newAdaptiveClaudeAuth("b-cold", "default_claude_max_5x", time.Time{}),
		newAdaptiveClaudeAuth("c-warm", "default_claude_pro", warmupFirstProd()),
	}
	s := NewAdaptiveSelector(AdaptiveSelectorConfig{Scheduling: cfg, SessionAffinity: true, SessionTTL: 2 * time.Hour}, WithAdaptiveClock(func() time.Time { return now }), WithAdaptiveRand(constRand(0)), WithAdaptiveSchedulingProvider(func() internalconfig.AccountSchedulingConfig { return cfg }))
	t.Cleanup(s.Stop)
	return s, &cfg, &now, auths
}

func servingPick(t *testing.T, s *AdaptiveSelector, opts cliproxyexecutor.Options, auths []*Auth) string {
	t.Helper()
	a, err := s.Pick(context.Background(), "claude", "", opts, auths)
	if err != nil {
		t.Fatal(err)
	}
	return a.ID
}

func TestWarmupServingReservePinAndDisable(t *testing.T) {
	s, cfg, now, auths := servingFixture(t)
	opts := servingOptions("root", "", "parent prompt", "request")
	if got := servingPick(t, s, opts, auths); got != "b-cold" {
		t.Fatalf("reserve picked %s", got)
	}
	if _, set := AuthFirstProductionAt(auths[1]); set {
		t.Fatal("selection stamped an unserved account")
	}
	key := "claude::claude:root::"
	entry, _ := s.servingEntry(key, *now)
	assigned := entry.serving.assignedAt
	*now = now.Add(time.Minute)
	if got := servingPick(t, s, opts, auths); got != "b-cold" {
		t.Fatalf("reserved binding migrated via D5: %s", got)
	}
	entry, _ = s.servingEntry(key, *now)
	if !entry.serving.reserved || !entry.serving.assignedAt.Equal(assigned) {
		t.Fatal("keep reset pin or assignment age")
	}
	cfg.WarmupServingReserve = 0
	if got := servingPick(t, s, opts, auths); got != "a-mature" {
		t.Fatalf("disabled D5 changed: %s", got)
	}
	if s.servingActive.Load() || len(s.servingAccounts) != 0 {
		t.Fatal("hot disable retained state")
	}
	cfg.WarmupServingReserve = 0.5
	*now = now.Add(time.Minute)
	if got := servingPick(t, s, opts, auths); got != "a-mature" {
		t.Fatal("re-enable revived old pin")
	}
}

func TestWarmupServingDefaultRNGAndNonClaude(t *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		s, cfg, _, auths := servingFixture(t)
		if provider == "claude" {
			cfg.WarmupServingReserve = 0
		}
		calls := 0
		s.rng = func() float64 { calls++; return 0 }
		if provider == "codex" {
			auths = []*Auth{{ID: "codex", Provider: "codex", Status: StatusActive, Metadata: map[string]any{}}}
		}
		if _, err := s.Pick(context.Background(), provider, "", cliproxyexecutor.Options{}, auths); err != nil {
			t.Fatal(err)
		}
		wantCalls := 1
		if provider == "codex" {
			wantCalls = 0
		} // A single weighted candidate does not draw.
		if calls != wantCalls || s.servingActive.Load() {
			t.Fatalf("disabled/non-Claude changed RNG/state: %s %d", provider, calls)
		}
	}
}

func TestWarmupServingConcurrentOpportunities(t *testing.T) {
	s, _, _, auths := servingFixture(t)
	const n = 32
	results := make(chan string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			a, err := s.Pick(context.Background(), "claude", "", servingOptions(fmt.Sprint(i), "", "parent", "new"), auths)
			if err != nil {
				results <- "error"
				return
			}
			results <- a.ID
		}(i)
	}
	wg.Wait()
	close(results)
	counts := map[string]int{}
	for id := range results {
		counts[id]++
	}
	if counts["b-cold"] != n/2 || counts["c-warm"] != n/2 {
		t.Fatalf("unserved reservations piled up: %v", counts)
	}
	if s.gate.DailyCount("b-cold") != 0 {
		t.Fatal("selection counted as real service")
	}
}

func TestWarmupServingRealRequestDeficit(t *testing.T) {
	s, _, _, auths := servingFixture(t)
	s.gate.RecordRequest("b-cold")
	if got := servingPick(t, s, servingOptions("root", "", "parent", "new"), auths); got != "c-warm" {
		t.Fatalf("least served not preferred: %s", got)
	}
}

func TestWarmupServingGatesAndFailover(t *testing.T) {
	for _, test := range []string{"daily", "tokens", "concurrency", "disabled", "retry", "all-warming", "single"} {
		t.Run(test, func(t *testing.T) {
			s, cfg, _, auths := servingFixture(t)
			opts := servingOptions("root", "", "parent", "new")
			want := "a-mature"
			switch test {
			case "daily":
				for _, a := range auths[1:] {
					for i := 0; i < 200; i++ {
						s.gate.RecordRequest(a.ID)
					}
				}
			case "tokens":
				for i := range cfg.WarmupCurve {
					cfg.WarmupCurve[i].TokenDailyBudget = 1
				}
				for _, a := range auths[1:] {
					s.gate.RecordTokens(a.ID, 1)
				}
			case "concurrency":
				s.gate.mu.Lock()
				for _, a := range auths[1:] {
					s.gate.inflight[a.ID] = 1
				}
				s.gate.mu.Unlock()
			case "disabled":
				for _, a := range auths[1:] {
					a.Disabled = true
				}
			case "retry":
				opts = withFailoverMatureOnly(opts)
			case "all-warming":
				auths = auths[1:]
				want = "b-cold"
			case "single":
				auths = auths[1:2]
				want = "b-cold"
			}
			if got := servingPick(t, s, opts, auths); got != want {
				t.Fatalf("%s got %s want %s", test, got, want)
			}
		})
	}
}

func TestWarmupServingChildIdentityAndResume(t *testing.T) {
	s, _, now, auths := servingFixture(t)
	parent := servingOptions("root", "", "parent instructions", "parent history")
	s.cache.Set("claude::claude:root::", "a-mature")
	servingPick(t, s, parent, auths)
	fresh := servingOptions("root", "child-a", "independent instructions", "delegated task")
	if got := servingPick(t, s, fresh, auths); got != "b-cold" {
		t.Fatalf("fresh child not reserved: %s", got)
	}
	*now = now.Add(time.Minute)
	resume := servingOptions("root", "child-a", "independent instructions", "delegated task", "result", "continue")
	if got := servingPick(t, s, resume, auths); got != "b-cold" {
		t.Fatalf("resume did not keep child identity: %s", got)
	}
	if got := servingPick(t, s, parent, auths); got != "a-mature" {
		t.Fatal("child overwrote parent")
	}
	// The real CLI's fork adds billing/gitStatus text but retains the parent's
	// history and a fork-boilerplate block; it has no dedicated type header.
	fork := servingOptions("root", "fork-a", "parent instructions\n\ngitStatus: This is the git status at the start of the conversation.\nmain", "parent history", "tool call", "<fork-boilerplate>You are a worker fork.</fork-boilerplate>")
	if got := servingPick(t, s, fork, auths); got != "a-mature" {
		t.Fatalf("fork lost parent affinity: %s", got)
	}
	unknown := servingOptions("root", "unknown-a", "parent instructions", "different task")
	if got := servingPick(t, s, unknown, auths); got != "a-mature" {
		t.Fatalf("unknown split from parent: %s", got)
	}
	*now = now.Add(time.Minute)
	unknown = servingOptions("root", "unknown-a", "now different", "new text")
	if got := servingPick(t, s, unknown, auths); got != "a-mature" {
		t.Fatal("unknown was reclassified mid-segment")
	}
	nested := servingOptions("root", "nested-a", "nested instructions", "independent nested task")
	nested.Headers.Set("X-Claude-Code-Parent-Agent-Id", "child-a")
	if got := servingPick(t, s, nested, auths); got != "c-warm" {
		t.Fatalf("nested child failed: %s", got)
	}
	// A new model has no observed parent sample in its own namespace.
	if _, err := s.Pick(context.Background(), "claude", "different-model", fresh, auths); err != nil {
		t.Fatal(err)
	}
	entry, _ := s.servingEntry(warmupChildKey("claude::claude:root::different-model", "child-a"), *now)
	if entry.serving == nil || !entry.serving.parentAffine || entry.serving.reserved {
		t.Fatal("cross-model borrowed parent evidence")
	}
}

func TestWarmupServingForkInheritsPinAndInvalidation(t *testing.T) {
	s, cfg, now, auths := servingFixture(t)
	parent := servingOptions("root", "", "parent", "history")
	servingPick(t, s, parent, auths)
	*now = now.Add(time.Minute)
	fork := servingOptions("root", "fork", "parent", "history", "answer", "<fork-boilerplate>fork</fork-boilerplate>")
	if got := servingPick(t, s, fork, auths); got != "b-cold" {
		t.Fatalf("fork lost reserved parent: %s", got)
	}
	*now = now.Add(time.Minute)
	if got := servingPick(t, s, fork, auths); got != "b-cold" {
		t.Fatal("fork pin was not retained")
	}
	cfg.WarmupServingReserve = 0
	servingPick(t, s, parent, auths)
	if _, ok := s.cache.Get(warmupChildKey("claude::claude:root::", "fork")); ok {
		t.Fatal("disabled child survived")
	}
	cfg.WarmupServingReserve = 0.5
	s.InvalidateAuth("b-cold")
	if _, exists := s.servingAccounts["b-cold"]; exists {
		t.Fatal("invalidation retained account state")
	}
}

func TestWarmupServingReservedBudgetFailureClearsPin(t *testing.T) {
	s, _, now, auths := servingFixture(t)
	opts := servingOptions("root", "", "parent", "task")
	servingPick(t, s, opts, auths)
	for i := 0; i < 200; i++ {
		s.gate.RecordRequest("b-cold")
	}
	*now = now.Add(time.Minute)
	if got := servingPick(t, s, opts, auths); got != "a-mature" {
		t.Fatal("over-budget pin did not move to mature")
	}
	entry, _ := s.servingEntry("claude::claude:root::", *now)
	if entry.serving.reserved {
		t.Fatal("rebind retained pin")
	}
}

func TestWarmupServingMigrationWindows(t *testing.T) {
	for _, mode := range []string{"idle", "context-reset", "age", "disabled-budget", "inflight", "opaque", "append", "starvation-reset"} {
		t.Run(mode, func(t *testing.T) {
			var logs bytes.Buffer
			previousOutput := log.StandardLogger().Out
			log.SetOutput(&logs)
			defer log.SetOutput(previousOutput)
			s, cfg, now, auths := servingFixture(t)
			cfg.WarmupServingMigrationTokenBudget = 100000
			cfg.WarmupServingMaxBindingAgeSeconds = 60
			original := servingOptions("root", "", "parent", strings.Repeat("history", 1000), "reply", "continue")
			s.cache.Set("claude::claude:root::", "a-mature")
			servingPick(t, s, original, auths)
			*now = now.Add(61 * time.Second)
			next := original
			wantMove := true
			switch mode {
			case "idle":
				cfg.WarmupServingMaxBindingAgeSeconds = 0
				*now = now.Add(5 * time.Minute)
			case "context-reset":
				entry, _ := s.servingEntry("claude::claude:root::", *now)
				state := *entry.serving
				state.assignedAt = *now // Reset must win without an aged binding.
				s.setServingEntry("claude::claude:root::", "a-mature", state, *now)
				next = servingOptions("root", "", "parent", "short summary")
				cfg.WarmupServingMaxBindingAgeSeconds = 0
			case "disabled-budget":
				cfg.WarmupServingMigrationTokenBudget = 0
				wantMove = false
			case "inflight":
				s.gate.mu.Lock()
				s.gate.inflight["a-mature"] = 1
				s.gate.mu.Unlock()
				wantMove = false
			case "opaque":
				next = servingOptions("root", "", "parent", "x")
				var body map[string]any
				json.Unmarshal(next.OriginalRequest, &body)
				body["messages"] = []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "image", "source": map[string]any{"type": "base64", "data": "abc"}}}}}
				next.OriginalRequest, _ = json.Marshal(body)
				wantMove = false
			case "append":
				cfg.WarmupServingMaxBindingAgeSeconds = 0
				next = servingOptions("root", "", "parent", strings.Repeat("history", 1000), "reply", "continue", "reply2", "more")
				wantMove = false
			case "starvation-reset":
				s.gate.RecordRequest("b-cold")
				s.gate.RecordRequest("c-warm")
				wantMove = false
			}
			if mode == "idle" || mode == "context-reset" {
				for id, state := range s.servingAccounts {
					state.unchangedSince = now.Add(-2 * time.Hour)
					s.servingAccounts[id] = state
				}
			}
			got := servingPick(t, s, next, auths)
			if wantMove && got != "b-cold" {
				t.Fatalf("%s failed to migrate: %s", mode, got)
			}
			if !wantMove && got != "a-mature" {
				t.Fatalf("%s unexpectedly migrated: %s", mode, got)
			}
			if wantMove {
				if !strings.Contains(logs.String(), "adaptive-select: migration-"+mode) {
					t.Fatalf("wrong migration reason: %s", logs.String())
				}
				if len(s.servingCharges) != 1 || s.servingCharges[0].tokens <= 0 {
					t.Fatal("migration cost not reserved")
				}
				*now = now.Add(30 * time.Second)
				if got := servingPick(t, s, next, auths); got != "b-cold" {
					t.Fatal("new segment churned")
				}
			}
		})
	}
}

func TestWarmupServingMigrationBudgetAndConcurrency(t *testing.T) {
	s, cfg, now, auths := servingFixture(t)
	cfg.WarmupServingMaxBindingAgeSeconds = 60
	opts := servingOptions("root", "", "parent", "task")
	cost := summarizeWarmupRequest(opts.OriginalRequest).inputCost
	cfg.WarmupServingMigrationTokenBudget = cost
	s.cache.Set("claude::claude:root::", "a-mature")
	servingPick(t, s, opts, auths)
	*now = now.Add(time.Minute)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.Pick(context.Background(), "claude", "", opts, auths)
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if len(s.servingCharges) != 1 {
		t.Fatalf("concurrent budget spend=%d", len(s.servingCharges))
	}
	// A second root cannot overspend the shared hour, even on a natural reset.
	second := servingOptions("next", "", "parent", "task")
	s.cache.Set("claude::claude:next::", "a-mature")
	servingPick(t, s, second, auths)
	*now = now.Add(61 * time.Second)
	if got := servingPick(t, s, second, auths); got != "a-mature" {
		t.Fatal("hour budget overspent")
	}
	*now = now.Add(time.Hour)
	if got := servingPick(t, s, second, auths); got == "a-mature" {
		t.Fatal("rolling hour budget did not recover")
	}
}

func TestWarmupServingSummarySafety(t *testing.T) {
	parent := summarizeWarmupRequest(servingOptions("s", "", "parent", "history").OriginalRequest)
	for _, prompt := range []string{"parent", "parent\n\ngitStatus: This is the git status at the start of the conversation.\nchanged"} {
		child := summarizeWarmupRequest(servingOptions("s", "a", prompt, "new task").OriginalRequest)
		if isFreshWarmupChild(child, parent) {
			t.Fatal("dynamic system variation authorized fresh routing")
		}
	}
	if parent.cacheTTL != 5*time.Minute || !parent.known {
		t.Fatalf("summary=%+v", parent)
	}
	opts := servingOptions("s", "", "parent", "history")
	opts.OriginalRequest = []byte(strings.ReplaceAll(string(opts.OriginalRequest), `"ttl":"5m"`, `"ttl":"unknown"`))
	if got := summarizeWarmupRequest(opts.OriginalRequest); got.cacheTTL != time.Hour {
		t.Fatal("unknown TTL was not conservative")
	}
	if got := summarizeWarmupRequest([]byte(`{"messages":[]}`)); got.known {
		t.Fatal("invalid payload accepted")
	}
	shortFork := summarizeWarmupRequest(servingOptions("s", "fork", "different system", "<fork-boilerplate>fork</fork-boilerplate>").OriginalRequest)
	if !shortFork.fork || isFreshWarmupChild(shortFork, parent) {
		t.Fatal("decoded fork marker did not protect a shortened fork")
	}
}

func TestWarmupServingManagerMixedClaudeRoute(t *testing.T) {
	s, _, now, auths := servingFixture(t)
	manager := NewManager(nil, s, nil)
	manager.scheduler.setGlobalProxyConfigured(true)
	manager.executors["claude"] = schedulerTestExecutor{provider: "claude"}
	for _, a := range auths {
		if _, err := manager.Register(context.Background(), a); err != nil {
			t.Fatal(err)
		}
	}
	parent := servingOptions("root", "", "parent", "history")
	s.cache.Set("mixed::claude:root::", "a-mature")
	pick := func(opts cliproxyexecutor.Options) string {
		t.Helper()
		a, _, provider, err := manager.pickNextMixed(context.Background(), []string{"claude"}, "", opts, nil)
		if err != nil {
			t.Fatal(err)
		}
		if provider != "claude" {
			t.Fatalf("provider=%s", provider)
		}
		return a.ID
	}
	if got := pick(parent); got != "a-mature" {
		t.Fatalf("parent=%s", got)
	}
	fresh := servingOptions("root", "child", "independent", "task")
	if got := pick(fresh); got != "b-cold" {
		t.Fatalf("mixed manager bypassed reserve: %s", got)
	}
	*now = now.Add(time.Minute)
	if got := pick(fresh); got != "b-cold" {
		t.Fatalf("mixed manager lost child binding: %s", got)
	}
	if got := pick(parent); got != "a-mature" {
		t.Fatal("mixed child changed root")
	}
	if warmupServingClaudeRoute("mixed", append(auths, &Auth{ID: "codex", Provider: "codex"})) {
		t.Fatal("true mixed pool opted into Claude policy")
	}
}

func TestWarmupServingDisableRejectsStaleSnapshot(t *testing.T) {
	s, cfg, _, auths := servingFixture(t)
	opts := servingOptions("root", "", "parent", "task")
	servingPick(t, s, opts, auths)
	var live atomic.Pointer[internalconfig.AccountSchedulingConfig]
	live.Store(cfg)
	readSnapshot, continuePick := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	s.scheduling = func() internalconfig.AccountSchedulingConfig {
		snapshot := *live.Load()
		if calls.Add(1) == 1 {
			close(readSnapshot)
			<-continuePick
		}
		return snapshot
	}
	done := make(chan error, 1)
	go func() { _, err := s.Pick(context.Background(), "claude", "", opts, auths); done <- err }()
	<-readSnapshot
	disabled := *cfg
	disabled.WarmupServingReserve = 0
	live.Store(&disabled)
	servingPick(t, s, opts, auths)
	close(continuePick)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if s.servingActive.Load() || len(s.servingAccounts) != 0 || len(s.servingCharges) != 0 {
		t.Fatal("stale enabled snapshot resurrected state")
	}
	s.cache.mu.RLock()
	defer s.cache.mu.RUnlock()
	for _, entry := range s.cache.entries {
		if entry.serving != nil {
			t.Fatal("stale pick recreated pin")
		}
	}
}

func TestWarmupServingExpiredBindingDoesNotReserve(t *testing.T) {
	for _, cleanup := range []bool{false, true} {
		t.Run(fmt.Sprint(cleanup), func(t *testing.T) {
			s, cfg, now, auths := servingFixture(t)
			cfg.WarmupServingMigrationTokenBudget = 0
			s.cache.Stop()
			s.cache = NewSessionCache(30 * time.Minute)
			opts := servingOptions("root", "", "parent", "task")
			s.cache.Set("claude::claude:root::", "a-mature")
			servingPick(t, s, opts, auths)
			s.cache.mu.Lock()
			entry := s.cache.entries["claude::claude:root::"]
			entry.expiresAt = time.Now().Add(-time.Minute)
			s.cache.entries["claude::claude:root::"] = entry
			s.cache.mu.Unlock()
			*now = now.Add(40 * time.Minute)
			if cleanup {
				s.cache.cleanup()
			}
			if got := servingPick(t, s, opts, auths); got != "a-mature" {
				t.Fatalf("expired binding bypassed migration budget through reserve: %s", got)
			}
			if len(s.servingCharges) != 0 {
				t.Fatal("expiry incorrectly charged proactive migration")
			}
		})
	}
}

func TestWarmupServingProtectsUncachedTailAndResetsAfterward(t *testing.T) {
	s, cfg, now, auths := servingFixture(t)
	cfg.WarmupServingMaxBindingAgeSeconds = 60
	cfg.WarmupServingMigrationTokenBudget = 100000
	parent := servingOptions("root", "", "parent", strings.Repeat("history", 1000), "answer", "continue")
	s.cache.Set("claude::claude:root::", "a-mature")
	servingPick(t, s, parent, auths)
	*now = now.Add(61 * time.Second)
	var body map[string]any
	if err := json.Unmarshal(parent.OriginalRequest, &body); err != nil {
		t.Fatal(err)
	}
	messages := body["messages"].([]any)
	assistant := messages[1].(map[string]any)["content"].([]any)[0].(map[string]any)
	assistant["cache_control"] = map[string]any{"type": "ephemeral"}
	parent.OriginalRequest, _ = json.Marshal(body)
	if !summarizeWarmupRequest(parent.OriginalRequest).uncachedTail {
		t.Fatal("cached-assistant/uncached-user shape not protected")
	}
	if got := servingPick(t, s, parent, auths); got != "a-mature" {
		t.Fatal("age fallback moved the prefix-dependent tail task")
	}
	if len(s.servingCharges) != 0 {
		t.Fatal("protected tail consumed migration budget")
	}
	shortened := servingOptions("root", "", "parent", "short summary")
	if got := servingPick(t, s, shortened, auths); got != "b-cold" {
		t.Fatal("completed reset could not migrate")
	}
}
