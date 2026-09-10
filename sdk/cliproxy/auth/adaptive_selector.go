package auth

import (
	"context"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// AdaptiveSelector is the tier/quota/warm-up-aware credential selector for the
// adaptive account-scheduling change
// (openspec/changes/add-adaptive-account-scheduling, tasks.md Phase 1 task 1.2 +
// Phase 2 task 2.2 + Phase 3 task 3.2 + Phase 4 task 4.1). It implements the
// existing auth.Selector interface, so the routing layer can select it via
// routing.strategy == internalconfig.RoutingStrategyAdaptive
// (sdk/cliproxy/service_config.go:newRoutingSelector, a separate wiring slice --
// this file is deliberately NOT wired in itself).
//
// What it composes (all mechanisms owned by earlier sibling slices; this file
// only orchestrates them into one Selector):
//
//   - Weighted selection over the currently-available credentials, using the
//     pure AccountSelectionWeight score (account_weight.go: tier base capacity x
//     quota headroom x freshness factor -- design.md D1). Distribution is
//     proportional to weight, so a Claude Max 20x account承接 more than a Max 5x
//     account, and an account low on quota headroom承接 less (spec.md "高容量/
//     高余量账号承接更多").
//
//   - Per-account outbound rate limiting (account_rate_limiter.go, design.md
//     D2): the account a weighted pick lands on must pass its own token bucket
//     (rpm/burst derived from its warm-up stage / mature ceiling) BEFORE it is
//     returned. An account over its instantaneous ceiling is skipped in favour
//     of the next weighted candidate rather than being handed the request and
//     left to 429 after the fact (spec.md "每账号限流平滑", task 2.2). Because a
//     warming account's rpm ceiling is tiny (design §5.1: w1 = 3 rpm) while a
//     mature account's is generous (design §5.3: ~45 rpm), a workflow-style
//     burst naturally drains a warming account's bucket after a request or two
//     and then routes to a mature account for the rest -- the "洪峰路由成熟号"
//     behaviour (design.md D4, task 3.2) falls out of the weight + token-bucket
//     combination without any explicit flood detector.
//
//   - Session stickiness with maturity grading (design.md D5, task 4.1). When
//     session affinity is enabled this selector maintains its own SessionCache
//     and reuses the package's existing session-ID extraction
//     (extractSessionIDs in selector.go -- NOT re-implemented) so it can see
//     cache hits directly and grade them, which an outer SessionAffinitySelector
//     wrapper could not (that wrapper short-circuits on a cache hit and never
//     consults the inner selector). Therefore the routing wiring MUST build this
//     selector with SessionAffinity set and MUST NOT additionally wrap it in a
//     SessionAffinitySelector.
//
// Backward compatibility (design.md D7): a provider this scheduler has no tier
// weight for (anything other than claude/codex, or a tier configured to weight
// 0) yields no weighted candidate and falls through to the wrapped fallback
// selector (round-robin by default), so non-Claude/Codex traffic behaves exactly
// as it does today. If every weighted candidate is momentarily rate-limited the
// request is still never denied: the overflow is served by a WEIGHTED draw over
// the same adaptive candidates (still proportional to tier capacity), and only a
// genuinely non-adaptive / no-candidate pool degrades to the round-robin fallback
// -- the rpm token bucket is an outbound smoother, never a hard gate that can
// manufacture a 429 the upstream did not send.
//
// The one deliberate hard gate is the UTC-daily warm-up budget (hole-2 thin-pool
// fix, dailyBudgetHardGate): when the ONLY accounts able to serve are warming
// accounts that have ALL spent their daily budget, the selector denies with a
// retryable 429 (errAccountDailyBudgetExhausted) instead of degrading to the
// round-robin fallback, because that fallback re-picks over the full pool and
// would ignore the daily budget -- hammering the very account warm-up is meant to
// protect (only the concurrency=1 gate left as a backstop). This is scoped
// strictly to the all-over-budget case: as long as any mature account, any
// under-budget account, or any non-adaptive account can serve, the request is
// routed normally and never denied.
type AdaptiveSelector struct {
	// fallback is the base selector used for non-adaptive providers and as the
	// degraded path when no weighted candidate can currently be served. Never
	// nil after construction (defaults to &RoundRobinSelector{}).
	fallback Selector

	// scheduling returns the live AccountSchedulingConfig snapshot to score
	// against. It is a function (not a stored value) so a hot config reload is
	// picked up on the next Pick without rebuilding the selector; by default it
	// closes over the snapshot passed at construction.
	scheduling func() internalconfig.AccountSchedulingConfig

	// limiter is the per-account token-bucket smoother. Owned (created and
	// reclaim-looped by this selector) unless injected via
	// WithAdaptiveRateLimiter, in which case the injector owns its lifecycle.
	limiter     *AccountRateLimiter
	ownsLimiter bool

	// gate is the per-account in-flight concurrency semaphore + UTC-daily
	// request-budget counter (account_gate.go). The selector reads it at Pick
	// time to avoid an account already at its concurrency ceiling or past its
	// warm-up daily budget; the auth Manager's execution path drives the same
	// gate instance (reached via the AccountGate accessor) to acquire/release a
	// slot and record each request. Always non-nil after construction (created
	// here unless injected via WithAdaptiveAccountGate). It holds no background
	// goroutine, so it needs no Stop.
	gate *AccountConcurrencyGate

	// cache holds session -> auth stickiness bindings. Nil when session
	// affinity is disabled.
	cache           *SessionCache
	sessionAffinity bool

	// now / rng are injectable for deterministic tests (production defaults:
	// time.Now, rand.Float64). rng MUST return a value in [0,1) and, in
	// production, MUST be safe for concurrent use (rand.Float64 is).
	now func() time.Time
	rng func() float64

	// pickMu guards the harden-account-scheduling-limiter P1a anti-streak state
	// below. It is ONLY touched when a Pick's config carries AntiStreakLimit > 0
	// (the feature is off by default), so the default pure-weighted path takes no
	// extra lock and its concurrency profile is unchanged.
	pickMu sync.Mutex
	// lastWarmPickID / warmPickStreak track the most recently picked WARMING
	// account and how many times in a row it has been selected, so the anti-streak
	// rule can rotate off it once the streak hits AntiStreakLimit. A mature /
	// non-adaptive pick clears the streak (it only counts consecutive warming
	// picks). Guarded by pickMu.
	lastWarmPickID string
	warmPickStreak int
}

// defaultAdaptiveReclaimInterval is how often an owned rate limiter's idle
// buckets are reclaimed. It only bounds memory for churning accounts and has no
// effect on rate-limiting decisions, so a coarse cadence is fine.
const defaultAdaptiveReclaimInterval = 5 * time.Minute

// AdaptiveSelectorConfig is the construction input for NewAdaptiveSelector. It
// is a struct (rather than positional args) so the routing wiring slice can set
// only the fields it cares about and so new knobs can be added without breaking
// that call site.
type AdaptiveSelectorConfig struct {
	// Fallback is the base selector for non-adaptive providers and the degraded
	// (all-rate-limited / no-weighted-candidate) path. Defaults to a
	// RoundRobinSelector when nil.
	Fallback Selector
	// Scheduling is the AccountSchedulingConfig snapshot to score against.
	// Callers wanting live hot-reload should also pass
	// WithAdaptiveSchedulingProvider; otherwise this snapshot is used for the
	// selector's lifetime.
	Scheduling internalconfig.AccountSchedulingConfig
	// SessionAffinity enables the design.md D5 sticky-with-grading path. When
	// false the selector is a pure weighted picker and never binds sessions.
	SessionAffinity bool
	// SessionTTL is the stickiness TTL; <=0 defaults to one hour (matching the
	// existing SessionAffinitySelector default).
	SessionTTL time.Duration
}

// AdaptiveSelectorOption customizes an AdaptiveSelector at construction.
type AdaptiveSelectorOption func(*AdaptiveSelector)

// WithAdaptiveClock injects the wall-clock the selector (and, if it owns one,
// its rate limiter) reads. nil is ignored. Production uses time.Now.
func WithAdaptiveClock(now func() time.Time) AdaptiveSelectorOption {
	return func(s *AdaptiveSelector) {
		if now != nil {
			s.now = now
		}
	}
}

// WithAdaptiveRand injects the [0,1) random source used for weighted selection.
// nil is ignored. The supplied function MUST be safe for concurrent use in
// production (the default, rand.Float64, is); a test may pass a single-goroutine
// deterministic source.
func WithAdaptiveRand(r func() float64) AdaptiveSelectorOption {
	return func(s *AdaptiveSelector) {
		if r != nil {
			s.rng = r
		}
	}
}

// WithAdaptiveRateLimiter injects a rate limiter instead of letting the selector
// create its own. The injector then owns the limiter's lifecycle (Stop /
// reclaim loop); the selector will not start a reclaim loop or Stop it. nil is
// ignored. Useful for sharing one limiter across selectors or for driving it
// with a mock clock in tests.
func WithAdaptiveRateLimiter(l *AccountRateLimiter) AdaptiveSelectorOption {
	return func(s *AdaptiveSelector) {
		if l != nil {
			s.limiter = l
			s.ownsLimiter = false
		}
	}
}

// WithAdaptiveAccountGate injects the per-account concurrency + daily-budget
// gate instead of letting the selector create its own. Useful for sharing one
// gate across selectors or for pre-loading counts / driving the UTC clock in
// tests. nil is ignored.
func WithAdaptiveAccountGate(g *AccountConcurrencyGate) AdaptiveSelectorOption {
	return func(s *AdaptiveSelector) {
		if g != nil {
			s.gate = g
		}
	}
}

// WithAdaptiveSchedulingProvider makes the selector read config live from fn on
// every Pick (for hot-reload), overriding the static Scheduling snapshot. nil is
// ignored.
func WithAdaptiveSchedulingProvider(fn func() internalconfig.AccountSchedulingConfig) AdaptiveSelectorOption {
	return func(s *AdaptiveSelector) {
		if fn != nil {
			s.scheduling = fn
		}
	}
}

// NewAdaptiveSelector builds an AdaptiveSelector. It creates and starts an owned
// rate limiter (with an idle-bucket reclaim loop) unless one is injected via
// WithAdaptiveRateLimiter, and a SessionCache when cfg.SessionAffinity is set.
// Call Stop to release those resources (the auth Manager does this
// automatically via the StoppableSelector interface on shutdown / selector
// replacement).
func NewAdaptiveSelector(cfg AdaptiveSelectorConfig, opts ...AdaptiveSelectorOption) *AdaptiveSelector {
	s := &AdaptiveSelector{
		fallback:        cfg.Fallback,
		sessionAffinity: cfg.SessionAffinity,
		now:             time.Now,
		rng:             rand.Float64,
	}
	if s.fallback == nil {
		s.fallback = &RoundRobinSelector{}
	}
	snapshot := cfg.Scheduling
	s.scheduling = func() internalconfig.AccountSchedulingConfig { return snapshot }

	for _, opt := range opts {
		if opt != nil {
			opt(s)
		}
	}

	if s.limiter == nil {
		s.limiter = NewAccountRateLimiter(WithClock(s.now))
		s.ownsLimiter = true
	}
	if s.gate == nil {
		s.gate = NewAccountConcurrencyGate(WithGateClock(s.now))
	}
	if s.sessionAffinity {
		ttl := cfg.SessionTTL
		if ttl <= 0 {
			ttl = time.Hour
		}
		s.cache = NewSessionCache(ttl)
	}
	if s.ownsLimiter {
		s.limiter.StartReclaimLoop(defaultAdaptiveReclaimInterval)
	}
	return s
}

// Pick implements Selector. See the type doc for the full strategy.
func (s *AdaptiveSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	now := s.now()
	available, err := getAvailableAuths(auths, provider, model, now)
	if err != nil {
		return nil, err
	}
	available = preferCodexWebsocketAuths(ctx, provider, available)
	cfg := s.scheduling()

	if s.sessionAffinity && s.cache != nil {
		if picked, handled, reason, sessionID, errPick := s.pickWithAffinity(ctx, provider, model, opts, auths, available, cfg, now); handled {
			if errPick == nil {
				s.logPick(ctx, reason, provider, model, sessionID, picked, cfg, now)
			}
			return picked, errPick
		}
	}

	// ERR-3 failover mature-only preference: on a failover retry the execution loop
	// sets this hint so the retry prefers a mature account over a warming (养号) one;
	// scoreFailoverCandidates falls back to the full pool when no mature account
	// exists, so an all-warming fleet still serves.
	failoverMatureOnly := failoverMatureOnlyFromMetadata(opts.Metadata)
	if picked, ok := s.pickFromCandidates(s.scoreFailoverCandidates(available, cfg, now, failoverMatureOnly), cfg, now); ok {
		reason := "weighted-new"
		if failoverMatureOnly {
			reason = "weighted-new-failover"
		}
		s.logPick(ctx, reason, provider, model, "", picked, cfg, now)
		return picked, nil
	}
	// Hole-2 thin-pool hard gate: if the empty candidate set is caused SOLELY by
	// every servable account being a warming account that has spent its UTC-daily
	// budget, deny with a retryable 429 instead of letting the fallback bypass the
	// budget and hammer them. Returns nil (fall through to the fallback) whenever
	// any non-over-budget account exists, so it never false-rejects a healthy pool.
	if gateErr := s.dailyBudgetHardGate(available, cfg, now); gateErr != nil {
		selectorLogEntry(ctx).Warnf(
			"adaptive-select: daily-budget-hardgate | every serving account is warming and over its daily budget, denying (retryable) provider=%s model=%s",
			provider, model,
		)
		return nil, gateErr
	}
	// Harden P0(b) concurrency hard gate: if the ONLY servable accounts are warming
	// accounts already at their in-flight concurrency ceiling, deny (retryable)
	// instead of degrading to the round-robin fallback -- which ignores the
	// concurrency gate entirely and would re-admit a concurrency-full warming
	// account (the same thin-pool bypass the daily-budget hard gate closes). Scoped
	// so a mature / under-headroom / non-adaptive alternative always routes
	// normally (a mature account in the pool guarantees this returns nil, PROD-3b).
	if gateErr := s.concurrencyHardGate(available, cfg, now); gateErr != nil {
		selectorLogEntry(ctx).Warnf(
			"adaptive-select: concurrency-hardgate | every serving account is warming and at its concurrency ceiling, denying (retryable) provider=%s model=%s",
			provider, model,
		)
		return nil, gateErr
	}
	// Degraded: no adaptive-weighted candidate exists at all (a non-adaptive
	// provider, or a pool whose every account scores zero weight). A pool whose
	// every warming account is over its daily budget does NOT reach here -- the
	// hard gate above denies that thin-pool case. A pool that merely has all token
	// buckets momentarily drained does NOT reach here either -- pickFromCandidates
	// serves that as a weighted overflow above. Serve via the fallback selector
	// rather than deny -- the token bucket smooths, it never manufactures a 429.
	picked, errFallback := s.fallback.Pick(ctx, provider, model, opts, auths)
	if errFallback == nil {
		s.logPick(ctx, "fallback-degraded", provider, model, "", picked, cfg, now)
	}
	return picked, errFallback
}

// logPick emits exactly one Info line per resolved Pick, mirroring the
// SessionAffinitySelector log style in selector.go (reusing selectorLogEntry so
// the request_id field is attached, and truncateSessionID for the session id)
// and adding the adaptive-specific tier and selection weight. Without it,
// routing.strategy=adaptive is invisible in main.log: AdaptiveSelector returns
// directly and bypasses SessionAffinitySelector, whose Info lines are the only
// per-request account-hit logging today, so V1 could not observe which account
// each request landed on or the resulting distribution.
//
// reason distinguishes the branch that produced the pick so a preserved sticky
// binding (sticky-keep-*) reads differently from a fresh weighted reselection
// (rebind-* / weighted-new) or a degraded fallback (fallback-degraded). It is
// called once per Pick at each mutually-exclusive terminal decision point, so
// it never double-logs. A nil pick (only reachable on a fallback error path the
// caller already excludes) is skipped defensively.
func (s *AdaptiveSelector) logPick(ctx context.Context, reason, provider, model, sessionID string, picked *Auth, cfg internalconfig.AccountSchedulingConfig, now time.Time) {
	if picked == nil {
		return
	}
	selectorLogEntry(ctx).Infof(
		"adaptive-select: %s | session=%s auth=%s tier=%s weight=%.3f provider=%s model=%s",
		reason, truncateSessionID(sessionID), picked.ID, adaptiveTierLabel(picked), AccountSelectionWeight(picked, cfg, now), provider, model,
	)
}

// adaptiveTierLabel renders picked's fine-grained subscription tier for the
// selection log, namespaced by provider (claude -> "max_20x"/"max_5x"/"pro"/
// "unknown", codex -> "codex_pro"/"codex_plus"/"codex_unknown"). A provider this
// scheduler does not tier-weight is logged as its raw provider name so a
// fallback / non-adaptive pick is still legible in main.log.
func adaptiveTierLabel(a *Auth) string {
	if a == nil {
		return "unknown"
	}
	switch strings.ToLower(strings.TrimSpace(a.Provider)) {
	case "claude":
		return a.ClaudeSubscriptionTier().String()
	case "codex":
		return "codex_" + a.CodexSubscriptionTier().String()
	default:
		return a.Provider
	}
}

// pickWithAffinity handles the session-sticky path (design.md D5). It returns
// handled=false only when no session identity could be extracted, in which case
// the caller falls through to the plain weighted pick. When a session identity
// exists it always fully resolves (bind + return), returning handled=true. It
// also surfaces the branch reason (for the one-line-per-pick observability log)
// and the extracted primary session id, both consumed only by the caller's
// logPick and having no effect on selection.
func (s *AdaptiveSelector) pickWithAffinity(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths, available []*Auth, cfg internalconfig.AccountSchedulingConfig, now time.Time) (picked *Auth, handled bool, reason, sessionID string, err error) {
	primaryID, fallbackID := extractSessionIDs(opts.Headers, opts.OriginalRequest, opts.Metadata)
	if primaryID == "" {
		return nil, false, "", "", nil
	}
	cacheKey := provider + "::" + primaryID + "::" + model

	if boundID, ok := s.cache.GetAndRefresh(cacheKey); ok {
		pickedSticky, stickyReason, errResolve := s.resolveSticky(ctx, provider, model, opts, auths, available, cfg, now, cacheKey, boundID)
		return pickedSticky, true, stickyReason, primaryID, errResolve
	}
	// Inherit a first-turn (short-hash) binding for the full session key so a
	// conversation does not jump credentials once the assistant reply lands (the
	// same inheritance the existing SessionAffinitySelector performs).
	if fallbackID != "" && fallbackID != primaryID {
		fallbackKey := provider + "::" + fallbackID + "::" + model
		if boundID, ok := s.cache.Get(fallbackKey); ok {
			pickedSticky, stickyReason, errResolve := s.resolveSticky(ctx, provider, model, opts, auths, available, cfg, now, cacheKey, boundID)
			return pickedSticky, true, stickyReason, primaryID, errResolve
		}
	}
	pickedNew, newReason, errSelect := s.selectAndBind(ctx, provider, model, opts, auths, available, cfg, now, cacheKey)
	return pickedNew, true, newReason, primaryID, errSelect
}

// resolveSticky applies the design.md D5 maturity grading to an existing sticky
// binding (boundID) for cacheKey:
//
//   - Bound credential no longer available (cooled down / disabled / removed):
//     reselect and rebind.
//   - Bound credential is a non-adaptive provider (no tier weight): keep the
//     binding untouched -- this scheduler owns no smoothing policy for it, so it
//     behaves exactly like the existing session affinity for those providers.
//   - Bound credential is mature and within its soft ceiling (token bucket
//     allows the request): keep the binding, preserving prompt-cache continuity
//     (spec.md "成熟号软上限内保持粘性").
//   - Bound credential is mature but at its ceiling (near the risk hard
//     threshold): reselect and rebind (spec.md "近风控硬阈值才改选").
//   - Bound credential is still warming AND a mature account is available:
//     break stickiness and route to that mature account, rebinding so subsequent
//     turns follow the mature account (spec.md "养号号打破粘性改路由成熟号").
//   - Bound credential is still warming but NO mature account exists to route to
//     (an all-warming pool) AND the bound account can still serve: keep the
//     warming binding (sticky-keep-warming-no-mature). Breaking stickiness here
//     would only swap one equally-young account for another -- it cannot better
//     protect the new account and needlessly discards the session's prompt cache.
//     If instead the bound warming account can no longer serve (hard
//     rate-limited / daily-budget-spent / concurrency-full) it falls through to a
//     full-pool reselection, so the session is never wedged on an unservable
//     binding.
//
// The returned reason string labels which grading branch produced the result
// (for the observability log only; it never affects the selection). "keep"
// reasons denote a preserved sticky binding, "rebind-*" reasons (inherited from
// selectAndBind) denote a fresh weighted/fallback reselection.
func (s *AdaptiveSelector) resolveSticky(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths, available []*Auth, cfg internalconfig.AccountSchedulingConfig, now time.Time, cacheKey, boundID string) (*Auth, string, error) {
	var bound *Auth
	for _, candidate := range available {
		if candidate != nil && candidate.ID == boundID {
			bound = candidate
			break
		}
	}
	if bound == nil {
		return s.selectAndBind(ctx, provider, model, opts, auths, available, cfg, now, cacheKey)
	}
	if !s.adaptiveEligible(bound, cfg) {
		// Non-Claude/Codex sticky target: nothing to grade, keep the binding.
		// Persist it under cacheKey so a binding reached via the first-turn
		// fallback-key inheritance path (pickWithAffinity's fallbackID lookup) is
		// also pinned to the primary/full session key -- mirroring
		// SessionAffinitySelector's fallback-hit rebind at selector.go:489. On the
		// main GetAndRefresh hit path this Set is a harmless refresh (GetAndRefresh
		// already extended the TTL). Without it the primary key is never bound, so
		// every subsequent turn re-derives from the fallback key via the
		// non-refreshing Get, and the binding expires at that fallback key's
		// original (never-extended) TTL mid-session -- the design D5 "成熟号软上限
		// 内保持粘性" stickiness regression this fixes.
		s.cache.Set(cacheKey, bound.ID)
		return bound, "sticky-keep-nonadaptive", nil
	}
	if s.isMature(bound, cfg, now) {
		rpm, burst := s.rateLimitParams(bound, cfg, now)
		if s.limiter.Allow(bound.ID, rpm, burst) {
			// Keep the mature-within-soft-ceiling binding, and persist it under
			// cacheKey for the same reason as the non-adaptive branch above: an
			// inherited first-turn binding must be pinned to the primary session
			// key (refreshing its TTL) instead of surviving only under the fallback
			// key, whose non-refreshing Get would otherwise let the binding expire
			// at its original TTL mid-session (selector.go:489 rebinds identically
			// on its fallback hit).
			s.cache.Set(cacheKey, bound.ID)
			return bound, "sticky-keep-mature", nil
		}
		// At the soft ceiling -> treat as近风控硬阈值, reselect across the pool.
		return s.selectAndBind(ctx, provider, model, opts, auths, available, cfg, now, cacheKey)
	}
	// Warming sticky target. Breaking stickiness is only worthwhile when we can
	// route onto a MORE-protected (mature) account (design.md D5 "养号号打破粘性
	// 改路由成熟号"); try that first. With the weighted-overflow pickFromCandidates
	// this succeeds whenever ANY mature account exists (even one whose bucket is
	// momentarily drained), so the keep guard below is reached exactly when the
	// available pool contains no mature account at all.
	if picked, ok := s.pickFromCandidates(s.scoreCandidates(available, cfg, now, true), cfg, now); ok {
		s.cache.Set(cacheKey, picked.ID)
		return picked, "rebind-weighted-mature", nil
	}
	// No mature account exists to route to (an all-warming pool). Keep the current
	// binding AS LONG AS the bound warming account can still serve, so the session
	// preserves prompt-cache continuity instead of churning credentials every turn
	// (the all-warming-pool stickiness regression this fixes). This keep guard is
	// scoped strictly to the "no mature target" case and never overrides an
	// unservable bound account: a cooled-down / quarantined / removed account is
	// already absent from `available` (handled by the bound==nil reselect above),
	// and a hard rate-limited / daily-budget-spent / concurrency-full bound account
	// fails boundServableForKeep and falls through to the full-pool reselection
	// below, so we never pin the session to a 429-ing binding.
	if s.boundServableForKeep(bound, cfg, now) {
		s.cache.Set(cacheKey, bound.ID)
		return bound, "sticky-keep-warming-no-mature", nil
	}
	// Bound warming account can no longer serve and there is no mature target:
	// reselect across the full pool (may land on another warming account) rather
	// than pin the session to an unservable binding.
	return s.selectAndBind(ctx, provider, model, opts, auths, available, cfg, now, cacheKey)
}

// boundServableForKeep reports whether the still-warming bound sticky account can
// serve this request right now, so resolveSticky may keep its binding when no
// mature account exists to route to (an all-warming pool). It applies the same
// gating order as pickFromCandidates -- daily budget and concurrency headroom
// first (neither consumes a token), then the account's own token bucket -- and,
// exactly like the mature-keep branch, consumes one token on success so the kept
// account is charged for this request. A false result -- over daily budget, at
// its concurrency ceiling, or its token bucket momentarily empty (a hard rate
// limit) -- means the bound account is NOT servable and the caller must reselect
// rather than wedge the session on it. It deliberately does NOT re-check base
// availability / quarantine / cooldown: `bound` was found in the already-filtered
// `available` slice, so its presence there is that check.
func (s *AdaptiveSelector) boundServableForKeep(a *Auth, cfg internalconfig.AccountSchedulingConfig, now time.Time) bool {
	if s.overWarmupBudget(a, cfg, now) {
		return false
	}
	if !s.hasConcurrencyHeadroom(a, cfg, now) {
		return false
	}
	rpm, burst := s.rateLimitParams(a, cfg, now)
	return s.limiter.Allow(a.ID, rpm, burst)
}

// selectAndBind performs a weighted pick over the full available pool and records
// the result under cacheKey. When the weighted pool yields nothing (a
// non-adaptive provider, or no scorable candidate at all) it delegates to the
// fallback selector, still binding the result so the session stays put.
//
// The returned reason string labels which sub-path produced the pick
// ("rebind-weighted" | "rebind-fallback"), for the observability log only -- it
// never affects selection. The mature-preferring reselection a still-warming
// sticky target triggers is handled inline by resolveSticky (which also owns the
// "keep the warming binding when no mature target exists" guard), so this helper
// itself no longer needs a preferMature mode.
func (s *AdaptiveSelector) selectAndBind(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths, available []*Auth, cfg internalconfig.AccountSchedulingConfig, now time.Time, cacheKey string) (*Auth, string, error) {
	// ERR-3 failover mature-only preference threaded through the sticky reselection
	// path: on a failover retry the bound account was already tried and is absent
	// from `available` (so resolveSticky reaches this reselection), and the retry
	// should prefer a mature account here too. scoreFailoverCandidates falls back to
	// the full pool when no mature account exists, so a sticky session on an
	// all-warming fleet still reselects a servable warming account.
	failoverMatureOnly := failoverMatureOnlyFromMetadata(opts.Metadata)
	if picked, ok := s.pickFromCandidates(s.scoreFailoverCandidates(available, cfg, now, failoverMatureOnly), cfg, now); ok {
		s.cache.Set(cacheKey, picked.ID)
		return picked, "rebind-weighted", nil
	}
	// Same hole-2 thin-pool hard gate the non-sticky Pick path applies: a sticky
	// session must not use the fallback to bypass the daily budget onto an
	// all-over-budget warming pool either. Deny (retryable) instead; the binding is
	// left untouched so a later turn (with budget restored or a mature account
	// back) can re-resolve. Only fires when NO non-over-budget account exists.
	if gateErr := s.dailyBudgetHardGate(available, cfg, now); gateErr != nil {
		return nil, "rebind-daily-budget-denied", gateErr
	}
	// Same harden P0(b) concurrency hard gate the non-sticky Pick path applies: a
	// sticky session must not use the fallback to bypass the concurrency gate onto
	// an all-concurrency-full warming pool either. Only fires when NO compliant
	// server exists.
	if gateErr := s.concurrencyHardGate(available, cfg, now); gateErr != nil {
		return nil, "rebind-concurrency-denied", gateErr
	}
	picked, errPick := s.fallback.Pick(ctx, provider, model, opts, auths)
	if errPick == nil && picked != nil {
		s.cache.Set(cacheKey, picked.ID)
	}
	return picked, "rebind-fallback", errPick
}

// adaptiveCandidate pairs a credential with its current selection weight.
type adaptiveCandidate struct {
	auth   *Auth
	weight float64
}

// scoreCandidates scores every available credential with AccountSelectionWeight,
// dropping non-positive weights (non-adaptive providers, or a tier configured to
// weight 0) and -- when matureOnly is set -- every still-warming account. The
// result is sorted by auth ID so a given rng value maps to a deterministic pick,
// which keeps weighted selection reproducible in tests.
func (s *AdaptiveSelector) scoreCandidates(available []*Auth, cfg internalconfig.AccountSchedulingConfig, now time.Time, matureOnly bool) []adaptiveCandidate {
	candidates := make([]adaptiveCandidate, 0, len(available))
	for _, candidate := range available {
		if candidate == nil {
			continue
		}
		if matureOnly && !s.isMature(candidate, cfg, now) {
			continue
		}
		weight := AccountSelectionWeight(candidate, cfg, now)
		if weight <= 0 {
			continue
		}
		if s.overWarmupBudget(candidate, cfg, now) {
			// Warming account has spent its rolling-24h request budget (P2, design
			// §5.1's primary warm-up throttle) OR its billable-token budget (P3).
			// Drop it from this pick so traffic routes to accounts with budget
			// left -- mature accounts have no daily cap and so承接 the overflow
			// (design D4). The budget frees again as the rolling window advances.
			continue
		}
		candidates = append(candidates, adaptiveCandidate{auth: candidate, weight: weight})
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].auth.ID < candidates[j].auth.ID })
	return candidates
}

