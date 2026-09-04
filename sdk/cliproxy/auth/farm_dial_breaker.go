// Fork-only farm scheduling resilience: a persistent-dead-proxy dial-failure
// circuit breaker with automatic dial-recovery restore.
//
// PROBLEM this closes (pure scheduling layer):
// A farm account whose per-account proxy string is LEGAL but currently
// unreachable (a dial failure, HTTP status 0 -- the connection never completed,
// so no HTTP response was ever received) is NOT cooled down by the existing MarkResult
// switch: a status-0 failure falls into that switch's `default` branch, which
// zeroes NextRetryAfter. isAuthBlockedForModel then treats a zero NextRetryAfter
// as "not blocked", so the dead-proxy account stays in the rotation and roughly
// 1/N requests keep selecting it, each eating a full dial timeout (HTTP ~30s,
// SOCKS5 longer) before the higher-layer failover moves on. The request never
// hard-fails (failover covers it) but every batch that lands on the dead account
// gets slow.
//
// This is DELIBERATELY DISTINCT from the two neighbouring mechanisms and touches
// neither:
//   - The terminal-auth auto-quarantine (evaluateAutoQuarantineLocked): that is
//     an ACCOUNT-layer lock for a revoked/invalid credential (real serving 401)
//     that needs operator reauth. It is permanent until reauth. A dial failure is
//     a NETWORK-layer condition; the breaker here is TEMPORARY and self-recovers.
//   - The empty/illegal proxy fail-closed egress guard (authMissingProxyURL in
//     scheduler.go + the write-side 400 reject): that is a LEGALITY gate for a
//     missing/malformed proxy, and it fails closed by removing the account from
//     scheduling so traffic never leaks to a direct IP-exposing connection. The
//     breaker here is for a proxy that is LEGAL but merely unreachable; it never
//     relaxes or touches that fail-closed egress guard.
//
// The breaker reuses the existing selection chokepoint (isAuthBlockedForModel)
// and the existing scheduler auto-restore machinery (promoteExpiredLocked) rather
// than inventing a parallel one:
//   - Detection/counting lives in MarkResult via evaluateDialFailureBreakerLocked
//     (called right after evaluateAutoQuarantineLocked), maintaining a per-account
//     consecutive dial-failure streak with zero intervening successes.
//   - Once the streak reaches the threshold the account gets a SHORT, escalating
//     backoff window (dialBreakerUntil). While it is in the future the selector
//     gate forkDialFailureBreakerBlocked skips the account with a distinct,
//     fork-only block reason (blockReasonDialBreaker).
//   - Auto-restore is free: the moment the window elapses the gate stops firing
//     (the account rejoins the rotation and re-probes), and a single real success
//     immediately clears the streak+window (dial recovery). No reauth needed.
//
// It is farm-scoped (only AuthFarmEnrolled accounts can ever be affected; ordinary
// accounts are untouched) and gated behind an env flag that DEFAULTS OFF (staged
// rollout). When the flag is off every function here is a strict no-op, so
// non-farm and pre-flag behaviour is byte-identical.
//
// Non-starvation is guaranteed at the selection aggregation layer (the legacy
// availableAuthsForRouteModel/getAvailableAuths pools and the built-in scheduler
// pick paths): the breaker is a PREFER-TO-SKIP, not a hard exclude. If EVERY
// candidate is blocked solely by the dial breaker, one is still selected as a
// last-resort probe (which doubles as the recovery attempt) instead of collapsing
// to "no auth available" — see the ignoreDialBreaker fallbacks in selector.go /
// conductor_selection.go / scheduler.go.
package auth

