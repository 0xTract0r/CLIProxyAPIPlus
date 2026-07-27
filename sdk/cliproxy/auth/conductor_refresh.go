package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	log "github.com/sirupsen/logrus"
)

// RefreshEvaluator allows runtime state to override refresh decisions.
type RefreshEvaluator interface {
	ShouldRefresh(now time.Time, auth *Auth) bool
}

const (
	refreshCheckInterval  = 5 * time.Second
	refreshMaxConcurrency = 16
	refreshPendingBackoff = time.Minute
	refreshFailureBackoff = 5 * time.Minute
	// refreshIneffectiveBackoff throttles refresh attempts when an executor returns
	// success but the auth still evaluates as needing refresh (e.g. token expiry
	// wasn't updated). Without this guard, the auto-refresh loop can tight-loop and
	// burn CPU at idle.
	refreshIneffectiveBackoff = 30 * time.Second
	// refreshMinDwellFallback is the floor for the anti-thrash backoff used
	// after a successful refresh when the upstream provider does not return a
	// parseable expiry. It is intentionally longer than refreshIneffectiveBackoff
	// so a short lead time cannot create a tight refresh loop that hammers the
	// token endpoint and rewrites the auth file every minute (fork anti-thrash).
	refreshMinDwellFallback = 15 * time.Minute
	quotaBackoffBase        = time.Second
	quotaBackoffMax         = 30 * time.Minute
	transientErrorCooldown  = time.Minute
)

// antiThrashRefreshBackoff returns the minimum wait that must elapse before
// auth becomes eligible for another refresh after a successful refresh whose
// shouldRefresh check would otherwise immediately fire again. The goal is to
// guarantee that each issued access_token is used for a meaningful fraction of
// its real lifetime so the system stops hammering the upstream token endpoint.
//
// Selection rules:
//   - If the freshly-issued token has a parseable expiry, wait until half of
//     its remaining lifetime has elapsed (clamped to refreshIneffectiveBackoff
//     as a lower bound to avoid pathological 0/negative values).
//   - Otherwise fall back to refreshMinDwellFallback, which is intentionally
//     long enough that thrash cannot recur even when the executor reports no
//     expiry at all.
func antiThrashRefreshBackoff(auth *Auth, now time.Time) time.Duration {
	if auth == nil {
		return refreshMinDwellFallback
	}
	expiry, ok := auth.ExpirationTime()
	if !ok || expiry.IsZero() {
		return refreshMinDwellFallback
	}
	ttl := expiry.Sub(now)
	if ttl <= 0 {
		// Token already expired (or upstream returned a stale expiry): keep
		// the legacy short backoff so a real refresh retry can happen soon.
		return refreshIneffectiveBackoff
	}
	half := ttl / 2
	if half < refreshIneffectiveBackoff {
		return refreshIneffectiveBackoff
	}
	return half
}

// StartAutoRefresh launches a background loop that evaluates auth freshness
// every few seconds and triggers refresh operations when required.
// Only one loop is kept alive; starting a new one cancels the previous run.
func (m *Manager) StartAutoRefresh(parent context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = refreshCheckInterval
	}

	m.mu.Lock()
	cancelPrev := m.refreshCancel
	m.refreshCancel = nil
	m.refreshLoop = nil
	m.mu.Unlock()
	if cancelPrev != nil {
		cancelPrev()
	}

	ctx, cancelCtx := context.WithCancel(parent)
	workers := refreshMaxConcurrency
	if cfg, ok := m.runtimeConfig.Load().(*internalconfig.Config); ok && cfg != nil && cfg.AuthAutoRefreshWorkers > 0 {
		workers = cfg.AuthAutoRefreshWorkers
	}
	loop := newAuthAutoRefreshLoop(m, interval, workers)

	m.mu.Lock()
	m.refreshCancel = cancelCtx
	m.refreshLoop = loop
	m.mu.Unlock()

	loop.rebuild(time.Now())
	go loop.run(ctx)
}

// StopAutoRefresh cancels the background refresh loop, if running.
// It also stops the selector if it implements StoppableSelector.
func (m *Manager) StopAutoRefresh() {
	m.mu.Lock()
	cancel := m.refreshCancel
	m.refreshCancel = nil
	m.refreshLoop = nil
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	// Stop selector if it implements StoppableSelector (e.g., SessionAffinitySelector)
	if stoppable, ok := m.selector.(StoppableSelector); ok {
		stoppable.Stop()
	}
}