// scoreFailoverCandidates scores the available pool for a fresh (non-sticky-keep)
// weighted pick, honoring the ERR-3 failover mature-only preference. When
// matureOnly is set (a failover retry -- the request has already tried and failed
// at least one credential) it first scores ONLY mature accounts, so retry traffic
// prefers成熟号 over养号号; and ONLY when that yields no candidate (an all-warming
// fleet, with no mature account to route to) does it fall back to scoring the full
// pool, so a pure warming fleet's failover still serves rather than hard-failing
// (兜底红线). When matureOnly is false it is exactly scoreCandidates(...,false) --
// the unchanged first-attempt behavior. Because mature accounts always pass the
// mature filter, this never locally excludes or 429s a mature account (PROD-3b);
// the empty-mature fallback is the only path that widens the pool, and it widens
// it to the same set the pre-ERR-3 code always used.
func (s *AdaptiveSelector) scoreFailoverCandidates(available []*Auth, cfg internalconfig.AccountSchedulingConfig, now time.Time, matureOnly bool) []adaptiveCandidate {
	if matureOnly {
		if mature := s.scoreCandidates(available, cfg, now, true); len(mature) > 0 {
			return mature
		}
	}
	return s.scoreCandidates(available, cfg, now, false)
}

// pickFromCandidates draws one credential from candidates proportional to
// weight, gating each draw on the account's own token bucket: a rate-limited
// draw is dropped (no token consumed -- AccountRateLimiter.Allow only consumes
// on success) and the draw repeats over the remaining pool. On the servable path
// it consumes exactly one token, always for the returned account.
//
// It returns ok=false when candidates is empty (a non-adaptive provider, or a
// pool whose every account scores zero weight / is over its warm-up budget) OR
// (harden P0(a)) when the overflow pool is empty because every non-mature
// candidate is at its concurrency ceiling, leaving the caller to run the
// concurrency / daily hard gates or degrade to the round-robin fallback. When the
// pool is non-empty and at least one candidate can still absorb overflow (a mature
// account, or a warming account with a free in-flight slot), it does NOT deny: the
// token bucket is an outbound smoother, never a hard gate, so it draws one final
// candidate proportional to weight over that overflow pool and returns it
// (ok=true). No token is consumed on that overflow draw -- every bucket is empty,
// there is none to take -- and, critically, the draw stays WEIGHTED (reusing
// weightedIndex) so a rate-limit overflow keeps routing proportionally to tier
// capacity instead of collapsing onto the uniform round-robin fallback (which
// would flatten a Max 20x account into an equal share with a Pro account).
func (s *AdaptiveSelector) pickFromCandidates(candidates []adaptiveCandidate, cfg internalconfig.AccountSchedulingConfig, now time.Time) (*Auth, bool) {
	if len(candidates) == 0 {
		return nil, false
	}
	pool := make([]adaptiveCandidate, len(candidates))
	copy(pool, candidates)
	for len(pool) > 0 {
		idx := s.weightedIndex(pool)
		candidate := pool[idx]
		if s.antiStreakShouldSkip(candidate.auth, cfg, now, len(pool)) {
			// Harden P1a anti-streak: this warming account has been selected
			// AntiStreakLimit times in a row and an alternative exists -- rotate
			// off it WITHOUT consuming a rate-limit token, cutting the
			// instantaneous concentration. Long-term per-tier share is unchanged
			// (rotation only after the streak cap, only with an alternative).
			pool = append(pool[:idx], pool[idx+1:]...)
			continue
		}
		if !s.hasConcurrencyHeadroom(candidate.auth, cfg, now) {
			// Account already at its in-flight concurrency ceiling. Drop it
			// WITHOUT consuming a rate-limit token (the concurrency check comes
			// before Allow for exactly this reason) and try the next weighted
			// candidate; a mature account with a higher ceiling naturally
			// absorbs the overflow.
			pool = append(pool[:idx], pool[idx+1:]...)
			continue
		}
		rpm, burst := s.rateLimitParams(candidate.auth, cfg, now)
		if s.limiter.Allow(candidate.auth.ID, rpm, burst) {
			s.notePick(candidate.auth, cfg, now)
			return candidate.auth, true
		}
		pool = append(pool[:idx], pool[idx+1:]...)
	}
	// Overflow: the pool is non-empty but every weighted candidate is momentarily
	// over its own token bucket (or at its advisory concurrency ceiling). Serve a
	// weighted draw, but ONLY over accounts that may still absorb overflow: mature
	// accounts (which keep their overflow tolerance so a mature account is never
	// handed a locally-manufactured 429, PROD-3b) plus warming accounts that still
	// have a free in-flight slot. A warming account already AT its concurrency
	// ceiling is excluded here so the per-account concurrency gate is真受约束 for
	// warming main traffic (harden P0(a)) instead of the pre-harden behavior of
	// re-admitting it over the original set. An empty overflow pool returns
	// ok=false so the caller runs the concurrency / daily hard gates (or the
	// fallback) rather than force a stream onto an over-ceiling warming account.
	overflow := make([]adaptiveCandidate, 0, len(candidates))
	for _, c := range candidates {
		if s.isMature(c.auth, cfg, now) || s.hasConcurrencyHeadroom(c.auth, cfg, now) {
			overflow = append(overflow, c)
		}
	}
	if len(overflow) == 0 {
		return nil, false
	}
	picked := overflow[s.weightedIndex(overflow)].auth
	s.notePick(picked, cfg, now)
	return picked, true
}