import (
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// FarmDialFailureBreakerEnabledEnvVar arms the dead-proxy dial-failure breaker.
// It is env-driven (like the sibling FARM_REQUIRE_* gates) so enabling it needs
// no config-schema change and stays decoupled from non-farm request handling.
// Unlike FARM_REQUIRE_PROVISIONED (fail-safe default-armed), this feature is a
// PERFORMANCE/resilience optimisation, not a safety fail-closed, so it stays an
// allowlist default-OFF during staged rollout: only a recognised truthy token
// (1/true/yes/on) arms it; unset/empty/unrecognised leaves it disarmed.
const FarmDialFailureBreakerEnabledEnvVar = "FARM_PROXY_DIALFAIL_BREAKER_ENABLED"

// The tunables below are env-overridable with safe defaults so an operator can
// tighten/loosen the breaker without a rebuild. Each getter clamps to a sane
// floor so a garbage/zero override can never disable the guard by accident or
// produce a pathological (e.g. negative) window.
const (
	// FarmDialFailureBreakerThresholdEnvVar overrides the consecutive dial-failure
	// count (zero intervening successes, within the streak window) required to
	// trip the breaker.
	FarmDialFailureBreakerThresholdEnvVar = "FARM_PROXY_DIALFAIL_BREAKER_THRESHOLD"
	// FarmDialFailureBreakerBaseBackoffSecondsEnvVar overrides the initial backoff
	// (seconds) applied the first time the breaker trips.
	FarmDialFailureBreakerBaseBackoffSecondsEnvVar = "FARM_PROXY_DIALFAIL_BREAKER_BASE_BACKOFF_SECONDS"
	// FarmDialFailureBreakerMaxBackoffSecondsEnvVar caps the escalating backoff
	// (seconds) for a persistently dead proxy.
	FarmDialFailureBreakerMaxBackoffSecondsEnvVar = "FARM_PROXY_DIALFAIL_BREAKER_MAX_BACKOFF_SECONDS"
	// FarmDialFailureBreakerWindowSecondsEnvVar overrides the rolling window
	// (seconds) used to detect a consecutive-failure streak; a gap longer than
	// this resets the streak so an occasional isolated blip never accumulates.
	FarmDialFailureBreakerWindowSecondsEnvVar = "FARM_PROXY_DIALFAIL_BREAKER_WINDOW_SECONDS"

	// dialFailureBreakerDefaultThreshold is the default consecutive dial-failure
	// count to trip the breaker. Kept low (a persistently dead proxy fails every
	// selection) but > 1 so a single transient blip never trips it.
	dialFailureBreakerDefaultThreshold = 3
	// dialFailureBreakerDefaultBaseBackoff is the default first-trip backoff.
	// Short on purpose: the breaker must re-probe soon in case the proxy recovers,
	// while still saving the bulk of the wasted dial timeouts in between.
	dialFailureBreakerDefaultBaseBackoff = 60 * time.Second
	// dialFailureBreakerDefaultMaxBackoff caps the escalating backoff so a
	// persistently dead proxy is skipped for longer, but never so long that a
	// recovered proxy stays parked for an unreasonable time.
	dialFailureBreakerDefaultMaxBackoff = 10 * time.Minute
	// dialFailureBreakerDefaultWindow is the rolling streak window. A dial failure
	// older than this (with no intervening success) does not count toward the
	// current streak.
	dialFailureBreakerDefaultWindow = 10 * time.Minute
)

// blockReasonDialBreaker is a fork-only block reason, distinct from the upstream
// iota-defined reasons in selector.go (blockReasonNone/Cooldown/Disabled/Other)
// and from the other fork reasons in provisioned_gate.go (blockReasonUnprovisioned
// 1<<16, blockReasonContainerNotAlive 1<<17). It is given the next distinct high
// bit so it never collides even if upstream appends new iota reasons. It marks an
// account skipped by the dead-proxy dial-failure breaker. Unlike the fail-closed
// provisioning/liveness reasons, a dial-breaker skip is SOFT: the selection
// aggregation layer falls back to a breaker-blocked account when it is the only
// thing left, so the request never collapses to "no auth available".
const blockReasonDialBreaker blockReason = 1 << 18

// dialFailureBreakerEnabled reports whether the breaker is armed. Allowlist,
// default-OFF: only a recognised truthy token arms it; unset/empty/unrecognised
// leaves it disarmed so pre-flag behaviour is byte-identical. Mirrors
// farmRequireContainerAliveEnabled's shape (NOT the fail-safe default-armed shape
// of farmRequireProvisionedEnabled) — this is a resilience optimisation, not a
// safety gate, so it must stay opt-in during rollout.
func dialFailureBreakerEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(FarmDialFailureBreakerEnabledEnvVar))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// dialFailureBreakerThreshold returns the tripping threshold, clamped to a floor
// of 1 so a zero/garbage override can never disable tripping.
func dialFailureBreakerThreshold() int {
	if v, ok := parseEnvInt(FarmDialFailureBreakerThresholdEnvVar); ok && v >= 1 {
		return v
	}
	return dialFailureBreakerDefaultThreshold
}