func (m *Manager) queueRefreshReschedule(authID string) {
	if m == nil || authID == "" {
		return
	}
	m.mu.RLock()
	loop := m.refreshLoop
	m.mu.RUnlock()
	if loop == nil {
		return
	}
	loop.queueReschedule(authID)
}

func (m *Manager) queueRefreshUnschedule(authID string) {
	if m == nil || authID == "" {
		return
	}
	m.mu.RLock()
	loop := m.refreshLoop
	m.mu.RUnlock()
	if loop == nil {
		return
	}
	loop.remove(authID)
}

func (m *Manager) shouldRefresh(a *Auth, now time.Time) bool {
	if a == nil {
		return false
	}
	if hasUnauthorizedAuthFailure(a) {
		return false
	}
	if !a.NextRefreshAfter.IsZero() && now.Before(a.NextRefreshAfter) {
		return false
	}
	if evaluator, ok := a.Runtime.(RefreshEvaluator); ok && evaluator != nil {
		return evaluator.ShouldRefresh(now, a)
	}

	lastRefresh := a.LastRefreshedAt
	if lastRefresh.IsZero() {
		if ts, ok := authLastRefreshTimestamp(a); ok {
			lastRefresh = ts
		}
	}

	expiry, hasExpiry := a.ExpirationTime()

	if interval := authPreferredInterval(a); interval > 0 {
		if hasExpiry && !expiry.IsZero() {
			if !expiry.After(now) {
				return true
			}
			if expiry.Sub(now) <= interval {
				return true
			}
		}
		if lastRefresh.IsZero() {
			return true
		}
		return now.Sub(lastRefresh) >= interval
	}

	provider := strings.ToLower(a.Provider)
	lead := ProviderRefreshLead(provider, a.Runtime)
	if lead == nil {
		return false
	}
	if *lead <= 0 {
		if hasExpiry && !expiry.IsZero() {
			return now.After(expiry)
		}
		return false
	}
	if hasExpiry && !expiry.IsZero() {
		return time.Until(expiry) <= *lead
	}
	if !lastRefresh.IsZero() {
		return now.Sub(lastRefresh) >= *lead
	}
	return true
}

func authPreferredInterval(a *Auth) time.Duration {
	if a == nil {
		return 0
	}
	if d := durationFromMetadata(a.Metadata, "refresh_interval_seconds", "refreshIntervalSeconds", "refresh_interval", "refreshInterval"); d > 0 {
		return d
	}
	if d := durationFromAttributes(a.Attributes, "refresh_interval_seconds", "refreshIntervalSeconds", "refresh_interval", "refreshInterval"); d > 0 {
		return d
	}
	return 0
}

func durationFromMetadata(meta map[string]any, keys ...string) time.Duration {
	if len(meta) == 0 {
		return 0
	}
	for _, key := range keys {
		if val, ok := meta[key]; ok {
			if dur := parseDurationValue(val); dur > 0 {
				return dur
			}
		}
	}
	return 0
}

func durationFromAttributes(attrs map[string]string, keys ...string) time.Duration {
	if len(attrs) == 0 {
		return 0
	}
	for _, key := range keys {
		if val, ok := attrs[key]; ok {
			if dur := parseDurationString(val); dur > 0 {
				return dur
			}
		}
	}
	return 0
}

func parseDurationValue(val any) time.Duration {
	switch v := val.(type) {
	case time.Duration:
		if v <= 0 {
			return 0
		}
		return v
	case int:
		if v <= 0 {
			return 0
		}
		return time.Duration(v) * time.Second
	case int32:
		if v <= 0 {
			return 0
		}
		return time.Duration(v) * time.Second
	case int64:
		if v <= 0 {
			return 0
		}
		return time.Duration(v) * time.Second
	case uint:
		if v == 0 {
			return 0
		}
		return time.Duration(v) * time.Second
	case uint32:
		if v == 0 {
			return 0
		}
		return time.Duration(v) * time.Second
	case uint64:
		if v == 0 {
			return 0
		}
		return time.Duration(v) * time.Second
	case float32:
		if v <= 0 {
			return 0
		}
		return time.Duration(float64(v) * float64(time.Second))
	case float64:
		if v <= 0 {
			return 0
		}
		return time.Duration(v * float64(time.Second))
	case json.Number:
		if i, err := v.Int64(); err == nil {
			if i <= 0 {
				return 0
			}
			return time.Duration(i) * time.Second
		}
		if f, err := v.Float64(); err == nil && f > 0 {
			return time.Duration(f * float64(time.Second))
		}
	case string:
		return parseDurationString(v)
	}
	return 0
}