// weightedIndex returns an index into pool chosen proportional to each entry's
// weight, using s.rng() in [0,1). A non-positive total (should not happen -- the
// caller drops non-positive weights) degrades to index 0.
func (s *AdaptiveSelector) weightedIndex(pool []adaptiveCandidate) int {
	total := 0.0
	for _, candidate := range pool {
		total += candidate.weight
	}
	if total <= 0 {
		return 0
	}
	target := s.rng() * total
	acc := 0.0
	for i, candidate := range pool {
		acc += candidate.weight
		if target < acc {
			return i
		}
	}
	return len(pool) - 1
}

// rateLimitParams derives the token-bucket rpm/burst for an account at its
// current warm-up stage. rpm is the stage's (or mature ceiling's) rpm limit;
// burst is the mature burst allowance for a mature account, otherwise the
// stage's concurrency limit (a small, tight burst while warming). A burst below
// 1 is clamped up so a lone request is never wedged behind a zero-capacity
// bucket (AccountRateLimiter.Allow clamps too; this keeps the intent explicit).
func (s *AdaptiveSelector) rateLimitParams(a *Auth, cfg internalconfig.AccountSchedulingConfig, now time.Time) (rpm float64, burst int) {
	status := AccountWarmupStatusFor(a, now, cfg)
	rpm = float64(status.RPMLimit)
	if status.Mature {
		burst = cfg.MatureLimits.Burst
	} else {
		burst = status.ConcurrencyLimit
	}
	// Apply the per-account safety-test rate multiplier (design §8.3) AFTER the
	// tier/warm-up derivation, so it scales the ceiling the account already sits
	// at (warming or mature) without touching selection weight. rate_scale=1.0 is
	// a no-op; a fractional scale is floored so a lone request is never wedged.
	scale := AccountRateScale(a, cfg)
	rpm = scaleLimitRPM(rpm, scale)
	burst = scaleLimitInt(burst, scale)
	// Harden P3 quota-aware pacing: softly slow a still-WARMING account's rpm when
	// it is out-pacing its fair share to reset (or when its quota burn sample is
	// stale -- fail safe to the floor). Applied AFTER the stage/scale derivation so
	// it composes on the ceiling the account already sits at, and ONLY to rpm (never
	// burst). Mature accounts are deliberately untouched (无感): status.Mature short-
	// circuits before the multiplier is even read, so the mature max_20x:5x:pro
	// smoothing profile is byte-identical to before.
	if rpm > 0 && !status.Mature {
		if pace := s.pacingRPMMultiplier(a, now); pace < 1 {
			rpm *= pace
		}
	}
	if burst < 1 {
		burst = 1
	}
	return rpm, burst
}

