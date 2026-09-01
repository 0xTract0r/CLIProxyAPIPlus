package auth

import (
	"context"
	"math/rand"
	"sort"
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
// as it does today. Likewise, if every weighted candidate is momentarily
// rate-limited, the request is served via the fallback selector (spreading the
// overflow across the pool) rather than being denied -- the token bucket is an
// outbound smoother, never a hard gate that can manufacture a 429 the upstream
// did not send.
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

	// cache holds session -> auth stickiness bindings. Nil when session
	// affinity is disabled.
	cache           *SessionCache
	sessionAffinity bool

	// now / rng are injectable for deterministic tests (production defaults:
	// time.Now, rand.Float64). rng MUST return a value in [0,1) and, in
	// production, MUST be safe for concurrent use (rand.Float64 is).
	now func() time.Time
	rng func() float64
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
		if picked, handled, errPick := s.pickWithAffinity(ctx, provider, model, opts, auths, available, cfg, now); handled {
			return picked, errPick
		}
	}

	if picked, ok := s.pickFromCandidates(s.scoreCandidates(available, cfg, now, false), cfg, now); ok {
		return picked, nil
	}
	// Degraded: no adaptive-weighted candidate could be served right now (a
	// non-adaptive provider, or every weighted candidate is momentarily over its
	// own rate limit). Serve via the fallback selector rather than deny -- the
	// token bucket smooths, it never manufactures a 429.
	return s.fallback.Pick(ctx, provider, model, opts, auths)
}

// pickWithAffinity handles the session-sticky path (design.md D5). It returns
// handled=false only when no session identity could be extracted, in which case
// the caller falls through to the plain weighted pick. When a session identity
// exists it always fully resolves (bind + return), returning handled=true.
func (s *AdaptiveSelector) pickWithAffinity(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths, available []*Auth, cfg internalconfig.AccountSchedulingConfig, now time.Time) (*Auth, bool, error) {
	primaryID, fallbackID := extractSessionIDs(opts.Headers, opts.OriginalRequest, opts.Metadata)
	if primaryID == "" {
		return nil, false, nil
	}
	cacheKey := provider + "::" + primaryID + "::" + model

	if boundID, ok := s.cache.GetAndRefresh(cacheKey); ok {
		picked, errResolve := s.resolveSticky(ctx, provider, model, opts, auths, available, cfg, now, cacheKey, boundID)
		return picked, true, errResolve
	}
	// Inherit a first-turn (short-hash) binding for the full session key so a
	// conversation does not jump credentials once the assistant reply lands (the
	// same inheritance the existing SessionAffinitySelector performs).
	if fallbackID != "" && fallbackID != primaryID {
		fallbackKey := provider + "::" + fallbackID + "::" + model
		if boundID, ok := s.cache.Get(fallbackKey); ok {
			picked, errResolve := s.resolveSticky(ctx, provider, model, opts, auths, available, cfg, now, cacheKey, boundID)
			return picked, true, errResolve
		}
	}
	picked, errSelect := s.selectAndBind(ctx, provider, model, opts, auths, available, cfg, now, cacheKey, false)
	return picked, true, errSelect
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
//   - Bound credential is still warming: break stickiness and route to a mature
//     account, rebinding so subsequent turns follow the mature account (spec.md
//     "养号号打破粘性改路由成熟号").
func (s *AdaptiveSelector) resolveSticky(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths, available []*Auth, cfg internalconfig.AccountSchedulingConfig, now time.Time, cacheKey, boundID string) (*Auth, error) {
	var bound *Auth
	for _, candidate := range available {
		if candidate != nil && candidate.ID == boundID {
			bound = candidate
			break
		}
	}
	if bound == nil {
		return s.selectAndBind(ctx, provider, model, opts, auths, available, cfg, now, cacheKey, false)
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
		return bound, nil
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
			return bound, nil
		}
		// At the soft ceiling -> treat as近风控硬阈值, reselect across the pool.
		return s.selectAndBind(ctx, provider, model, opts, auths, available, cfg, now, cacheKey, false)
	}
	// Warming sticky target -> break stickiness, prefer a mature account.
	return s.selectAndBind(ctx, provider, model, opts, auths, available, cfg, now, cacheKey, true)
}

// selectAndBind performs a weighted pick (optionally restricted to mature
// accounts) and records the result under cacheKey. When preferMature yields no
// servable mature candidate it falls back to the full weighted pool, and when
// even that yields nothing it delegates to the fallback selector (still binding
// the result so the session stays put).
func (s *AdaptiveSelector) selectAndBind(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths, available []*Auth, cfg internalconfig.AccountSchedulingConfig, now time.Time, cacheKey string, preferMature bool) (*Auth, error) {
	if preferMature {
		if picked, ok := s.pickFromCandidates(s.scoreCandidates(available, cfg, now, true), cfg, now); ok {
			s.cache.Set(cacheKey, picked.ID)
			return picked, nil
		}
	}
	if picked, ok := s.pickFromCandidates(s.scoreCandidates(available, cfg, now, false), cfg, now); ok {
		s.cache.Set(cacheKey, picked.ID)
		return picked, nil
	}
	picked, errPick := s.fallback.Pick(ctx, provider, model, opts, auths)
	if errPick == nil && picked != nil {
		s.cache.Set(cacheKey, picked.ID)
	}
	return picked, errPick
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
		candidates = append(candidates, adaptiveCandidate{auth: candidate, weight: weight})
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].auth.ID < candidates[j].auth.ID })
	return candidates
}

// pickFromCandidates draws one credential from candidates proportional to
// weight, gating each draw on the account's own token bucket: a rate-limited
// draw is dropped (no token consumed -- AccountRateLimiter.Allow only consumes
// on success) and the draw repeats over the remaining pool. It returns ok=false
// only when the pool is empty or every candidate is currently rate-limited, so
// exactly one token is ever consumed per Pick and always for the returned
// account.
func (s *AdaptiveSelector) pickFromCandidates(candidates []adaptiveCandidate, cfg internalconfig.AccountSchedulingConfig, now time.Time) (*Auth, bool) {
	if len(candidates) == 0 {
		return nil, false
	}
	pool := make([]adaptiveCandidate, len(candidates))
	copy(pool, candidates)
	for len(pool) > 0 {
		idx := s.weightedIndex(pool)
		candidate := pool[idx]
		rpm, burst := s.rateLimitParams(candidate.auth, cfg, now)
		if s.limiter.Allow(candidate.auth.ID, rpm, burst) {
			return candidate.auth, true
		}
		pool = append(pool[:idx], pool[idx+1:]...)
	}
	return nil, false
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