func parseDurationString(raw string) time.Duration {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0
	}
	if dur, err := time.ParseDuration(s); err == nil && dur > 0 {
		return dur
	}
	if secs, err := strconv.ParseFloat(s, 64); err == nil && secs > 0 {
		return time.Duration(secs * float64(time.Second))
	}
	return 0
}

func authLastRefreshTimestamp(a *Auth) (time.Time, bool) {
	if a == nil {
		return time.Time{}, false
	}
	if a.Metadata != nil {
		if ts, ok := lookupMetadataTime(a.Metadata, "last_refresh", "lastRefresh", "last_refreshed_at", "lastRefreshedAt"); ok {
			return ts, true
		}
	}
	if a.Attributes != nil {
		for _, key := range []string{"last_refresh", "lastRefresh", "last_refreshed_at", "lastRefreshedAt"} {
			if val := strings.TrimSpace(a.Attributes[key]); val != "" {
				if ts, ok := parseTimeValue(val); ok {
					return ts, true
				}
			}
		}
	}
	return time.Time{}, false
}

func lookupMetadataTime(meta map[string]any, keys ...string) (time.Time, bool) {
	for _, key := range keys {
		if val, ok := meta[key]; ok {
			if ts, ok1 := parseTimeValue(val); ok1 {
				return ts, true
			}
		}
	}
	return time.Time{}, false
}

func (m *Manager) markRefreshPending(id string, now time.Time) bool {
	m.mu.Lock()
	auth, ok := m.auths[id]
	if !ok || auth == nil {
		m.mu.Unlock()
		return false
	}
	if !auth.NextRefreshAfter.IsZero() && now.Before(auth.NextRefreshAfter) {
		m.mu.Unlock()
		return false
	}
	auth.NextRefreshAfter = now.Add(refreshPendingBackoff)
	m.auths[id] = auth
	m.mu.Unlock()

	m.queueRefreshReschedule(id)
	return true
}

type authRefreshLock struct {
	mu sync.Mutex
}

func authAccessToken(auth *Auth) string {
	if token := authMetadataString(auth, "access_token"); token != "" {
		return token
	}
	return authMetadataString(auth, "accessToken")
}

func authHasRefreshCredential(auth *Auth) bool {
	if authMetadataString(auth, "refresh_token") != "" {
		return true
	}
	return authMetadataString(auth, "refreshToken") != ""
}

func clearUnauthorizedModelStates(auth *Auth, now time.Time) []string {
	if auth == nil || len(auth.ModelStates) == 0 {
		return nil
	}
	var resumed []string
	for model, state := range auth.ModelStates {
		if state == nil || state.LastError == nil {
			continue
		}
		if state.LastError.StatusCode() != http.StatusUnauthorized && !strings.EqualFold(state.LastError.Code, "unauthorized") {
			continue
		}
		resetModelState(state, now)
		resumed = append(resumed, model)
	}
	if len(resumed) > 0 {
		updateAggregatedAvailability(auth, now)
	}
	return resumed
}

// tryRefreshAfterUnauthorized refreshes OAuth credentials once after a 401 so the
// current auth can be retried before fallback/suspend.
func (m *Manager) tryRefreshAfterUnauthorized(ctx context.Context, auth *Auth, execErr error, alreadyTried bool) (*Auth, bool) {
	if m == nil || auth == nil || alreadyTried || execErr == nil {
		return auth, false
	}
	if !isUnauthorizedError(execErr) || !authHasRefreshCredential(auth) {
		return auth, false
	}
	log.Debugf("unauthorized response for %s (%s), refreshing credentials before fallback", auth.Provider, auth.ID)
	refreshed, errRefresh := m.refreshAuthForRequest(ctx, auth.ID, authAccessToken(auth))
	if errRefresh != nil || refreshed == nil {
		log.Debugf("credential refresh before fallback failed for %s (%s): %v", auth.Provider, auth.ID, errRefresh)
		return auth, false
	}
	return refreshed, true
}