// isMature reports whether a is past its warm-up curve, derived from the
// warm-up-status view (account_warmup.go) rather than account_weight.go's
// AccountIsMature. The two sibling helpers deliberately disagree on the
// no-anchor case -- AccountIsMature treats a credential with no
// first_production_at anchor as mature (so the weighted score is not perpetually
// starved), while AccountWarmupStatusFor treats it as "cold" (not mature). This
// selector keeps a single internal source of truth by using the warm-up-status
// view for BOTH maturity grading and rate-limit params, so the same account is
// never simultaneously "mature" for stickiness and "cold" for rate limiting. The
// warm-up-status view is also the more conservative (anti-ban fail-safe)
// interpretation: an un-anchored Claude/Codex credential does not hold
// stickiness or absorb floods until it has actually been anchored (which the
// wiring slice is expected to do on real first production use -- see gaps).
func (s *AdaptiveSelector) isMature(a *Auth, cfg internalconfig.AccountSchedulingConfig, now time.Time) bool {
	return AccountWarmupStatusFor(a, now, cfg).Mature
}

// adaptiveEligible reports whether a is a provider/tier this scheduler actually
// scores (positive configured tier base weight -- claude/codex today). Used to
// leave non-adaptive providers' sticky bindings ungraded.
func (s *AdaptiveSelector) adaptiveEligible(a *Auth, cfg internalconfig.AccountSchedulingConfig) bool {
	return a != nil && a.AccountTierBaseWeight(cfg.TierWeights) > 0
}

