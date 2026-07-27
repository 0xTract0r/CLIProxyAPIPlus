package auth

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
)

// This file carries fork-only Manager behavior that upstream v7.2.101 does not
// have, ported out of the fork conductor monolith into the upstream split-file
// structure. Each item here still has live callers (either inside this package
// or in other packages) so dropping them would break the build and silently
// remove a fork capability.

// SetHook replaces the lifecycle hook used for auth and result observations.
// Upstream removed the setter; the fork wires the auth-registry hook through it
// from sdk/cliproxy/builder.go (coreManager.SetHook(authRegistryHook{...})).
func (m *Manager) SetHook(hook Hook) {
	if m == nil {
		return
	}
	if hook == nil {
		hook = NoopHook{}
	}
	m.mu.Lock()
	m.hook = hook
	m.mu.Unlock()
}

// hydrateRuntimeFields applies runtime-only fields (per-account proxy_url and
// custom outbound headers) from persisted metadata onto the auth. It is invoked
// at Register/Update so a freshly loaded or updated credential carries its
// per-account egress configuration (T041 proxy binding, #66 managed headers)
// before the scheduler and executors observe it. Upstream dropped this call;
// the fork keeps it because the runtime fields are anti-correlation critical.
func hydrateRuntimeFields(auth *Auth) {
	ApplyRuntimeFieldsFromMetadata(auth)
	ApplyCustomHeadersFromMetadata(auth)
}

// authAllowsRouteModel reports whether the given auth is permitted to serve the
// given route model. It gates subscription-restricted models (Claude Opus,
// Codex Spark) at the scheduler layer so an account whose plan does not include
// a premium model is never rotated in for it. Called from scheduler.go while
// building each auth's servable model set.
func authAllowsRouteModel(auth *Auth, model string) bool {
	if auth == nil {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(auth.Provider)) {
	case "claude":
		return authAllowsClaudeRouteModel(auth, model)
	case "codex":
		return authAllowsCodexRouteModel(auth, model)
	default:
		return true
	}
}

func authAllowsClaudeRouteModel(auth *Auth, model string) bool {
	if !registry.IsClaudeOpusModelID(canonicalModelKey(model)) {
		return true
	}
	return authAllowsClaudeOpusModel(auth)
}

func authAllowsCodexRouteModel(auth *Auth, model string) bool {
	if !registry.IsCodexSparkModelID(canonicalModelKey(model)) {
		return true
	}
	return registry.CodexPlanAllowsSpark(authCodexSubscriptionPlanType(auth))
}

// IncrementCyberPolicyCount atomically bumps the cyber_policy flag counter and
// timestamp for the auth with the given ID, then persists the change.
//
// The read-modify-write of CyberPolicyFlagCount / LastCyberPolicyAt happens
// entirely under m.mu so concurrent hits cannot lose increments. Persistence
// (store + hooks) runs after the lock is released to avoid re-entrant locking
// inside Update / persist. When persistence fails the in-memory counter is
// still bumped (visible to subsequent reads via m.auths) but the error is
// surfaced so callers can suppress webhook dispatch and emit an ERROR log.
//
// Returns the new count, the timestamp recorded, and a persistence error if
// any. When the auth cannot be located it returns (0, zero time, nil). Called
// from internal/runtime/executor/codex_cyber_policy.go.
func (m *Manager) IncrementCyberPolicyCount(ctx context.Context, authID string) (int, time.Time, error) {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return 0, time.Time{}, nil
	}
	now := time.Now().UTC()
	m.mu.Lock()
	existing, ok := m.auths[authID]
	if !ok || existing == nil {
		m.mu.Unlock()
		return 0, time.Time{}, nil
	}
	// Perform the read-modify-write under the lock: mutate the in-memory entry
	// directly so concurrent hits cannot lose increments.
	existing.CyberPolicyFlagCount++
	existing.LastCyberPolicyAt = now
	newCount := existing.CyberPolicyFlagCount
	snapshot := existing.Clone()
	m.mu.Unlock()
	if snapshot == nil {
		return newCount, now, nil
	}
	// Persist outside the lock: persist no longer acquires m.mu, but keep it out
	// of the critical section for safety. Surface the error so callers can decide
	// whether to suppress the webhook dispatch.
	if err := m.persist(ctx, snapshot); err != nil {
		return newCount, now, err
	}
	return newCount, now, nil
}