func (m *Manager) refreshAuth(ctx context.Context, id string) {
	_, _ = m.refreshAuthForRequest(ctx, id, "")
}

// refreshAuthForRequest performs a synchronous credential refresh for the given auth.
// failedAccessToken lets concurrent callers reuse a refresh that already replaced the
// access token that produced the unauthorized response.
func (m *Manager) refreshAuthForRequest(ctx context.Context, id, failedAccessToken string) (*Auth, error) {
	if m == nil {
		return nil, errors.New("auth manager is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, errors.New("auth id is empty")
	}

	lockValue, _ := m.refreshLocks.LoadOrStore(id, &authRefreshLock{})
	lock, _ := lockValue.(*authRefreshLock)
	if lock == nil {
		lock = &authRefreshLock{}
		m.refreshLocks.Store(id, lock)
	}
	lock.mu.Lock()
	defer lock.mu.Unlock()

	m.mu.RLock()
	auth := m.auths[id]
	var exec ProviderExecutor
	refreshDisabled := false
	if auth != nil {
		exec = m.executors[auth.Provider]
		refreshDisabled = auth.RefreshDisabled()
	}
	m.mu.RUnlock()
	if auth == nil || exec == nil {
		return nil, errors.New("auth or executor not found")
	}

	// Fork anti-thrash short-circuit: once a credential reaches a terminal
	// reauth-required state (refresh token reused / invalid_grant), RefreshDisabled()
	// is set and we must not attempt another refresh -- retrying a dead token cannot
	// recover it and may trip provider refresh-token reuse detection. Clear any pending
	// refresh time, keep the scheduler snapshot current, reschedule so the auto-refresh
	// loop stops hammering, and return the existing credential without calling Refresh.
	// Edge-triggered: the terminal-error branch below only fires once because this
	// guard swallows every later refresh attempt (#164).
	if refreshDisabled {
		m.mu.Lock()
		current := m.auths[id]
		if current != nil {
			current.NextRefreshAfter = time.Time{}
			m.auths[id] = current
			if m.scheduler != nil {
				m.scheduler.upsertAuth(current.Clone())
			}
		}
		m.mu.Unlock()
		m.queueRefreshReschedule(id)
		if current != nil {
			return current.Clone(), nil
		}
		return auth.Clone(), nil
	}

	// Another request may already have refreshed this credential.
	if failedAccessToken != "" {
		if currentToken := authAccessToken(auth); currentToken != "" && currentToken != failedAccessToken {
			return auth.Clone(), nil
		}
	}

	cloned := auth.Clone()
	updated, err := exec.Refresh(ctx, cloned)
	if err != nil && errors.Is(err, context.Canceled) {
		log.Debugf("refresh canceled for %s, %s", auth.Provider, auth.ID)
		return nil, err
	}
	log.Debugf("refreshed %s, %s, %v", auth.Provider, auth.ID, err)
	now := time.Now()
	if err != nil {
		shouldReschedule := false
		var reauthSnapshot *Auth
		m.mu.Lock()
		if current := m.auths[id]; current != nil {
			// MERGE-REVIEW: combined upstream's unauthorized (401) refresh handling
			// with the fork's terminal reauth-required handling (refresh token reused
			// / invalid_grant -> persisted reauth_required so the dead token is never
			// retried). Upstream's 401 path is checked first so a plain 401 refresh
			// failure is classified as "unauthorized"; only the fork's stricter
			// terminal signals (refresh-token-reuse phrasing, or a non-401
			// invalid_grant) escalate to the persisted reauth_required state.
			// Everything else keeps the short transient backoff and retries.
			if isUnauthorizedError(err) {
				current.LastError = refreshErrorFromError(err)
				current.NextRefreshAfter = time.Time{}
				current.Unavailable = true
				current.Status = StatusError
				current.StatusMessage = "unauthorized"
			} else if code, terminal := terminalRefreshAuthError(err); terminal {
				// Terminal refresh failures (refresh token reused / invalid_grant /
				// revoked / expired) cannot be recovered by retrying the same token,
				// and retrying may trip provider reuse detection. Persist the
				// reauth-required state so the failure is visible after a restart and
				// the auto-refresh loop stops hammering a dead token. Edge-triggered:
				// once RefreshDisabled() is true a later refresh short-circuits before
				// reaching here, so this fires once per actual terminal event (#164).
				logEntryWithRequestID(ctx).WithFields(log.Fields{
					"auth_ref":       current.ID,
					"provider":       current.Provider,
					"error_code":     code,
					"cred_fp":        refreshTokenFingerprintFromMetadata(current.Metadata),
					"instance_id":    processInstanceID(),
					"classification": classifyTerminalRefreshFailure(code),
				}).Error("terminal refresh failure: reauth required")
				// #163 semi-automatic reauth alert on the same untracked->locked edge
				// as the diagnostic log above. Only Claude has an auth-scoped one-click
				// reauth endpoint today, so the URL is only attached for that provider;
				// other providers still get the WARN so the lock isn't silent.
				alertFields := log.Fields{
					"auth_ref":       current.ID,
					"provider":       current.Provider,
					"error_code":     code,
					"instance_id":    processInstanceID(),
					"classification": classifyTerminalRefreshFailure(code),
				}
				alertMessage := "reauth required: credential locked, manual re-authentication needed"
				if strings.EqualFold(strings.TrimSpace(current.Provider), "claude") {
					if reauthURL := reauthAlertURL(current.ID); reauthURL != "" {
						alertFields["reauth_url"] = reauthURL
						alertMessage = "reauth required: credential locked, generate a fresh sign-in link via " + reauthURL
					}
				}
				logEntryWithRequestID(ctx).WithFields(alertFields).Warn(alertMessage)
				current.markRefreshReauthRequiredWithReason(now, code)
				reauthSnapshot = current.Clone()
			} else {
				current.LastError = refreshErrorFromError(err)
				current.NextRefreshAfter = now.Add(refreshFailureBackoff)
				current.UpdatedAt = now
			}
			m.auths[id] = current
			shouldReschedule = true
			if m.scheduler != nil {
				m.scheduler.upsertAuth(current.Clone())
			}
		}
		m.mu.Unlock()
		if reauthSnapshot != nil {
			if errPersist := m.persist(ctx, reauthSnapshot); errPersist != nil {
				logEntryWithRequestID(ctx).WithField("auth_id", id).Warnf("failed to persist reauth-required refresh state: %v", errPersist)
			}
			m.hook.OnAuthUpdated(ctx, reauthSnapshot.Clone())
		}
		if shouldReschedule {
			m.queueRefreshReschedule(id)
		}
		return nil, err
	}
	if updated == nil {
		updated = cloned
	}
	// Preserve runtime created by the executor during Refresh.
	// If executor didn't set one, fall back to the previous runtime.
	if updated.Runtime == nil {
		updated.Runtime = auth.Runtime
	}
	updated.LastRefreshedAt = now
	updated.NextRefreshAfter = time.Time{}
	updated.LastError = nil
	updated.StatusMessage = ""
	updated.Unavailable = false
	if updated.Status == StatusError {
		updated.Status = StatusActive
	}
	updated.UpdatedAt = now
	modelsToResume := clearUnauthorizedModelStates(updated, now)
	if m.shouldRefresh(updated, now) {
		// Fork anti-thrash: use a backoff floored at half the freshly-issued
		// token's remaining lifetime (or refreshMinDwellFallback when no expiry
		// is parseable) instead of the short refreshIneffectiveBackoff, so a
		// successful refresh that still evaluates as refreshable cannot tight-loop
		// the token endpoint and rewrite the auth file every minute.
		updated.NextRefreshAfter = now.Add(antiThrashRefreshBackoff(updated, now))
	}
	saved, errUpdate := m.Update(ctx, updated)
	for _, model := range modelsToResume {
		registry.GetGlobalRegistry().ResumeClientModel(id, model)
	}
	if errUpdate != nil {
		log.Debugf("persist refreshed auth %s (%s) failed: %v", auth.Provider, auth.ID, errUpdate)
	}
	if saved != nil {
		return saved, nil
	}
	return updated.Clone(), nil
}