// AccountGate exposes the per-account concurrency + daily-budget gate this
// selector maintains (account_gate.go) so the auth Manager's execution path can
// drive the SAME instance (acquire/release an in-flight slot, record a request)
// that Pick gates against -- keeping the selector's avoidance and the execution
// path's accounting on one live count. It satisfies the accountGateProvider
// contract the Manager type-asserts on. Never nil after construction.
func (s *AdaptiveSelector) AccountGate() *AccountConcurrencyGate {
	return s.gate
}

// dailyBudgetHardGate returns errAccountDailyBudgetExhausted (a retryable 429)
// when the ONLY reason no adaptive candidate can be served is that every account
// able to serve this request is a still-warming account that has already spent
// its UTC-daily warm-up budget. In that thin-pool case the round-robin fallback
// would bypass the daily budget and hammer the very account warm-up is protecting
// (the hole-2 hard-gate fix), so the caller must deny rather than fall back onto
// it.
//
// It is called ONLY on the empty-candidate path (pickFromCandidates returned
// ok=false); a non-empty candidate set is always served (as a weighted overflow
// at worst) and never reaches here. It returns nil -- letting the caller degrade
// to the fallback exactly as before -- in every other case, so it can NEVER
// manufacture a denial while any servable alternative exists:
//
//   - The first available account that is NOT over its daily budget
//     short-circuits to nil. That covers a mature account (DailyBudget 0 =
//     unbounded), a warming account with budget left, and any non-adaptive /
//     tier-0 account (the gate never counts requests for providers the scheduler
//     does not manage, so overDailyBudget is always false for them -- design D7
//     backward compatibility is preserved).
//   - A pool with no over-budget account at all (an empty pool, or a purely
//     non-adaptive pool) returns nil: not this gate's concern.
//
// `available` is the same post-preferCodexWebsocket pool the fallback selector
// re-derives and serves over (RoundRobinSelector.Pick applies the identical
// filter), so this examination exactly predicts what the fallback would otherwise
// hand out -- the gate denies precisely the requests the fallback would have
// mis-served, and no others.
func (s *AdaptiveSelector) dailyBudgetHardGate(available []*Auth, cfg internalconfig.AccountSchedulingConfig, now time.Time) error {
	hasOverBudget := false
	for _, a := range available {
		if a == nil {
			continue
		}
		if s.overWarmupBudget(a, cfg, now) {
			hasOverBudget = true
			continue
		}
		// A servable account that is NOT over its warm-up budget exists; the
		// fallback can serve it, so never deny (a concurrency-full-but-under-budget
		// account still defers to concurrencyHardGate downstream).
		return nil
	}
	if !hasOverBudget {
		return nil
	}
	return errAccountDailyBudgetExhausted()
}