// dialFailureBreakerBaseBackoff returns the first-trip backoff, clamped to a
// floor of 1s so a zero override can never produce an already-expired window.
func dialFailureBreakerBaseBackoff() time.Duration {
	if v, ok := parseEnvInt(FarmDialFailureBreakerBaseBackoffSecondsEnvVar); ok && v >= 1 {
		return time.Duration(v) * time.Second
	}
	return dialFailureBreakerDefaultBaseBackoff
}

// dialFailureBreakerMaxBackoff returns the escalation cap, never below the base
// backoff so the cap can never invert the ladder.
func dialFailureBreakerMaxBackoff() time.Duration {
	base := dialFailureBreakerBaseBackoff()
	if v, ok := parseEnvInt(FarmDialFailureBreakerMaxBackoffSecondsEnvVar); ok && v >= 1 {
		d := time.Duration(v) * time.Second
		if d < base {
			return base
		}
		return d
	}
	if dialFailureBreakerDefaultMaxBackoff < base {
		return base
	}
	return dialFailureBreakerDefaultMaxBackoff
}

// dialFailureBreakerWindow returns the rolling streak window, clamped to a floor
// of 1s so a zero/garbage override can never make every failure start a fresh
// streak (which would stop the breaker from ever tripping).
func dialFailureBreakerWindow() time.Duration {
	if v, ok := parseEnvInt(FarmDialFailureBreakerWindowSecondsEnvVar); ok && v >= 1 {
		return time.Duration(v) * time.Second
	}
	return dialFailureBreakerDefaultWindow
}

func parseEnvInt(key string) (int, bool) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return 0, false
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, false
	}
	return v, true
}

// isDialFailureResultError reports whether a failure result represents a
// connectivity/transport ("dial") failure — the proxy (or upstream) could not be
// reached and NO HTTP response was ever received, so the result carries HTTP
// status 0.
//
// It classifies purely by "status == 0 and this is not a request-shaped error",
// deliberately NOT by matching substrings like "connection refused" in the raw
// error body: that keeps it robust across the many transport stacks (HTTP proxy
// CONNECT, SOCKS5 dial, plain TCP) whose wording varies, and mirrors how
// isTerminalAuthQuarantineResultError classifies by status rather than message.
// Every failure that DID receive an HTTP response (401/402/403/404/429/5xx/…)
// carries a non-zero status and is excluded here, so it keeps following its own
// dedicated cooldown/quarantine path unchanged.
//
// Request-scoped errors are excluded: those are tied to the request body (e.g. a
// store=false item miss), not the account's proxy path, so they must never count
// toward a dead-proxy verdict. A nil error is not a dial failure.
//
// A rare false positive (e.g. a mid-stream client cancellation surfacing as a
// status-0 error) is harmless by construction: the breaker only trips on a STREAK
// of consecutive status-0 failures with ZERO intervening successes inside the
// window, so any interspersed real success on a healthy proxy resets the streak
// before it can trip — and even a trip only imposes a short, self-recovering skip.
func isDialFailureResultError(err *Error) bool {
	if err == nil {
		return false
	}
	if statusCodeFromResult(err) != 0 {
		return false
	}
	if isRequestScopedResultError(err) {
		return false
	}
	// A status-0 error with neither a code nor a message carries no signal at all;
	// require some content so a zero-value Error struct is not misread as a dial
	// failure.
	return strings.TrimSpace(err.Code) != "" || strings.TrimSpace(err.Message) != ""
}