// quotaRetryAfterFromHeaders inspects codex quota headers on a response and, when
// the primary or secondary usage bucket is already exhausted (>=100%), returns
// the retry-after until the soonest reset. This lets a "successful" codex
// response proactively cool the account down so the scheduler rotates off it
// before it starts returning 429s. Fork-only codex quota-rotation capability;
// upstream v7.2.101 does not parse these headers. Guarded by scheduler_test.go
// TestManager_ExecuteSuccessCodexExhaustedHeaderCoolsAuth (+ ExecuteCount).
func quotaRetryAfterFromHeaders(provider string, headers http.Header, now time.Time) *time.Duration {
	if !strings.EqualFold(strings.TrimSpace(provider), "codex") || len(headers) == 0 {
		return nil
	}
	if now.IsZero() {
		now = time.Now()
	}
	var latest time.Time
	for _, prefix := range []string{"X-Codex", "X-Codex-Bengalfox"} {
		if quotaHeaderPercentExhausted(headers.Get(prefix + "-Primary-Used-Percent")) {
			if resetAt := quotaHeaderResetTime(headers, prefix+"-Primary", now); resetAt.After(latest) {
				latest = resetAt
			}
		}
		if quotaHeaderPercentExhausted(headers.Get(prefix + "-Secondary-Used-Percent")) {
			if resetAt := quotaHeaderResetTime(headers, prefix+"-Secondary", now); resetAt.After(latest) {
				latest = resetAt
			}
		}
	}
	if latest.IsZero() || !latest.After(now) {
		return nil
	}
	retryAfter := latest.Sub(now)
	return &retryAfter
}

func quotaHeaderPercentExhausted(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false
	}
	value, err := strconv.ParseFloat(raw, 64)
	return err == nil && value >= 100
}

func quotaHeaderResetTime(headers http.Header, prefix string, now time.Time) time.Time {
	if headers == nil {
		return time.Time{}
	}
	if raw := strings.TrimSpace(headers.Get(prefix + "-Reset-At")); raw != "" {
		if unixSeconds, err := strconv.ParseInt(raw, 10, 64); err == nil && unixSeconds > 0 {
			resetAt := time.Unix(unixSeconds, 0)
			if resetAt.After(now) {
				return resetAt
			}
		}
	}
	if raw := strings.TrimSpace(headers.Get(prefix + "-Reset-After-Seconds")); raw != "" {
		if seconds, err := strconv.ParseInt(raw, 10, 64); err == nil && seconds > 0 {
			return now.Add(time.Duration(seconds) * time.Second)
		}
	}
	return time.Time{}
}

// quotaRetryAfterFromHeadersNow is a convenience wrapper for execution/stream
// call sites that do not otherwise need the time package; it evaluates the codex
// quota headers against the current wall-clock time.
func quotaRetryAfterFromHeadersNow(provider string, headers http.Header) *time.Duration {
	return quotaRetryAfterFromHeaders(provider, headers, time.Now())
}

// resultIndicatesPlanQuota reports whether a 429 error explicitly signals a
// plan-level usage/quota exhaustion (e.g. codex usage_limit_reached), as opposed
// to a transient capacity / TPM rate limit. MarkResult's 429 handling uses it
// (together with the presence of an upstream RetryAfter hint) to decide between
// the plan-quota cooldown (Quota.Exceeded=true, escalating) and the brief
// transient cooldown (Quota.Exceeded=false). A bare "rate_limit" without a quota
// marker stays transient so a capacity blip does not flip plan-level quota state.
func resultIndicatesPlanQuota(err *Error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Message + " " + err.Code)
	return strings.Contains(text, "quota") ||
		strings.Contains(text, "usage_limit") ||
		strings.Contains(text, "usage limit") ||
		strings.Contains(text, "plan_limit") ||
		strings.Contains(text, "plan limit")
}