// overDailyBudget reports whether a is a warming account that has already met or
// exceeded its configured UTC-daily budget. Mature accounts (DailyBudget 0 =
// unbounded, design §5.1) always return false, as does a stage with no
// configured daily budget or (defensively) a nil gate.
func (s *AdaptiveSelector) overDailyBudget(a *Auth, cfg internalconfig.AccountSchedulingConfig, now time.Time) bool {
	if s.gate == nil || a == nil {
		return false
	}
	status := AccountWarmupStatusFor(a, now, cfg)
	if status.Mature || status.DailyBudget <= 0 {
		return false
	}
	// Scale the warm-up daily budget by the per-account rate multiplier (§8.3)
	// so a fractional rate_scale tightens the budget too, matching the rpm /
	// concurrency scaling. scaleLimitInt floors a positive budget at 1.
	budget := scaleLimitInt(status.DailyBudget, AccountRateScale(a, cfg))
	// Harden P2: pass the persisted rolling-window buckets from the (possibly
	// stale) auth clone so a cold gate re-seeds this account's already-spent budget
	// after a process restart instead of resetting it to 0 (fail-open fix). The
	// seed is honored only when the gate has no live entry yet; the in-memory count
	// is authoritative thereafter, so a stale clone is harmless.
	seed := readDailyWindowBuckets(a.Metadata, accountSchedulingDailyWindowKey)
	return s.gate.OverDailyBudgetWindow(a.ID, budget, seed)
}