// dialFailureBreakerBackoffForStreak returns the escalating backoff for a streak
// that has reached (or exceeded) the threshold. The first trip (streak ==
// threshold) uses the base backoff; each additional consecutive dial failure
// beyond the threshold doubles it, capped at the max. This keeps a briefly-flaky
// proxy parked only a short time (fast re-probe) while a persistently dead proxy
// backs off progressively longer, up to the cap.
func dialFailureBreakerBackoffForStreak(streak int) time.Duration {
	base := dialFailureBreakerBaseBackoff()
	max := dialFailureBreakerMaxBackoff()
	threshold := dialFailureBreakerThreshold()
	exponent := streak - threshold
	if exponent < 0 {
		exponent = 0
	}
	// Guard against overflow / absurd shift counts: once the exponent is large
	// enough that base<<exponent would meet or exceed the cap, just return the cap.
	if exponent >= 32 {
		return max
	}
	backoff := base << uint(exponent)
	if backoff <= 0 || backoff > max {
		return max
	}
	return backoff
}

// forkDialFailureBreakerBlocked reports whether the dead-proxy dial-failure
// breaker should currently skip auth during selection. It is the selector-side
// gate, isomorphic to forkRequireProvisionedBlocked: a strict no-op (returns
// false) when the feature is disarmed, and otherwise only ever fires for an
// explicitly farm-enrolled account whose breaker window (dialBreakerUntil) is
// still in the future. It self-clears the instant the window elapses (the caller
// re-evaluates with now) or a real success clears dialBreakerUntil.
func forkDialFailureBreakerBlocked(auth *Auth, now time.Time) bool {
	if !dialFailureBreakerEnabled() {
		return false
	}
	if auth == nil {
		return false
	}
	if !AuthFarmEnrolled(auth) {
		return false
	}
	return auth.dialBreakerUntil.After(now)
}

// dialBreakerFallbackList returns the last-resort candidate list for the legacy
// selection pools when every account is blocked solely by the dial breaker. It
// copies and stably sorts by ID so the fallback pick is deterministic (the
// round-robin/fill-first selectors index into this slice), and never mutates the
// caller's slice.
func dialBreakerFallbackList(candidates []*Auth) []*Auth {
	if len(candidates) == 0 {
		return candidates
	}
	out := make([]*Auth, len(candidates))
	copy(out, candidates)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// evaluateDialFailureBreakerLocked maintains the per-account consecutive
// dial-failure streak and trips/escalates the breaker window. It must be called
// once per MarkResult invocation, after the other status/state mutations for this
// result (like evaluateAutoQuarantineLocked), so it is the final word on the
// breaker fields for this call. Callers must hold m.mu.
//
// Semantics, mirroring evaluateAutoQuarantineLocked's streak discipline:
//   - Strict no-op when the feature is disarmed or the account is not farm-enrolled
//     (ordinary accounts are never affected).
//   - A real success proves the proxy path is healthy again: it resets the streak
//     AND clears any active breaker window (dial-recovery auto-restore).
//   - A non-dial failure (401/429/5xx/… — anything that got an HTTP response, or a
//     request-scoped error) neither advances nor resets the streak: it is not a
//     success, so an in-progress streak survives it, but it is not evidence of a
//     dead proxy either.
//   - A dial failure (status 0) advances the streak; a gap longer than the window
//     restarts it. Reaching the threshold sets (or escalates) dialBreakerUntil.
func (m *Manager) evaluateDialFailureBreakerLocked(auth *Auth, success bool, resultErr *Error, now time.Time) {
	if auth == nil {
		return
	}
	if !dialFailureBreakerEnabled() {
		return
	}
	if !AuthFarmEnrolled(auth) {
		return
	}
	if success {
		auth.dialFailureStreak = 0
		auth.dialFailureStreakStartAt = time.Time{}
		auth.dialBreakerUntil = time.Time{}
		return
	}
	if !isDialFailureResultError(resultErr) {
		return
	}
	if auth.dialFailureStreak <= 0 || now.Sub(auth.dialFailureStreakStartAt) > dialFailureBreakerWindow() {
		auth.dialFailureStreak = 1
		auth.dialFailureStreakStartAt = now
		return
	}
	auth.dialFailureStreak++
	if auth.dialFailureStreak >= dialFailureBreakerThreshold() {
		auth.dialBreakerUntil = now.Add(dialFailureBreakerBackoffForStreak(auth.dialFailureStreak))
	}
}