// overTokenBudget reports whether a is a warming account that has met or exceeded
// its configured billable-token daily budget over the rolling 24h window (harden
// P3). Mature accounts (unbounded) and any account/stage with no configured token
// budget return false. It is INERT until the billable-token counting sink
// (internal/usage, a separate slice) records tokens into the gate: until then
// every account's token count is 0, so this always returns false and adds no
// behavior on its own -- the mechanism and config knob land now, the sink later.
func (s *AdaptiveSelector) overTokenBudget(a *Auth, cfg internalconfig.AccountSchedulingConfig, now time.Time) bool {
	if s.gate == nil || a == nil {
		return false
	}
	if AccountWarmupStatusFor(a, now, cfg).Mature {
		return false
	}
	budget := s.tokenDailyBudgetFor(a, cfg, now)
	if budget <= 0 {
		return false
	}
	budget = scaleLimitInt(budget, AccountRateScale(a, cfg))
	seed := readDailyWindowBuckets(a.Metadata, accountSchedulingTokenWindowKey)
	return s.gate.OverTokenBudget(a.ID, budget, seed)
}

// tokenDailyBudgetFor resolves the billable-token daily budget for a at its
// current warm-up stage (harden P3). AccountWarmupStatus does not itself carry the
// token budget (adding it would touch the account_warmup.go stage-resolution
// slice, out of this change's file scope), so this re-derives it from the same
// resolved StageName: the matching warmup-curve stage, curve[0] for the
// not-yet-anchored "cold" state, or MatureLimits when mature. 0 = unbounded.
func (s *AdaptiveSelector) tokenDailyBudgetFor(a *Auth, cfg internalconfig.AccountSchedulingConfig, now time.Time) int {
	return resolveTokenDailyBudget(cfg, AccountWarmupStatusFor(a, now, cfg))
}

// resolveTokenDailyBudget resolves the billable-token daily budget for a resolved
// warm-up status against cfg (harden P3). It is the shared free-function form of
// tokenDailyBudgetFor so the Manager's token sink (recordBillableTokensForAccount)
// resolves the SAME budget the selector's overTokenBudget gate reads, without
// duplicating the stage-lookup logic. 0 = unbounded.
func resolveTokenDailyBudget(cfg internalconfig.AccountSchedulingConfig, status AccountWarmupStatus) int {
	if status.Mature {
		return cfg.MatureLimits.TokenDailyBudget
	}
	for _, stage := range cfg.WarmupCurve {
		if stage.Name == status.StageName {
			return stage.TokenDailyBudget
		}
	}
	// "cold" (no anchor yet) resolves to the first (most restrictive) stage's
	// limits -- mirror that here for its token budget too.
	if len(cfg.WarmupCurve) > 0 {
		return cfg.WarmupCurve[0].TokenDailyBudget
	}
	return 0
}

// -----------------------------------------------------------------------------
// P3 billable-token sink registration (harden-account-scheduling-limiter).
//
// The billable-token gate/bucket landed inert in the first batch (no sink fed it,
// so every token count stayed 0). This is the sink hook: internal/usage (which
// imports this package) calls RecordAccountBillableTokens per completed request,
// and it routes to the Manager method registered here via a package var. Keeping
// the reference as a package var -- rather than an internal/usage -> Manager import
// -- avoids an import cycle (internal/usage already imports this package) and keeps
// the mechanism inert until a Manager registers.
// -----------------------------------------------------------------------------

// accountBillableTokenSink holds the active Manager's bound token-recording method.
var accountBillableTokenSink atomic.Pointer[func(authID string, billableTokens int)]

// RegisterAccountBillableTokenSink installs (or, with nil, clears) the billable-
// token sink. The Manager registers its own bound method on config apply.
func RegisterAccountBillableTokenSink(fn func(authID string, billableTokens int)) {
	if fn == nil {
		accountBillableTokenSink.Store(nil)
		return
	}
	accountBillableTokenSink.Store(&fn)
}

// RecordAccountBillableTokens routes a completed request's non-cache-read billable
// token count to the registered sink (harden P3). Called from internal/usage's
// per-request record path. A non-positive count or an unregistered sink is a no-op,
// so with no adaptive Manager / no token budget configured this changes nothing.
func RecordAccountBillableTokens(authID string, billableTokens int) {
	if billableTokens <= 0 {
		return
	}
	if fn := accountBillableTokenSink.Load(); fn != nil {
		(*fn)(authID, billableTokens)
	}
}

// pacingSnapshotStaleAfter is how old the last quota burn sample may be before the
// pacing multiplier fails safe to the floor for a warming account. Package var so a
// future config-wiring slice can tune it without an API change.
var pacingSnapshotStaleAfter = 15 * time.Minute

// pacingRPMMultiplier returns the quota-aware pacing multiplier for a WARMING
// account (harden P3). The caller MUST only apply it to warming accounts (mature
// accounts stay无感). It is 1 (no slowdown) unless the account has a fresh burn
// signal showing it is out-pacing its fair share to reset, in which case it is that
// dry-run pacing factor; a burn sample older than pacingSnapshotStaleAfter fails
// safe to the pacing floor (stale quota headroom cannot be trusted, so slow the
// account we are protecting). An account with NO burn history yet (a brand-new /
// never-quota-polled account, or a provider whose quota this subsystem does not
// poll) is NOT throttled -- multiplier 1 -- so warm-up is never frozen for lack of
// data.
func (s *AdaptiveSelector) pacingRPMMultiplier(a *Auth, now time.Time) float64 {
	st := ReadAccountBurnState(a)
	if st.HasPrev && now.Sub(st.PrevAt) > pacingSnapshotStaleAfter {
		return PacingFactorFloor
	}
	obs := AccountPacingObservabilityFor(a, now)
	if !obs.HasPacingFactor {
		return 1
	}
	return obs.PacingFactorDryRun
}

// overWarmupBudget is the combined warm-up budget predicate: a warming account is
// "over budget" -- and so is dropped from selection and can trip the thin-pool
// hard gate -- if it has spent EITHER its rolling-24h request budget (P2) OR its
// rolling-24h billable-token budget (P3). Mature accounts are never over budget.
// Because the token window is inert until its sink is wired, this is byte-for-byte
// equivalent to overDailyBudget until then.
func (s *AdaptiveSelector) overWarmupBudget(a *Auth, cfg internalconfig.AccountSchedulingConfig, now time.Time) bool {
	return s.overDailyBudget(a, cfg, now) || s.overTokenBudget(a, cfg, now)
}

// canServeCompliantly reports whether a can serve a request right now without
// violating a warm-up budget or a warming account's concurrency ceiling. It is
// the "any servable alternative exists" probe the thin-pool hard gates use to stay
// strictly scoped (find one servable account and short-circuit to nil, "宁漏拦不
// 误拦"):
//   - a non-adaptive account is never gated (design D7) -> always servable;
//   - a mature account keeps overflow tolerance (never locally 429'd) -> servable;
//   - a warming account is servable only while under budget AND with a free slot.
func (s *AdaptiveSelector) canServeCompliantly(a *Auth, cfg internalconfig.AccountSchedulingConfig, now time.Time) bool {
	if a == nil {
		return false
	}
	if !s.adaptiveEligible(a, cfg) {
		return true
	}
	if s.isMature(a, cfg, now) {
		return true
	}
	if s.overWarmupBudget(a, cfg, now) {
		return false
	}
	return s.hasConcurrencyHeadroom(a, cfg, now)
}

// concurrencyHardGate returns errAccountConcurrencyBusy (a retryable 429) when the
// ONLY reason no adaptive candidate can be served is that every servable account
// is a still-warming account already at its in-flight concurrency ceiling (harden
// P0(b)). Like dailyBudgetHardGate it is called only on the empty-candidate path
// and denies precisely the requests the round-robin fallback would otherwise
// mis-serve onto a concurrency-full warming account (the fallback ignores the
// concurrency gate entirely).
//
// It NEVER denies while any compliant server exists (a mature account, an
// under-budget warming account with a free slot, or any non-adaptive account), so
// a mature account in the pool guarantees nil (PROD-3b: a mature account is never
// the cause of a local 429). It returns nil unless it both finds no compliant
// server AND observes at least one concurrency-full warming account, so an
// all-over-budget pool (dailyBudgetHardGate's concern) or a genuinely empty pool
// falls through unchanged. Mirrors dailyBudgetHardGate's short-circuit ordering:
// the first compliant server returns nil immediately.
func (s *AdaptiveSelector) concurrencyHardGate(available []*Auth, cfg internalconfig.AccountSchedulingConfig, now time.Time) error {
	concurrencyFullWarming := false
	for _, a := range available {
		if a == nil {
			continue
		}
		if s.canServeCompliantly(a, cfg, now) {
			return nil
		}
		if s.adaptiveEligible(a, cfg) && !s.isMature(a, cfg, now) &&
			!s.overWarmupBudget(a, cfg, now) && !s.hasConcurrencyHeadroom(a, cfg, now) {
			concurrencyFullWarming = true
		}
	}
	if concurrencyFullWarming {
		return errAccountConcurrencyBusy("")
	}
	return nil
}

// antiStreakShouldSkip reports whether the harden P1a anti-streak rule should skip
// picking a on this draw: only when enabled (cfg.AntiStreakLimit > 0), an
// alternative exists in the pool (poolSize > 1), a is a still-WARMING account
// (mature accounts' long-term share is left untouched), and a has already been
// picked AntiStreakLimit times in a row. Skipping forces the draw to rotate to
// another available account, cutting the instantaneous承流 concentration without
// changing the long-term per-tier share (rotation only fires after the streak cap
// and only when an alternative exists).
func (s *AdaptiveSelector) antiStreakShouldSkip(a *Auth, cfg internalconfig.AccountSchedulingConfig, now time.Time, poolSize int) bool {
	if cfg.AntiStreakLimit <= 0 || poolSize <= 1 || a == nil {
		return false
	}
	if !s.adaptiveEligible(a, cfg) || s.isMature(a, cfg, now) {
		return false
	}
	s.pickMu.Lock()
	defer s.pickMu.Unlock()
	return a.ID == s.lastWarmPickID && s.warmPickStreak >= cfg.AntiStreakLimit
}

// notePick records the resolved pick for the anti-streak rule (a no-op unless
// enabled). A repeated warming account advances the streak; a different warming
// account starts a new streak; a mature / non-adaptive pick clears it (the streak
// only tracks CONSECUTIVE warming picks). Called exactly once per resolved
// weighted/overflow pick.
func (s *AdaptiveSelector) notePick(a *Auth, cfg internalconfig.AccountSchedulingConfig, now time.Time) {
	if cfg.AntiStreakLimit <= 0 || a == nil {
		return
	}
	warming := s.adaptiveEligible(a, cfg) && !s.isMature(a, cfg, now)
	s.pickMu.Lock()
	defer s.pickMu.Unlock()
	if !warming {
		s.lastWarmPickID = ""
		s.warmPickStreak = 0
		return
	}
	if a.ID == s.lastWarmPickID {
		s.warmPickStreak++
	} else {
		s.lastWarmPickID = a.ID
		s.warmPickStreak = 1
	}
}

// hasConcurrencyHeadroom reports whether a still has a free in-flight slot under
// its current stage (or mature) ConcurrencyLimit. AccountWarmupStatus already
// resolves that single limit for both warming and mature accounts. A
// non-positive limit ("no ceiling configured for this stage") or a nil gate
// always reports headroom. This is an advisory Pick-time read; the authoritative
// slot reservation happens on the execution path (beginAccountExecution), so a
// small race between this read and that acquire can transiently over-admit by
// one -- the accepted soft-ceiling behavior.
func (s *AdaptiveSelector) hasConcurrencyHeadroom(a *Auth, cfg internalconfig.AccountSchedulingConfig, now time.Time) bool {
	if s.gate == nil || a == nil {
		return true
	}
	limit := AccountWarmupStatusFor(a, now, cfg).ConcurrencyLimit
	// Scale the concurrency ceiling by the per-account rate multiplier (§8.3),
	// consistent with the rpm/daily-budget scaling. scaleLimitInt preserves 0
	// ("no ceiling configured") and floors a positive limit at 1.
	limit = scaleLimitInt(limit, AccountRateScale(a, cfg))
	if limit <= 0 {
		return true
	}
	return s.gate.InFlight(a.ID) < limit
}

// InvalidateAuth removes every sticky binding pointing at authID. The auth
// Manager calls this (via an interface assertion, see
// conductor_lifecycle.go) when a credential cools down or is removed, so a
// session does not keep resolving to a dead account.
func (s *AdaptiveSelector) InvalidateAuth(authID string) {
	if s.cache != nil {
		s.cache.InvalidateAuth(authID)
	}
}

// Stop releases the selector's owned resources (session cache cleanup goroutine
// and, if the limiter is owned rather than injected, its reclaim loop). It
// implements StoppableSelector and is safe to call more than once. An injected
// rate limiter is left running for its owner to Stop.
func (s *AdaptiveSelector) Stop() {
	if s.cache != nil {
		s.cache.Stop()
	}
	if s.ownsLimiter && s.limiter != nil {
		s.limiter.Stop()
	}
}
