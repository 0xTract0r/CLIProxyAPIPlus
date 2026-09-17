package management

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

const (
	quotaSnapshotMetadataKey      = "quota_snapshot"
	quotaRefreshStatusMetadataKey = "quota_refresh_status"
	quotaRefreshErrorMetadataKey  = "quota_refresh_error"
	quotaLastRefreshedMetadataKey = "quota_last_refreshed_at"
	quotaNextRefreshMetadataKey   = "quota_next_refresh_after"
	quotaSnapshotPlanTypeKey      = "plan_type"
	// quotaRefreshHTTPStatusMetadataKey / quotaRefreshRetryAfterMetadataKey record
	// the upstream HTTP status and the parsed Retry-After hint of the last failed
	// quota probe. They are pure observability: without them a failed probe left
	// only the fixed quotaHTTPError sentence in quota_refresh_error, which cannot
	// distinguish a genuine rate limit from a degraded credential (both 429 on the
	// Anthropic oauth endpoints). Both keys are derived runtime state and are
	// therefore registered in reauthRuntimeMetadataKeys so a re-auth clears them.
	quotaRefreshHTTPStatusMetadataKey = "quota_refresh_http_status"
	quotaRefreshRetryAfterMetadataKey = "quota_refresh_retry_after_seconds"

	quotaRefreshStatusOK              = "ok"
	quotaRefreshStatusStale           = "stale"
	quotaRefreshStatusError           = "error"
	quotaRefreshStatusUnsupported     = "unsupported"
	quotaRefreshStatusReauthRequired  = "reauth_required"
	quotaRefreshStatusRefreshDisabled = "refresh_disabled"

	quotaUnsupportedProviderMessage = "provider does not support quota refresh"

	claudeQuotaCredentialUnauthorizedMessage  = "Claude credential unauthorized; reauthenticate this credential to refresh quota."
	codexQuotaCredentialUnauthorizedMessage   = "Codex credential unauthorized; reauthenticate this credential to refresh quota."
	genericQuotaCredentialUnauthorizedMessage = "Credential unauthorized; reauthenticate this credential to refresh quota."

	defaultQuotaSnapshotRefreshInterval = config.DefaultQuotaSnapshotRefreshInterval
	quotaSnapshotRefreshPollInterval    = time.Second
	quotaSnapshotRefreshRetryDelay      = time.Minute
	quotaSnapshotStartupJitterMax       = time.Minute
	quotaSnapshotProviderTimeout        = 15 * time.Second

	// quotaErrorBodyReadLimit bounds how much of a non-2xx quota response body is
	// read before field extraction. Provider error bodies are small JSON
	// documents; the cap keeps a hostile or truncated stream from being buffered.
	quotaErrorBodyReadLimit = 64 << 10
	// quotaErrorFieldMaxRunes bounds each extracted error field so the persisted
	// metadata stays compact even when a provider returns a long message.
	quotaErrorFieldMaxRunes = 200

	// quotaRefreshFailureLogInterval throttles the background poller's failure
	// logs per (auth, signature) pair. The poller ticks once per second, so an
	// unthrottled warn would flood main.log with one line per failing account per
	// tick; a changed signature (status/error class) still logs immediately.
	quotaRefreshFailureLogInterval = 15 * time.Minute
	// quotaRefreshSkipLogInterval throttles the sticky-skip visibility log. The
	// skip branch is silent by design and can freeze polling indefinitely, so it
	// needs a heartbeat, but at a much lower rate than the failure log.
	quotaRefreshSkipLogInterval = time.Hour
)

type QuotaSnapshotRefreshPolicy struct {
	Enabled             bool
	Interval            time.Duration
	Jitter              time.Duration
	StartupCatchUp      bool
	StartupMaxStaleness time.Duration
	ProviderTimeout     time.Duration
}

type quotaSnapshotRefreshPolicyPayload struct {
	Enabled                    bool  `json:"enabled"`
	IntervalSeconds            int64 `json:"interval_seconds"`
	JitterSeconds              int64 `json:"jitter_seconds"`
	StartupCatchUp             bool  `json:"startup_catch_up"`
	StartupMaxStalenessSeconds int64 `json:"startup_max_staleness_seconds"`
	ProviderTimeoutSeconds     int64 `json:"provider_timeout_seconds"`
}

type quotaSnapshotEntry struct {
	AuthID          string         `json:"auth_id"`
	AuthIndex       string         `json:"auth_index,omitempty"`
	Name            string         `json:"name,omitempty"`
	Provider        string         `json:"provider"`
	Label           string         `json:"label,omitempty"`
	Disabled        bool           `json:"disabled,omitempty"`
	Status          string         `json:"status"`
	Error           string         `json:"error,omitempty"`
	PlanType        string         `json:"plan_type,omitempty"`
	LastRefreshedAt *time.Time     `json:"last_refreshed_at,omitempty"`
	NextRefreshAt   *time.Time     `json:"next_refresh_at,omitempty"`
	Snapshot        map[string]any `json:"snapshot,omitempty"`
}

type quotaRefreshRequest struct {
	AuthID   string `json:"auth_id"`
	Name     string `json:"name"`
	Provider string `json:"provider"`
}

type quotaSnapshotPayload struct {
	GeneratedAt    time.Time                         `json:"generated_at"`
	Policy         quotaSnapshotRefreshPolicyPayload `json:"policy"`
	Entries        []quotaSnapshotEntry              `json:"entries"`
	RefreshResults []quotaRefreshResult              `json:"refresh_results,omitempty"`
}

type quotaRefreshResult struct {
	AuthID     string `json:"auth_id"`
	AuthIndex  string `json:"auth_index,omitempty"`
	Name       string `json:"name,omitempty"`
	Provider   string `json:"provider"`
	Label      string `json:"label,omitempty"`
	Status     string `json:"status"`
	Error      string `json:"error,omitempty"`
	ErrorClass string `json:"error_class,omitempty"`
	// HTTPStatus / RetryAfterSeconds / ProviderErrorType / ProviderErrorMessage
	// surface the observation captured from a non-2xx quota response. They are
	// omitempty so successful refreshes and non-HTTP failures keep their current
	// response shape. ProviderErrorType/Message are the allow-listed error.type
	// and error.message fields only (string-typed, truncated); the rest of the
	// upstream body is discarded. Allow-listed is not scrubbed — see
	// quotaErrorFieldsFromBody for the residual risk.
	HTTPStatus           int      `json:"http_status,omitempty"`
	RetryAfterSeconds    int64    `json:"retry_after_seconds,omitempty"`
	ProviderErrorType    string   `json:"provider_error_type,omitempty"`
	ProviderErrorMessage string   `json:"provider_error_message,omitempty"`
	ElapsedMS            int64    `json:"elapsed_ms"`
	Refreshed            bool     `json:"refreshed"`
	ProxySource          string   `json:"proxy_source,omitempty"`
	ProxyHash            string   `json:"proxy_hash,omitempty"`
	TargetURLs           []string `json:"target_urls,omitempty"`
}

func QuotaSnapshotRefreshPolicyFromConfig(cfg *config.Config) QuotaSnapshotRefreshPolicy {
	return QuotaSnapshotRefreshPolicy{
		Enabled:             config.QuotaSnapshotRefreshEnabled(cfg),
		Interval:            config.QuotaSnapshotRefreshInterval(cfg),
		Jitter:              config.QuotaSnapshotRefreshJitter(cfg),
		StartupCatchUp:      config.QuotaSnapshotRefreshStartupCatchUp(cfg),
		StartupMaxStaleness: config.QuotaSnapshotRefreshStartupMaxStaleness(cfg),
	}.normalized()
}

func (p QuotaSnapshotRefreshPolicy) normalized() QuotaSnapshotRefreshPolicy {
	if p.Interval <= 0 {
		p.Interval = config.DefaultQuotaSnapshotRefreshInterval
	}
	if p.Jitter < 0 {
		p.Jitter = 0
	}
	if p.StartupMaxStaleness < 0 {
		p.StartupMaxStaleness = config.DefaultQuotaSnapshotRefreshStartupMaxStaleness
	}
	if p.ProviderTimeout <= 0 {
		p.ProviderTimeout = quotaSnapshotProviderTimeout
	}
	return p
}

func (p QuotaSnapshotRefreshPolicy) payload() quotaSnapshotRefreshPolicyPayload {
	p = p.normalized()
	return quotaSnapshotRefreshPolicyPayload{
		Enabled:                    p.Enabled,
		IntervalSeconds:            int64(p.Interval / time.Second),
		JitterSeconds:              int64(p.Jitter / time.Second),
		StartupCatchUp:             p.StartupCatchUp,
		StartupMaxStalenessSeconds: int64(p.StartupMaxStaleness / time.Second),
		ProviderTimeoutSeconds:     int64(p.ProviderTimeout / time.Second),
	}
}

func (h *Handler) quotaSnapshotRefreshPolicy() QuotaSnapshotRefreshPolicy {
	if h == nil {
		return QuotaSnapshotRefreshPolicyFromConfig(nil)
	}
	h.mu.Lock()
	cfg := h.cfg
	h.mu.Unlock()
	return QuotaSnapshotRefreshPolicyFromConfig(cfg)
}

// StartQuotaSnapshotAutoRefresh launches the core-owned quota refresher. The
// management UI should read these persisted snapshots instead of directly
// fanning out provider quota API calls on page entry.
func (h *Handler) StartQuotaSnapshotAutoRefresh(parent context.Context, policy QuotaSnapshotRefreshPolicy) {
	if h == nil {
		return
	}
	if parent == nil {
		parent = context.Background()
	}
	policy = policy.normalized()

	h.mu.Lock()
	cancelPrev := h.quotaRefreshCancel
	h.quotaRefreshCancel = nil
	h.mu.Unlock()
	if cancelPrev != nil {
		cancelPrev()
	}
	if !policy.Enabled {
		return
	}

	ctx, cancel := context.WithCancel(parent)
	h.mu.Lock()
	h.quotaRefreshCancel = cancel
	h.mu.Unlock()

	go h.runQuotaSnapshotAutoRefresh(ctx, policy)
}

func (h *Handler) runQuotaSnapshotAutoRefresh(ctx context.Context, policy QuotaSnapshotRefreshPolicy) {
	ticker := time.NewTicker(quotaSnapshotRefreshPollInterval)
	defer ticker.Stop()
	h.refreshDueQuotaSnapshots(ctx, policy, true)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			h.refreshDueQuotaSnapshots(ctx, policy, false)
		}
	}
}

func (h *Handler) refreshDueQuotaSnapshots(ctx context.Context, policy QuotaSnapshotRefreshPolicy, startup bool) {
	policy = policy.normalized()
	if !policy.Enabled {
		return
	}
	manager := h.currentAuthManager()
	if manager == nil {
		return
	}
	now := time.Now().UTC()
	for _, auth := range manager.List() {
		if auth == nil || auth.Disabled || !quotaSnapshotProviderSupported(auth.Provider) {
			continue
		}
		// Farm supply-atomicity fail-closed gate (R5-3e): skip background quota
		// polling for enrolled-but-unprovisioned Claude accounts. The claude quota
		// probe (fetchProviderQuotaSnapshot) issues real GET /api/oauth/profile and
		// /api/oauth/usage requests to api.anthropic.com carrying this account's
		// token; probing one before it is bound to a container leaks the synthetic
		// device_id identity. Strict no-op when FARM_REQUIRE_PROVISIONED is off /
		// for non-enrolled accounts, so existing polling is unchanged.
		if coreauth.RequireProvisionedBlocked(auth) {
			// farm-account-liveness B1 (armed only): the anti-corr fail-closed gate is
			// skipping this account from health probing. If it was EVER bound to a
			// container (its device_id is already on-wire exposed) this is a
			// "health-blind" state, not a healthy one — stamp an explicit marker so the
			// projection renders it gray + alert instead of a falsely-green cached
			// snapshot. The gate's leak-prevention semantics are unchanged (never-bound
			// synthetic accounts are still never probed); this only adds observability.
			if farmLivenessDetectionEnabled() && coreauth.FarmHealthBlind(auth) {
				h.stampQuotaHealthBlind(ctx, auth, now)
			}
			continue
		}
		// Recovered (StatusActive) accounts may still carry a stale
		// quota_refresh_status=reauth_required written by an earlier transient
		// 401/403. Mirror the explicit-refresh path (quotaRefreshTargets) so the
		// implicit skip does not pin them forever; the next-refresh schedule below
		// still throttles re-probing so genuinely unauthorized accounts are not
		// hammered.
		// farm-account-liveness F1 (detection-only self-heal): a farm account
		// carrying the probe-set authoritative lock would otherwise be skipped
		// forever (implicit-skip + not-"recovered" because Status!=Active), leaving
		// a genuinely recovered credential pinned red with no re-prober when the
		// liveness probe is not armed. farmLivenessRecoveryReprobeEligible lets the
		// poller keep re-probing exactly those accounts (throttled by their normal
		// next-refresh schedule, so a truly revoked token is not hammered); a
		// successful re-probe then clears the lock via the success path above.
		if quotaSnapshotImplicitRefreshSkipped(auth) && !quotaSnapshotAuthRecovered(auth) && !farmLivenessRecoveryReprobeEligible(auth) {
			// Observability only: the predicate and the continue are unchanged, we
			// just stop the freeze from being externally invisible.
			h.logQuotaStickySkip(auth, now)
			continue
		}
		legacyUnsupported := quotaSnapshotLegacyUnsupportedProviderError(auth)
		next, hasNext := quotaSnapshotNextRefresh(auth)
		if hasNext && !legacyUnsupported {
			if next.After(now) {
				if startup && quotaSnapshotStartupCatchUpNeeded(auth, now, policy, true) {
					next = quotaSnapshotStartupCatchUpRefreshTime(auth, now, policy)
					if err := h.persistQuotaSnapshotSchedule(ctx, auth, next); err != nil && !strings.Contains(err.Error(), context.Canceled.Error()) {
						log.WithError(err).Debugf("management quota: startup catch-up schedule failed for %s/%s", auth.Provider, auth.ID)
					}
					if next.After(now) {
						continue
					}
				} else if startup && quotaSnapshotNextRefreshBeyondPolicy(auth, next, now, policy) {
					next = quotaSnapshotNextRefreshTime(auth, now, policy)
					if err := h.persistQuotaSnapshotSchedule(ctx, auth, next); err != nil && !strings.Contains(err.Error(), context.Canceled.Error()) {
						log.WithError(err).Debugf("management quota: policy reschedule failed for %s/%s", auth.Provider, auth.ID)
					}
					continue
				} else {
					continue
				}
			}
		} else if !legacyUnsupported {
			next := quotaSnapshotInitialRefreshTime(auth, now, policy)
			if startup && quotaSnapshotStartupCatchUpNeeded(auth, now, policy, false) {
				next = quotaSnapshotStartupCatchUpRefreshTime(auth, now, policy)
			}
			if err := h.persistQuotaSnapshotSchedule(ctx, auth, next); err != nil && !strings.Contains(err.Error(), context.Canceled.Error()) {
				log.WithError(err).Debugf("management quota: schedule failed for %s/%s", auth.Provider, auth.ID)
			}
			if next.After(now) {
				continue
			}
		}
		if updated, err := h.refreshQuotaSnapshot(ctx, auth, policy); err != nil && !strings.Contains(err.Error(), context.Canceled.Error()) {
			// Previously Debugf, i.e. invisible at the production Info level. The
			// failure detail now lands in the log at Warn, throttled per account.
			// updated carries the freshly persisted status when available, so the
			// error class is derived from post-write state.
			logged := auth
			if updated != nil {
				logged = updated
			}
			h.logQuotaBackgroundRefreshFailure(logged, err, time.Now().UTC())
		}
	}
}

// quotaRefreshLogEntry is one throttle slot: the last emitted signature for a
// key and when it was emitted.
type quotaRefreshLogEntry struct {
	signature string
	loggedAt  time.Time
}

// quotaRefreshLogAllowed implements the background poller's log throttle.
//
// Strategy: state-change-OR-interval, keyed per (event, auth). A line is emitted
// when the signature changes (a new status / error class / HTTP status for this
// account is news and must be visible immediately) and otherwise at most once
// per minInterval while the signature keeps repeating. This was chosen over a
// global rate limit because the poller ticks once per second over ALL accounts:
// a global limiter would let one noisy account starve the others, while a
// per-account interval alone would hide a state transition for up to the whole
// interval.
//
// Memory: entries are refreshed in place, so repeated logging for the same key
// does not grow the map. Entries are never DELETED though, so the real bound is
// the number of distinct (event, authID) pairs seen during the process
// lifetime — deleting an account, or re-authenticating it into a new ID, leaves
// a residual entry behind for as long as the process runs. That is accepted
// rather than fixed: each entry is a short string plus a timestamp, the account
// count is in the low tens, and a deployment restarts the process.
func (h *Handler) quotaRefreshLogAllowed(event, authID, signature string, now time.Time, minInterval time.Duration) bool {
	if h == nil {
		return true
	}
	key := event + "|" + authID
	h.quotaRefreshLogMu.Lock()
	defer h.quotaRefreshLogMu.Unlock()
	if h.quotaRefreshLogState == nil {
		h.quotaRefreshLogState = make(map[string]quotaRefreshLogEntry)
	}
	previous, found := h.quotaRefreshLogState[key]
	if found && previous.signature == signature && now.Sub(previous.loggedAt) < minInterval {
		return false
	}
	h.quotaRefreshLogState[key] = quotaRefreshLogEntry{signature: signature, loggedAt: now}
	return true
}

// logQuotaBackgroundRefreshFailure makes a background quota refresh failure
// visible in production. The failure used to be logged at Debug only, while the
// production log level is Info, so a two-hour quota outage left no trace at all.
// The line is throttled (see quotaRefreshLogAllowed) so N failing accounts
// cannot flood the log on the once-per-second tick.
func (h *Handler) logQuotaBackgroundRefreshFailure(auth *coreauth.Auth, err error, now time.Time) {
	if auth == nil || err == nil {
		return
	}
	observation := quotaProbeObservationFromError(err)
	errorClass := quotaSnapshotErrorClass(err, metadataString(auth.Metadata, quotaRefreshStatusMetadataKey))
	signature := fmt.Sprintf("%s|%d|%s", errorClass, observation.StatusCode, observation.ErrorType)
	if !h.quotaRefreshLogAllowed("background_refresh_failed", auth.ID, signature, now, quotaRefreshFailureLogInterval) {
		return
	}
	fields := log.Fields{
		"auth_id":     auth.ID,
		"auth_index":  auth.Index,
		"name":        auth.FileName,
		"provider":    auth.Provider,
		"error_class": errorClass,
		"event":       "quota_background_refresh_failed",
	}
	if observation.StatusCode > 0 {
		fields["http_status"] = observation.StatusCode
	}
	if observation.RetryAfter > 0 {
		fields["retry_after_seconds"] = int64(observation.RetryAfter.Round(time.Second) / time.Second)
	}
	if observation.ErrorType != "" {
		fields["provider_error_type"] = observation.ErrorType
	}
	if observation.ErrorMessage != "" {
		fields["provider_error_message"] = observation.ErrorMessage
	}
	log.WithFields(fields).WithError(err).Warn("management quota: background refresh failed")
}

// logQuotaStickySkip makes the sticky implicit-skip branch observable. That
// branch deliberately continues without probing AND without advancing
// quota_next_refresh_after, so an account can stay frozen indefinitely with no
// external signal at all (one test account sat frozen for 38h and was only
// found through the auth file mtime). This log does not change the skip
// decision; it only records it, at a low throttled rate because the state is
// expected to persist across many ticks.
func (h *Handler) logQuotaStickySkip(auth *coreauth.Auth, now time.Time) {
	if auth == nil {
		return
	}
	status := metadataString(auth.Metadata, quotaRefreshStatusMetadataKey)
	if !h.quotaRefreshLogAllowed("sticky_skip", auth.ID, status, now, quotaRefreshSkipLogInterval) {
		return
	}
	fields := log.Fields{
		"auth_id":      auth.ID,
		"auth_index":   auth.Index,
		"name":         auth.FileName,
		"provider":     auth.Provider,
		"quota_status": status,
		"event":        "quota_background_refresh_sticky_skip",
	}
	if next := metadataString(auth.Metadata, quotaNextRefreshMetadataKey); next != "" {
		fields["quota_next_refresh_after"] = next
	}
	if last := metadataString(auth.Metadata, quotaLastRefreshedMetadataKey); last != "" {
		fields["quota_last_refreshed_at"] = last
	}
	log.WithFields(fields).Warn("management quota: account skipped by sticky reauth-required state; polling is frozen until it is re-authenticated or explicitly refreshed")
}

// GetQuotaSnapshots returns persisted core quota snapshots without contacting
// upstream providers.
func (h *Handler) GetQuotaSnapshots(c *gin.Context) {
	c.JSON(http.StatusOK, h.quotaSnapshotPayload())
}

// RefreshQuotaSnapshots refreshes quota snapshots through the core auth manager.
// It is still core-owned and persisted; clients should not call provider quota
// endpoints directly.
func (h *Handler) RefreshQuotaSnapshots(c *gin.Context) {
	manager := h.currentAuthManager()
	if manager == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "auth manager unavailable"})
		return
	}

	var req quotaRefreshRequest
	_ = c.ShouldBindJSON(&req)
	targets := h.quotaRefreshTargets(manager, req)
	if len(targets) == 0 {
		if quotaRefreshHasImplicitSupportedTargets(manager, req) {
			c.JSON(http.StatusOK, h.quotaSnapshotPayload())
			return
		}
		c.JSON(http.StatusNotFound, gin.H{"error": "no supported quota auth found"})
		return
	}

	policy := h.quotaSnapshotRefreshPolicy()
	results := make([]quotaRefreshResult, 0, len(targets))
	for _, auth := range targets {
		results = append(results, h.refreshQuotaSnapshotResult(c.Request.Context(), auth, policy))
	}

	payload := h.quotaSnapshotPayload()
	payload.RefreshResults = results
	c.JSON(http.StatusOK, payload)
}

func (h *Handler) quotaRefreshTargets(manager *coreauth.Manager, req quotaRefreshRequest) []*coreauth.Auth {
	if req.AuthID != "" {
		if auth, ok := manager.GetByID(strings.TrimSpace(req.AuthID)); ok && quotaSnapshotProviderSupported(auth.Provider) {
			return []*coreauth.Auth{auth}
		}
		return nil
	}
	if req.Name != "" {
		auth := findAuthByName(manager, strings.TrimSpace(req.Name))
		if auth != nil && quotaSnapshotProviderSupported(auth.Provider) {
			return []*coreauth.Auth{auth}
		}
		return nil
	}

	provider := strings.ToLower(strings.TrimSpace(req.Provider))
	var targets []*coreauth.Auth
	for _, auth := range manager.List() {
		if auth == nil || auth.Disabled || !quotaSnapshotProviderSupported(auth.Provider) {
			continue
		}
		// An explicit, user-initiated global refresh re-probes credentials that
		// have recovered (e.g. after re-auth) even when a stale reauth_required
		// quota status lingers. Background auto-refresh stays cautious via
		// quotaSnapshotImplicitRefreshSkipped so it never hammers a genuinely
		// unauthorized quota endpoint.
		if quotaSnapshotImplicitRefreshSkipped(auth) && !quotaSnapshotAuthRecovered(auth) {
			continue
		}
		if provider != "" && strings.ToLower(auth.Provider) != provider {
			continue
		}
		targets = append(targets, auth)
	}
	return targets
}

func quotaRefreshHasImplicitSupportedTargets(manager *coreauth.Manager, req quotaRefreshRequest) bool {
	if manager == nil || req.AuthID != "" || req.Name != "" {
		return false
	}
	provider := strings.ToLower(strings.TrimSpace(req.Provider))
	for _, auth := range manager.List() {
		if auth == nil || auth.Disabled || !quotaSnapshotProviderSupported(auth.Provider) {
			continue
		}
		if provider != "" && strings.ToLower(auth.Provider) != provider {
			continue
		}
		return true
	}
	return false
}

func (h *Handler) quotaSnapshotPayload() quotaSnapshotPayload {
	policy := h.quotaSnapshotRefreshPolicy()
	return quotaSnapshotPayload{
		GeneratedAt: time.Now().UTC(),
		Policy:      policy.payload(),
		Entries:     h.quotaSnapshotEntries(),
	}
}

func (h *Handler) quotaSnapshotEntries() []quotaSnapshotEntry {
	manager := h.currentAuthManager()
	if manager == nil {
		return nil
	}
	auths := manager.List()
	entries := make([]quotaSnapshotEntry, 0, len(auths))
	for _, auth := range auths {
		if auth == nil || !quotaSnapshotProviderSupported(auth.Provider) {
			continue
		}
		entries = append(entries, quotaSnapshotEntryFromAuth(auth))
	}
	return entries
}

func quotaSnapshotEntryFromAuth(auth *coreauth.Auth) quotaSnapshotEntry {
	status := metadataString(auth.Metadata, quotaRefreshStatusMetadataKey)
	errMessage := metadataString(auth.Metadata, quotaRefreshErrorMetadataKey)
	if quotaSnapshotLegacyReauthRequired(auth) {
		status = quotaRefreshStatusReauthRequired
		errMessage = quotaCredentialUnauthorizedMessage(auth.Provider)
	} else if quotaSnapshotLegacyUnsupportedProviderError(auth) {
		status = quotaRefreshStatusStale
		errMessage = ""
	}
	if auth.RefreshDisabled() && status != quotaRefreshStatusOK && status != quotaRefreshStatusReauthRequired {
		status = quotaRefreshStatusRefreshDisabled
		errMessage = ""
	}
	entry := quotaSnapshotEntry{
		AuthID:    auth.ID,
		AuthIndex: auth.Index,
		Name:      auth.FileName,
		Provider:  auth.Provider,
		Label:     authDisplayName(auth),
		Disabled:  auth.Disabled,
		Status:    status,
		Error:     errMessage,
		PlanType:  metadataString(auth.Metadata, quotaSnapshotPlanTypeKey),
	}
	if entry.Status == "" {
		entry.Status = quotaRefreshStatusStale
	}
	if ts, ok := metadataTime(auth.Metadata, quotaLastRefreshedMetadataKey); ok {
		entry.LastRefreshedAt = &ts
	}
	if ts, ok := metadataTime(auth.Metadata, quotaNextRefreshMetadataKey); ok {
		entry.NextRefreshAt = &ts
	}
	if snapshot, ok := auth.Metadata[quotaSnapshotMetadataKey].(map[string]any); ok {
		entry.Snapshot = snapshot
	}
	return entry
}

// errQuotaRefreshProvisionedBlocked is returned by refreshQuotaSnapshot when the
// farm supply-atomicity fail-closed gate matches an account. It signals the quota
// probe was intentionally skipped (no api.anthropic.com egress) — distinct from a
// provider/network error — so refreshQuotaSnapshotResult renders a clean blocked
// result instead of a scary provider error, and callers can errors.Is it.
var errQuotaRefreshProvisionedBlocked = errors.New("farm account not provisioned: refusing anthropic quota probe (fail-closed)")

func (h *Handler) refreshQuotaSnapshot(ctx context.Context, auth *coreauth.Auth, policy QuotaSnapshotRefreshPolicy) (*coreauth.Auth, error) {
	policy = policy.normalized()
	manager := h.currentAuthManager()
	if manager == nil || auth == nil {
		return auth, fmt.Errorf("auth manager unavailable")
	}
	// Farm supply-atomicity fail-closed gate (R5-3e, explicit-refresh chokepoint):
	// refreshQuotaSnapshot is the SINGLE function through which BOTH the background
	// poller (refreshDueQuotaSnapshots) AND the explicit operator/frontend-triggered
	// endpoint (POST /quota/refresh -> RefreshQuotaSnapshots -> quotaRefreshTargets
	// -> refreshQuotaSnapshotResult) reach fetchProviderQuotaSnapshot, whose Claude
	// branch issues real GET /api/oauth/profile + /api/oauth/usage requests to
	// api.anthropic.com carrying this account's token AND its frozen managed device
	// profile — UA / stainless / protocol headers / x-client-request-id, i.e. the
	// per-account synthetic device identity (ClaudeExecutor.PrepareRequest applies
	// the full managed device profile on isAnthropicBase). The GET probe has no body,
	// so the leak is these request headers, not a body device_id. Probing an enrolled-but-unprovisioned Claude account before
	// it is bound to a container leaks the synthetic device_id <-> account
	// correlation over the account proxy. The poller already skips these accounts at
	// its scheduling loop (see the :229 gate); wiring the same predicate here closes
	// the previously-ungated explicit endpoint for EVERY trigger mode (by-id /
	// by-name / provider-wide / global) plus any future direct caller, with one
	// chokepoint. It reuses the exact same predicate as selection/refresh, so it is a
	// strict no-op when FARM_REQUIRE_PROVISIONED is off / for non-enrolled /
	// provisioned / non-Claude accounts, keeping default behaviour and every existing
	// quota test byte-identical.
	if coreauth.RequireProvisionedBlocked(auth) {
		return auth, errQuotaRefreshProvisionedBlocked
	}
	exec, ok := manager.Executor(auth.Provider)
	if !ok || exec == nil {
		if quotaSnapshotProviderSupported(auth.Provider) {
			next := time.Now().UTC().Add(quotaSnapshotRefreshRetryDelay)
			if err := h.persistQuotaSnapshotSchedule(ctx, auth, next); err != nil {
				return auth, err
			}
			return auth, fmt.Errorf("quota refresh executor unavailable for provider %s", auth.Provider)
		}
		return h.persistQuotaSnapshotError(ctx, auth, quotaRefreshStatusUnsupported, quotaUnsupportedProviderMessage, policy, quotaProbeObservation{})
	}

	now := time.Now().UTC()
	providerCtx, cancel := context.WithTimeout(ctx, policy.ProviderTimeout)
	defer cancel()
	snapshot, planType, err := fetchProviderQuotaSnapshot(providerCtx, exec, auth)
	if err != nil {
		status, message := quotaSnapshotErrorStatusAndMessage(err)
		return h.persistQuotaSnapshotError(ctx, auth, status, message, policy, quotaProbeObservationFromError(err))
	}

	updated := auth.Clone()
	if updated.Metadata == nil {
		updated.Metadata = make(map[string]any)
	}
	updated.Metadata[quotaSnapshotMetadataKey] = snapshot
	updated.Metadata[quotaRefreshStatusMetadataKey] = quotaRefreshStatusOK
	delete(updated.Metadata, quotaRefreshErrorMetadataKey)
	// A fresh success invalidates the previous failure's observation, so drop it
	// together with the error message it belonged to.
	clearQuotaFailureObservation(updated.Metadata)
	// farm-account-liveness B1: a fresh successful probe proves the account is
	// reachable/healthy again, so release any lingering health-blind marker.
	delete(updated.Metadata, farmHealthBlindMetadataKey)
	delete(updated.Metadata, farmHealthBlindAtMetadataKey)
	// farm-account-liveness F1 (symmetric recovery): a single successful quota
	// probe proves the credential is valid, so reset the auth-failure streak and
	// reliably CLEAR any authoritative probe-set lock. This is unconditional (not
	// flag-gated): recovery must always work, even if detection was disarmed after
	// a lock was written, so an account is never pinned red with no way out.
	// ClearCredentialUnauthorized only clears the probe-set lock — it never
	// reopens a refresh-token-reuse lock or an operator's explicit refresh-disable.
	farmLivenessResetAuthFailure(updated.Metadata)
	if updated.ClearCredentialUnauthorized(now) {
		log.WithFields(log.Fields{
			"auth_id":  updated.ID,
			"provider": updated.Provider,
			"event":    "farm_liveness_quota_recovered",
		}).Warn("farm liveness: quota probe succeeded; cleared authoritative credential-unauthorized lock")
	}
	updated.Metadata[quotaLastRefreshedMetadataKey] = now.Format(time.RFC3339)
	updated.Metadata[quotaNextRefreshMetadataKey] = quotaSnapshotNextRefreshTime(updated, now, policy).Format(time.RFC3339)
	if planType != "" {
		updated.Metadata[quotaSnapshotPlanTypeKey] = planType
	}
	// OBS observability layer (harden-account-scheduling-limiter design §4.0):
	// this is the single write-back point where a fresh utilization%, a fixed
	// refresh cadence, a live auth and a persist all coincide, so it is where the
	// EWMA burn rate / projected exhaustion are derived. It samples the previous
	// persisted state on `auth` against this fresh snapshot on `updated`, and
	// persists the new burn state into updated's account_scheduling sub-object
	// (which survives the wholesale quota_snapshot replacement, unlike anything
	// nested inside quota_snapshot). Compute-only: it never changes selection,
	// limiting or gating behaviour.
	coreauth.UpdateAccountBurnObservability(auth, updated, now)
	updated.UpdatedAt = now
	return manager.Update(ctx, updated)
}

func (h *Handler) refreshQuotaSnapshotResult(ctx context.Context, auth *coreauth.Auth, policy QuotaSnapshotRefreshPolicy) quotaRefreshResult {
	start := time.Now()
	result := quotaRefreshResultFromAuth(auth)
	updated, err := h.refreshQuotaSnapshot(ctx, auth, policy)
	result.ElapsedMS = time.Since(start).Milliseconds()
	if updated != nil {
		result = quotaRefreshResultFromAuth(updated)
		result.ElapsedMS = time.Since(start).Milliseconds()
	}
	if result.Status == "" {
		result.Status = quotaRefreshStatusStale
	}
	// Farm supply-atomicity fail-closed gate (R5-3e): the probe was intentionally
	// skipped (no anthropic egress). Report it as a deliberate skip rather than a
	// provider/network error — refreshed=false with a dedicated error_class and no
	// scary error string — and do not warn-log it (mirrors the poller's silent :229
	// skip). Return early so it never falls through to the provider-error path.
	if errors.Is(err, errQuotaRefreshProvisionedBlocked) {
		result.Refreshed = false
		result.ErrorClass = "provisioning_blocked"
		result.Error = ""
		return result
	}
	if err != nil {
		result.Refreshed = false
		result.ErrorClass = quotaSnapshotErrorClass(err, result.Status)
		if result.Error == "" {
			result.Error = err.Error()
		}
		// Surface the failure observation on the explicit refresh response so an
		// operator triggering POST /v0/management/quota/refresh sees the upstream
		// status and Retry-After directly instead of only the fixed message.
		result.applyProbeObservation(quotaProbeObservationFromError(err))
	} else if result.Status == quotaRefreshStatusOK {
		result.Refreshed = true
	}
	logQuotaRefreshResult(result, err)
	return result
}

func quotaRefreshResultFromAuth(auth *coreauth.Auth) quotaRefreshResult {
	if auth == nil {
		return quotaRefreshResult{}
	}
	entry := quotaSnapshotEntryFromAuth(auth)
	result := quotaRefreshResult{
		AuthID:      entry.AuthID,
		AuthIndex:   entry.AuthIndex,
		Name:        entry.Name,
		Provider:    entry.Provider,
		Label:       entry.Label,
		Status:      entry.Status,
		Error:       entry.Error,
		TargetURLs:  quotaProviderTargetURLs(entry.Provider),
		ProxySource: "direct",
	}
	if auth != nil && authProxyURL(auth) != "" {
		result.ProxySource = "account"
		result.ProxyHash = optionalSHA256(authProxyURL(auth))
	}
	if result.Status == quotaRefreshStatusOK {
		result.Refreshed = true
	}
	return result
}

// applyProbeObservation copies the observability-only failure detail onto the
// refresh result. Zero values are left untouched so the omitempty JSON shape of
// a successful or non-HTTP failure result is unchanged.
func (r *quotaRefreshResult) applyProbeObservation(observation quotaProbeObservation) {
	if r == nil {
		return
	}
	if observation.StatusCode > 0 {
		r.HTTPStatus = observation.StatusCode
	}
	if observation.RetryAfter > 0 {
		r.RetryAfterSeconds = int64(observation.RetryAfter.Round(time.Second) / time.Second)
	}
	if observation.ErrorType != "" {
		r.ProviderErrorType = observation.ErrorType
	}
	if observation.ErrorMessage != "" {
		r.ProviderErrorMessage = observation.ErrorMessage
	}
}

func logQuotaRefreshResult(result quotaRefreshResult, err error) {
	fields := log.Fields{
		"auth_id":      result.AuthID,
		"auth_index":   result.AuthIndex,
		"name":         result.Name,
		"provider":     result.Provider,
		"status":       result.Status,
		"error_class":  result.ErrorClass,
		"elapsed_ms":   result.ElapsedMS,
		"refreshed":    result.Refreshed,
		"proxy_source": result.ProxySource,
		"proxy_hash":   result.ProxyHash,
		"target_urls":  strings.Join(result.TargetURLs, ","),
	}
	if result.HTTPStatus > 0 {
		fields["http_status"] = result.HTTPStatus
	}
	if result.RetryAfterSeconds > 0 {
		fields["retry_after_seconds"] = result.RetryAfterSeconds
	}
	if result.ProviderErrorType != "" {
		fields["provider_error_type"] = result.ProviderErrorType
	}
	entry := log.WithFields(fields)
	if err != nil || result.Status == quotaRefreshStatusError || result.Status == quotaRefreshStatusReauthRequired {
		if err != nil {
			entry = entry.WithError(err)
		}
		entry.Warn("management quota refresh account failed")
		return
	}
	entry.Info("management quota refresh account completed")
}

// persistQuotaSnapshotError records a failed quota probe. observation carries
// the observability-only detail (upstream status, Retry-After, allow-listed
// error.type/error.message) of the failure; it never affects the persisted
// status, the fixed error message or the next-refresh schedule.
func (h *Handler) persistQuotaSnapshotError(ctx context.Context, auth *coreauth.Auth, status, message string, policy QuotaSnapshotRefreshPolicy, observation quotaProbeObservation) (*coreauth.Auth, error) {
	policy = policy.normalized()
	manager := h.currentAuthManager()
	if manager == nil || auth == nil {
		return auth, fmt.Errorf("auth manager unavailable")
	}
	now := time.Now().UTC()

	// farm-account-liveness C2 (anti-overwrite, armed only): once a credential is
	// AUTHORITATIVELY confirmed unauthorized (reauth-required lock present), a
	// later TRANSIENT probe failure (network timeout / context deadline, etc.)
	// SHALL NOT roll the confirmed state back to a benign `error`. The incident's
	// root cause was exactly this: a ~5-minute-later `context deadline exceeded`
	// overwrote quota_refresh_status=reauth_required back to `error`, hiding the
	// revocation. Only a successful probe or a manual reauth may clear the lock
	// (both handled elsewhere), so here we keep the confirmed sub-field and lock
	// intact and merely reschedule. Reauth-status failures (the confirming signal
	// itself) fall through so they can still be (re)written.
	if farmLivenessDetectionEnabled() &&
		status != quotaRefreshStatusReauthRequired &&
		coreauth.IsReauthRequiredMetadata(auth.Metadata) {
		if err := h.persistQuotaSnapshotSchedule(ctx, auth, quotaSnapshotNextRefreshTime(auth, now, policy)); err != nil {
			return auth, err
		}
		return auth, &quotaPersistedError{message: message, observation: observation}
	}

	updated := auth.Clone()
	if updated.Metadata == nil {
		updated.Metadata = make(map[string]any)
	}
	updated.Metadata[quotaRefreshStatusMetadataKey] = status
	updated.Metadata[quotaRefreshErrorMetadataKey] = message
	updated.Metadata[quotaNextRefreshMetadataKey] = quotaSnapshotNextRefreshTime(updated, now, policy).Format(time.RFC3339)
	// Observability keys for the last failed probe. This writer rewrites or drops
	// both together with the status/message above, so the pair always describes
	// the failure persisted here. The pair must be kept consistent at EVERY writer
	// of quota_refresh_status, not just this one — see
	// clearQuotaFailureObservation for the other status writers and why. Both keys
	// are also registered in reauthRuntimeMetadataKeys so a re-auth drops them
	// along with the rest of the derived quota runtime state.
	if observation.StatusCode > 0 {
		updated.Metadata[quotaRefreshHTTPStatusMetadataKey] = observation.StatusCode
	} else {
		delete(updated.Metadata, quotaRefreshHTTPStatusMetadataKey)
	}
	if observation.RetryAfter > 0 {
		updated.Metadata[quotaRefreshRetryAfterMetadataKey] = int64(observation.RetryAfter.Round(time.Second) / time.Second)
	} else {
		delete(updated.Metadata, quotaRefreshRetryAfterMetadataKey)
	}

	// farm-account-liveness C1 + C5(a) (authoritative escalation, armed only):
	// a confirmed `credential unauthorized` (HTTP 401/403 from the quota/profile
	// probe) must not stop at the non-authoritative quota_refresh_status
	// sub-field — the management account view reads the AUTHORITATIVE Status /
	// reauth_required lock, so leaving it in the sub-field is what let a revoked
	// account keep showing green. Escalate it into the same authoritative
	// reauth-required lock a terminal refresh failure writes, so the quota
	// probe layer becomes a real trigger source for the account going red.
	//
	// Guards (review F1/F1b): (1) 2-STRIKE — a single 401/403 must NOT lock; a
	// lone WAF/rate-limit 403 or a flaky 401 is common and would false-lock a
	// healthy account. We require farmLivenessAuthFailThreshold consecutive
	// confirmations within the window (mirroring the serving auto-quarantine
	// 401×2 model); before that we only keep the sub-field. (2) FARM-SCOPED —
	// only farm-enrolled accounts escalate, so this never touches production /
	// non-farm claude+codex accounts.
	if farmLivenessDetectionEnabled() && status == quotaRefreshStatusReauthRequired && coreauth.AuthFarmEnrolled(updated) {
		streak := farmLivenessRecordAuthFailure(updated.Metadata, now)
		if streak >= farmLivenessAuthFailThreshold {
			updated.MarkCredentialUnauthorized(now)
			log.WithFields(log.Fields{
				"auth_id":  updated.ID,
				"provider": updated.Provider,
				"streak":   streak,
				"event":    "farm_liveness_quota_unauthorized_escalated",
			}).Warn("farm liveness: quota probe confirmed credential unauthorized to threshold; marked reauth-required")
		} else {
			log.WithFields(log.Fields{
				"auth_id":  updated.ID,
				"provider": updated.Provider,
				"streak":   streak,
				"event":    "farm_liveness_quota_unauthorized_streak",
			}).Warn("farm liveness: quota probe unauthorized below threshold; not escalated yet")
		}
	}

	updated.UpdatedAt = now
	saved, err := manager.Update(ctx, updated)
	if err != nil {
		return saved, err
	}
	return saved, &quotaPersistedError{message: message, observation: observation}
}

// stampQuotaHealthBlind writes the explicit health-blind signal for an
// ever-bound farm account the anti-corr gate is skipping from health probing.
// It is idempotent and self-throttling: it no-ops when the account already
// carries a stronger authoritative reauth-required lock (a confirmed revocation
// is more specific than health-blind), and when the marker is already stamped
// (so the once-per-second poller does not churn Update calls). Only ever called
// when FARM_LIVENESS_DETECTION_ENABLED is armed.
func (h *Handler) stampQuotaHealthBlind(ctx context.Context, auth *coreauth.Auth, now time.Time) {
	if auth == nil {
		return
	}
	if coreauth.IsReauthRequiredMetadata(auth.Metadata) {
		return
	}
	// Self-throttle: the marker is written together with the health_blind quota
	// status, so an already-health_blind status means the stamp is current and the
	// once-per-second poller must not churn another Update.
	if metadataString(auth.Metadata, quotaRefreshStatusMetadataKey) == quotaRefreshStatusHealthBlind {
		return
	}
	manager := h.currentAuthManager()
	if manager == nil {
		return
	}
	updated := auth.Clone()
	if updated.Metadata == nil {
		updated.Metadata = make(map[string]any)
	}
	updated.Metadata[quotaRefreshStatusMetadataKey] = quotaRefreshStatusHealthBlind
	updated.Metadata[quotaRefreshErrorMetadataKey] = healthBlindQuotaErrorMessage
	// health_blind means "not probed", so any retained per-probe observation
	// belongs to an older failure and must not travel with the new status.
	clearQuotaFailureObservation(updated.Metadata)
	updated.Metadata[farmHealthBlindMetadataKey] = true
	if metadataString(updated.Metadata, farmHealthBlindAtMetadataKey) == "" {
		updated.Metadata[farmHealthBlindAtMetadataKey] = now.UTC().Format(time.RFC3339)
	}
	updated.UpdatedAt = now.UTC()
	if _, err := manager.Update(ctx, updated); err != nil && !strings.Contains(err.Error(), context.Canceled.Error()) {
		log.WithError(err).Debugf("management quota: health-blind stamp failed for %s/%s", auth.Provider, auth.ID)
	}
}

func (h *Handler) persistQuotaSnapshotSchedule(ctx context.Context, auth *coreauth.Auth, nextRefreshAt time.Time) error {
	manager := h.currentAuthManager()
	if manager == nil || auth == nil {
		return fmt.Errorf("auth manager unavailable")
	}
	updated := auth.Clone()
	if updated.Metadata == nil {
		updated.Metadata = make(map[string]any)
	}
	updated.Metadata[quotaNextRefreshMetadataKey] = nextRefreshAt.Format(time.RFC3339)
	updated.UpdatedAt = time.Now().UTC()
	_, err := manager.Update(ctx, updated)
	return err
}

func fetchProviderQuotaSnapshot(ctx context.Context, exec coreauth.ProviderExecutor, auth *coreauth.Auth) (map[string]any, string, error) {
	switch strings.ToLower(strings.TrimSpace(auth.Provider)) {
	case "codex":
		payload, err := fetchQuotaJSON(ctx, exec, auth, http.MethodGet, "https://chatgpt.com/backend-api/wham/usage", nil)
		if err != nil {
			return nil, "", quotaReauthErrorForProvider("codex", err)
		}
		return map[string]any{"usage": payload}, inferCodexPlanType(auth, payload), nil
	case "claude":
		headers := http.Header{"anthropic-beta": []string{"oauth-2025-04-20"}}
		profile, err := fetchQuotaJSON(ctx, exec, auth, http.MethodGet, "https://api.anthropic.com/api/oauth/profile", headers)
		if err != nil {
			return nil, "", quotaReauthErrorForProvider("claude", err)
		}
		usage, err := fetchQuotaJSON(ctx, exec, auth, http.MethodGet, "https://api.anthropic.com/api/oauth/usage", headers)
		if err != nil {
			return nil, "", quotaReauthErrorForProvider("claude", err)
		}
		planType := inferClaudePlanType(profile)
		return map[string]any{"profile": profile, "usage": usage}, planType, nil
	default:
		return nil, "", fmt.Errorf("provider %s quota refresh unsupported", auth.Provider)
	}
}

func fetchQuotaJSON(ctx context.Context, exec coreauth.ProviderExecutor, auth *coreauth.Auth, method, url string, headers http.Header) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	for name, values := range headers {
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}
	resp, err := exec.HttpRequest(ctx, auth, req)
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, fmt.Errorf("empty response")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Observability: the failure path used to discard the body and the whole
		// header set, leaving only the fixed non-success sentence to diagnose an
		// outage with. Read a bounded prefix of the body and keep the allow-listed
		// error.type / error.message plus the Retry-After hint. The body is read
		// raw (no Content-Encoding decoding) on purpose: a compressed or truncated
		// error body must degrade to empty fields rather than fail the request
		// differently than before.
		var errorType, errorMessage string
		if resp.Body != nil {
			raw, errRead := io.ReadAll(io.LimitReader(resp.Body, quotaErrorBodyReadLimit))
			if errRead == nil {
				errorType, errorMessage = quotaErrorFieldsFromBody(raw)
			}
			_ = resp.Body.Close()
		}
		return nil, &quotaHTTPError{
			StatusCode:   resp.StatusCode,
			RetryAfter:   quotaRetryAfterFromHeader(resp.Header, time.Now()),
			ErrorType:    errorType,
			ErrorMessage: errorMessage,
		}
	}
	body, err := quotaResponseBodyReader(resp)
	if err != nil {
		_ = resp.Body.Close()
		return nil, err
	}
	defer body.Close()
	data, err := io.ReadAll(io.LimitReader(body, 4<<20))
	if err != nil {
		return nil, err
	}
	var payload map[string]any
	if len(data) > 0 {
		normalized := normalizeQuotaJSONPayload(data)
		if err := json.Unmarshal(normalized, &payload); err != nil {
			return nil, fmt.Errorf("quota endpoint returned non-JSON response after decoding: %w", err)
		}
	}
	if payload == nil {
		payload = make(map[string]any)
	}
	return payload, nil
}

// quotaHTTPError carries the observation extracted from a non-2xx quota
// response. StatusCode is the only field that participates in behaviour
// (quotaHTTPStatusRequiresReauth / quotaSnapshotErrorClass); RetryAfter,
// ErrorType and ErrorMessage are observability-only and never change scheduling
// or reauth decisions.
//
// Their sinks are NOT the same, which matters for the privacy argument below:
// StatusCode and RetryAfter are the only two written to auth metadata
// (quota_refresh_http_status / quota_refresh_retry_after_seconds, see
// persistQuotaSnapshotError). ErrorType and ErrorMessage are never persisted at
// all — they reach only the process log (logQuotaBackgroundRefreshFailure logs
// both, logQuotaRefreshResult logs the type only) and the refresh API response
// (quotaRefreshResult.applyProbeObservation).
type quotaHTTPError struct {
	StatusCode int
	// RetryAfter is the parsed Retry-After header hint, zero when absent or
	// malformed. It is recorded so an operator can tell a genuine rate limit
	// (server-supplied backoff) from a degraded credential, which the Anthropic
	// oauth endpoints also answer with 429.
	RetryAfter time.Duration
	// ErrorType / ErrorMessage are the allow-listed error.type and error.message
	// fields of the upstream body. Everything else in the body is discarded on
	// purpose: quota error bodies can echo organization uuids or account emails,
	// which must never reach persisted metadata. The two kept fields are NOT
	// scrubbed internally — see quotaErrorFieldsFromBody for the residual risk
	// that a middlebox error page puts URL / org text inside error.message.
	ErrorType    string
	ErrorMessage string
}

// Error keeps the historical wording byte-for-byte. Downstream code matches on
// this text (quotaSnapshotLegacyReauthRequired infers 401/403 from persisted
// error strings, quotaSnapshotErrorClass falls back to a "non-success status"
// substring), so the added observation fields are deliberately NOT rendered
// here; they travel through metadata, the refresh result and logs instead.
func (e *quotaHTTPError) Error() string {
	return fmt.Sprintf("quota endpoint returned non-success status %d", e.StatusCode)
}

// quotaRetryAfterFromHeader parses a Retry-After header, supporting both the
// delay-seconds and the HTTP-date forms.
//
// This is an intentional ~15-line copy of retryAfterFromHeader in
// internal/runtime/executor/usage_limit_retry.go:93. That helper is package
// private and the management layer must not import the runtime executor package
// (management -> executor would couple the management API to request execution),
// so duplicating the parse is preferred over exporting a cross-package symbol
// for an observability-only field. Keep both copies in sync if the parsing rules
// change.
func quotaRetryAfterFromHeader(headers http.Header, now time.Time) time.Duration {
	if headers == nil {
		return 0
	}
	raw := strings.TrimSpace(headers.Get("Retry-After"))
	if raw == "" {
		return 0
	}
	if seconds, err := strconv.ParseInt(raw, 10, 64); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if resetAt, err := http.ParseTime(raw); err == nil && resetAt.After(now) {
		return resetAt.Sub(now)
	}
	return 0
}

// quotaErrorFieldsFromBody applies a two-field ALLOW-LIST to a non-2xx quota
// body: only error.type and error.message are read, both are required to be JSON
// strings, and both are truncated. The raw body is never returned, so the
// organization uuids / emails / tokens quota error bodies can echo stay out of
// auth metadata and out of the management UI.
//
// This is an allow-list, NOT redaction: nothing inside the two kept fields is
// scrubbed. A KNOWN RESIDUAL RISK remains — a legitimately string-typed
// error.message is forwarded verbatim to its sinks (the process log and the
// refresh API response; neither kept field is written to auth metadata), and a
// non-Anthropic middlebox in the chain (corporate proxy, WAF, load balancer
// error page) can put the request URL or organization-related text into that
// string. The allow-list bounds the blast radius to two short fields; it does
// not guarantee the content is identifier-free. Do not treat these fields as
// sanitized.
func quotaErrorFieldsFromBody(body []byte) (string, string) {
	if len(body) == 0 {
		return "", ""
	}
	return truncateQuotaErrorField(quotaErrorStringField(body, "error.type")),
		truncateQuotaErrorField(quotaErrorStringField(body, "error.message"))
}

// quotaErrorStringField reads path out of body only when it holds a JSON string.
//
// The type check is load-bearing, not defensive noise: gjson's Result.String()
// returns the RAW JSON text for object and array results (gjson.go, `case JSON:
// return t.Raw`), so a body shaped like
// {"error":{"message":{"account_email":"..."}}} would copy the entire nested
// object through the allow-list verbatim and persist it. Truncation cannot save
// that case because leak markers are short. Numbers / bools / null are dropped
// for the same reason the fields are documented as strings: no coercion into
// persisted text.
func quotaErrorStringField(body []byte, path string) string {
	result := gjson.GetBytes(body, path)
	if result.Type != gjson.String {
		return ""
	}
	return result.Str
}

func truncateQuotaErrorField(value string) string {
	value = strings.TrimSpace(value)
	runes := []rune(value)
	if len(runes) <= quotaErrorFieldMaxRunes {
		return value
	}
	return string(runes[:quotaErrorFieldMaxRunes]) + "..."
}

type quotaReauthRequiredError struct {
	Provider   string
	StatusCode int
	// RetryAfter / ErrorType / ErrorMessage carry the same observation as
	// quotaHTTPError. They are copied over rather than wrapped: adding Unwrap
	// here would make errors.As(err, **quotaHTTPError) start succeeding for
	// reauth errors and silently change quotaSnapshotErrorClass / reauth
	// classification, which must stay untouched.
	RetryAfter   time.Duration
	ErrorType    string
	ErrorMessage string
}

// Error keeps returning only the sanitized, tokenless message. Existing tests
// assert the upstream body never reaches this string, so the observation fields
// must not be rendered here.
func (e *quotaReauthRequiredError) Error() string {
	return quotaCredentialUnauthorizedMessage(e.Provider)
}

// quotaProbeObservation is the observability-only view of a failed quota probe.
// It never participates in scheduling, reauth or selection decisions.
type quotaProbeObservation struct {
	StatusCode   int
	RetryAfter   time.Duration
	ErrorType    string
	ErrorMessage string
}

// clearQuotaFailureObservation drops the last-failed-probe observability pair.
//
// It exists because quota_refresh_status has FIVE writers and only
// persistQuotaSnapshotError writes the observation. Every other writer moves the
// status to a value that contradicts a retained failure observation, so each one
// has to clear the pair or the account keeps advertising a stale upstream status
// after it recovered. The concrete regression: an account 429s
// (status=error, http_status=429, retry_after=300), a later liveness probe
// succeeds and flips status=ok — without this call the account still reports
// http_status=429 / retry_after=300 and the management view shows a rate limit
// that is long gone.
//
// Callers clear UNCONDITIONALLY, deliberately not gated on
// farmLivenessDetectionEnabled(): whether the pair is stale must not depend on
// which rollout flag happens to be armed, and a flag flip must not resurrect an
// old observation.
func clearQuotaFailureObservation(meta map[string]any) {
	if meta == nil {
		return
	}
	delete(meta, quotaRefreshHTTPStatusMetadataKey)
	delete(meta, quotaRefreshRetryAfterMetadataKey)
}

// quotaProbeObservationFromError reads the observation out of either failure
// error shape. Both are matched explicitly because quotaReauthErrorForProvider
// replaces a 401/403 quotaHTTPError with a quotaReauthRequiredError instead of
// wrapping it.
func quotaProbeObservationFromError(err error) quotaProbeObservation {
	if err == nil {
		return quotaProbeObservation{}
	}
	var httpErr *quotaHTTPError
	if errors.As(err, &httpErr) && httpErr != nil {
		return quotaProbeObservation{
			StatusCode:   httpErr.StatusCode,
			RetryAfter:   httpErr.RetryAfter,
			ErrorType:    httpErr.ErrorType,
			ErrorMessage: httpErr.ErrorMessage,
		}
	}
	var reauthErr *quotaReauthRequiredError
	if errors.As(err, &reauthErr) && reauthErr != nil {
		return quotaProbeObservation{
			StatusCode:   reauthErr.StatusCode,
			RetryAfter:   reauthErr.RetryAfter,
			ErrorType:    reauthErr.ErrorType,
			ErrorMessage: reauthErr.ErrorMessage,
		}
	}
	var persistedErr *quotaPersistedError
	if errors.As(err, &persistedErr) && persistedErr != nil {
		return persistedErr.observation
	}
	return quotaProbeObservation{}
}

// quotaPersistedError is what persistQuotaSnapshotError returns after recording
// a failure. Error() reproduces the exact message the previous
// fmt.Errorf("%s", message) produced, so every caller that matches on the text
// is unaffected; the struct only additionally ferries the observation to the
// caller for logging and for the refresh result, because the persisted message
// itself is deliberately tokenless and carries no status/Retry-After detail.
type quotaPersistedError struct {
	message     string
	observation quotaProbeObservation
}

func (e *quotaPersistedError) Error() string {
	return e.message
}

func quotaCredentialUnauthorizedMessage(provider string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "claude":
		return claudeQuotaCredentialUnauthorizedMessage
	case "codex":
		return codexQuotaCredentialUnauthorizedMessage
	default:
		return genericQuotaCredentialUnauthorizedMessage
	}
}

func quotaReauthErrorForProvider(provider string, err error) error {
	if code, ok := quotaHTTPStatusCode(err); ok && quotaHTTPStatusRequiresReauth(code) {
		observation := quotaProbeObservationFromError(err)
		return &quotaReauthRequiredError{
			Provider:     provider,
			StatusCode:   code,
			RetryAfter:   observation.RetryAfter,
			ErrorType:    observation.ErrorType,
			ErrorMessage: observation.ErrorMessage,
		}
	}
	return err
}

func quotaHTTPStatusCode(err error) (int, bool) {
	var httpErr *quotaHTTPError
	if errors.As(err, &httpErr) && httpErr != nil {
		return httpErr.StatusCode, true
	}
	return 0, false
}

func quotaHTTPStatusRequiresReauth(statusCode int) bool {
	return statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden
}

func quotaSnapshotErrorStatusAndMessage(err error) (string, string) {
	var reauthErr *quotaReauthRequiredError
	if errors.As(err, &reauthErr) && reauthErr != nil {
		return quotaRefreshStatusReauthRequired, reauthErr.Error()
	}
	if err == nil {
		return quotaRefreshStatusError, ""
	}
	return quotaRefreshStatusError, err.Error()
}

func quotaSnapshotErrorClass(err error, status string) string {
	switch status {
	case quotaRefreshStatusReauthRequired:
		return "reauth_required"
	case quotaRefreshStatusUnsupported:
		return "unsupported"
	}
	if err == nil {
		return ""
	}
	var httpErr *quotaHTTPError
	if errors.As(err, &httpErr) && httpErr != nil {
		return "http_status"
	}
	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
		return "timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	lower := strings.ToLower(err.Error())
	switch {
	case strings.Contains(lower, "connection not allowed by ruleset"):
		return "proxy_ruleset_reject"
	case strings.Contains(lower, "non-success status"):
		return "http_status"
	case strings.Contains(lower, "deadline exceeded") || strings.Contains(lower, "timeout") || strings.Contains(lower, "timed out"):
		return "timeout"
	case strings.Contains(lower, "executor unavailable"):
		return "executor_unavailable"
	default:
		return "provider_error"
	}
}

func quotaProviderTargetURLs(provider string) []string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "codex":
		return []string{"https://chatgpt.com/backend-api/wham/usage"}
	case "claude":
		return []string{
			"https://api.anthropic.com/api/oauth/profile",
			"https://api.anthropic.com/api/oauth/usage",
		}
	default:
		return nil
	}
}

func quotaSnapshotImplicitRefreshSkipped(auth *coreauth.Auth) bool {
	if auth == nil {
		return true
	}
	if metadataString(auth.Metadata, quotaRefreshStatusMetadataKey) == quotaRefreshStatusReauthRequired {
		return true
	}
	return quotaSnapshotLegacyReauthRequired(auth)
}

// quotaSnapshotAuthRecovered reports whether the credential itself is currently
// usable again, independent of a possibly-stale quota_refresh_status. After an
// operator re-authenticates, the credential becomes StatusActive (and not
// disabled/unavailable) even though an old reauth_required quota status may
// still linger in metadata. Such a recovered credential must not stay skipped
// forever on an explicit, user-initiated global quota refresh.
//
// Detection deliberately relies only on fields that are written fresh by the
// re-auth flow (Status / Disabled / Unavailable) and never on metadata flags
// such as refresh_disabled / reauth_required, which are operator-controlled and
// can be inherited stale across a re-auth round-trip.
func quotaSnapshotAuthRecovered(auth *coreauth.Auth) bool {
	if auth == nil || auth.Disabled || auth.Unavailable {
		return false
	}
	return auth.Status == coreauth.StatusActive
}

func quotaSnapshotLegacyReauthRequired(auth *coreauth.Auth) bool {
	if auth == nil {
		return false
	}
	if metadataString(auth.Metadata, quotaRefreshStatusMetadataKey) != quotaRefreshStatusError {
		return false
	}
	message := strings.ToLower(metadataString(auth.Metadata, quotaRefreshErrorMetadataKey))
	if message == "" {
		return false
	}
	hasAuthSignal := strings.Contains(message, "unauthorized") ||
		strings.Contains(message, "authentication_error") ||
		strings.Contains(message, "invalid authentication credentials") ||
		strings.Contains(message, "invalid token") ||
		strings.Contains(message, "forbidden")
	hasStatusSignal := strings.Contains(message, "401") || strings.Contains(message, "403")
	return hasAuthSignal && hasStatusSignal
}

func quotaSnapshotLegacyUnsupportedProviderError(auth *coreauth.Auth) bool {
	if auth == nil || auth.Metadata == nil || !quotaSnapshotProviderSupported(auth.Provider) {
		return false
	}
	status := strings.ToLower(strings.TrimSpace(metadataString(auth.Metadata, quotaRefreshStatusMetadataKey)))
	message := strings.TrimSpace(metadataString(auth.Metadata, quotaRefreshErrorMetadataKey))
	return status == quotaRefreshStatusUnsupported && strings.EqualFold(message, quotaUnsupportedProviderMessage)
}

func quotaResponseBodyReader(resp *http.Response) (io.ReadCloser, error) {
	if resp == nil || resp.Body == nil {
		return io.NopCloser(strings.NewReader("")), nil
	}
	encoding := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding")))
	switch encoding {
	case "", "identity":
		return resp.Body, nil
	case "gzip":
		reader, err := gzip.NewReader(resp.Body)
		if err != nil {
			return nil, err
		}
		return quotaReadCloser{
			Reader: reader,
			close: func() error {
				errClose := reader.Close()
				errBody := resp.Body.Close()
				if errClose != nil {
					return errClose
				}
				return errBody
			},
		}, nil
	case "br":
		return quotaReadCloser{
			Reader: brotli.NewReader(resp.Body),
			close:  resp.Body.Close,
		}, nil
	case "zstd":
		reader, err := zstd.NewReader(resp.Body)
		if err != nil {
			return nil, err
		}
		return quotaReadCloser{
			Reader: reader,
			close: func() error {
				reader.Close()
				return resp.Body.Close()
			},
		}, nil
	default:
		return resp.Body, nil
	}
}

type quotaReadCloser struct {
	io.Reader
	close func() error
}

func (r quotaReadCloser) Close() error {
	if r.close == nil {
		return nil
	}
	return r.close()
}

func normalizeQuotaJSONPayload(data []byte) []byte {
	text := string(data)
	if strings.ContainsRune(text, 0x1b) {
		text = stripQuotaANSIEscape(text)
	}
	text = strings.TrimPrefix(strings.TrimSpace(text), "\ufeff")
	if strings.HasPrefix(text, "{") {
		return []byte(text)
	}
	if idx := strings.Index(text, "{"); idx >= 0 {
		candidate := strings.TrimSpace(text[idx:])
		if json.Valid([]byte(candidate)) {
			return []byte(candidate)
		}
		if end := strings.LastIndex(candidate, "}"); end >= 0 {
			candidate = strings.TrimSpace(candidate[:end+1])
			if json.Valid([]byte(candidate)) {
				return []byte(candidate)
			}
		}
	}
	return []byte(text)
}

func stripQuotaANSIEscape(s string) string {
	in := []rune(s)
	var out []rune
	for i := 0; i < len(in); i++ {
		r := in[i]
		if r != 0x1b {
			out = append(out, r)
			continue
		}
		if i+1 >= len(in) {
			continue
		}
		next := in[i+1]
		switch next {
		case ']':
			i += 2
			for i < len(in) {
				if in[i] == 0x07 {
					break
				}
				if in[i] == 0x1b && i+1 < len(in) && in[i+1] == '\\' {
					i++
					break
				}
				i++
			}
		case '[':
			i += 2
			for i < len(in) {
				if (in[i] >= 'A' && in[i] <= 'Z') || (in[i] >= 'a' && in[i] <= 'z') {
					break
				}
				i++
			}
		default:
			// Drop a bare ESC and its immediate introducer.
		}
	}
	return string(out)
}

func inferClaudePlanType(profile map[string]any) string {
	if profile == nil {
		return ""
	}
	for _, key := range []string{"plan_type", "planType", "subscription_tier", "subscriptionTier"} {
		if value, ok := profile[key].(string); ok {
			if plan := normalizeClaudePlanType(value); plan != "" {
				return plan
			}
		}
	}
	if hasBool(profile, "has_claude_max", "hasClaudeMax", "has_max", "hasMax") {
		return "max"
	}
	if hasBool(profile, "has_claude_pro", "hasClaudePro", "has_pro", "hasPro") {
		return "pro"
	}
	if subscription, ok := profile["subscription"].(map[string]any); ok {
		if hasBool(subscription, "has_claude_max", "hasClaudeMax", "has_max", "hasMax") {
			return "max"
		}
		if hasBool(subscription, "has_claude_pro", "hasClaudePro", "has_pro", "hasPro") {
			return "pro"
		}
	}
	return ""
}

func claudeUsageCreditsEnabledFromQuotaSnapshot(meta map[string]any) bool {
	if hasBool(meta, "usage_credits_enabled", "extra_usage_enabled", "has_extra_usage_enabled") {
		return true
	}
	snapshot, _ := meta[quotaSnapshotMetadataKey].(map[string]any)
	usage, _ := snapshot["usage"].(map[string]any)
	extraUsage, _ := usage["extra_usage"].(map[string]any)
	if extraUsage == nil {
		extraUsage, _ = usage["extraUsage"].(map[string]any)
	}
	return hasBool(extraUsage, "is_enabled", "isEnabled", "enabled")
}

func inferCodexPlanType(auth *coreauth.Auth, usage map[string]any) string {
	if plan := firstNormalizedCodexPlanFromMap(usage); plan != "" {
		return plan
	}
	if auth == nil {
		return ""
	}
	if plan := firstNormalizedCodexPlanFromMap(auth.Metadata); plan != "" {
		return plan
	}
	if auth.Attributes != nil {
		for _, key := range codexPlanTypeKeys {
			if plan := registry.NormalizeCodexSubscriptionPlan(auth.Attributes[key]); plan != "" {
				return plan
			}
		}
	}
	return ""
}

var codexPlanTypeKeys = []string{"plan_type", "planType", "chatgpt_plan_type", "chatgptPlanType"}

func firstNormalizedCodexPlanFromMap(payload map[string]any) string {
	for _, key := range codexPlanTypeKeys {
		if plan := registry.NormalizeCodexSubscriptionPlan(stringValueFromMap(payload, key)); plan != "" {
			return plan
		}
	}
	return ""
}

func stringValueFromMap(payload map[string]any, key string) string {
	if len(payload) == 0 {
		return ""
	}
	value, ok := payload[key]
	if !ok {
		return ""
	}
	str, ok := value.(string)
	if !ok {
		return ""
	}
	return str
}

func normalizeClaudePlanType(raw string) string {
	lower := strings.ToLower(strings.TrimSpace(raw))
	switch {
	case strings.Contains(lower, "max"):
		return "max"
	case strings.Contains(lower, "pro"):
		return "pro"
	case strings.Contains(lower, "free"):
		return "free"
	default:
		return lower
	}
}

func hasBool(payload map[string]any, keys ...string) bool {
	for _, key := range keys {
		if value, ok := payload[key].(bool); ok && value {
			return true
		}
	}
	return false
}

func quotaSnapshotProviderSupported(provider string) bool {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "codex", "claude":
		return true
	default:
		return false
	}
}

func quotaSnapshotNextRefresh(auth *coreauth.Auth) (time.Time, bool) {
	if auth == nil {
		return time.Time{}, false
	}
	return metadataTime(auth.Metadata, quotaNextRefreshMetadataKey)
}

func metadataTime(meta map[string]any, key string) (time.Time, bool) {
	if meta == nil {
		return time.Time{}, false
	}
	raw, ok := meta[key]
	if !ok {
		return time.Time{}, false
	}
	switch value := raw.(type) {
	case time.Time:
		if !value.IsZero() {
			return value, true
		}
	case string:
		if ts, err := time.Parse(time.RFC3339, strings.TrimSpace(value)); err == nil && !ts.IsZero() {
			return ts, true
		}
	}
	return time.Time{}, false
}

func quotaSnapshotNextRefreshTime(auth *coreauth.Auth, now time.Time, policy QuotaSnapshotRefreshPolicy) time.Time {
	policy = policy.normalized()
	return now.Add(policy.Interval + quotaSnapshotJitter(auth, now, policy.Jitter))
}

func quotaSnapshotInitialRefreshTime(auth *coreauth.Auth, now time.Time, policy QuotaSnapshotRefreshPolicy) time.Time {
	policy = policy.normalized()
	return now.Add(quotaSnapshotJitter(auth, now, policy.Jitter))
}

func quotaSnapshotNextRefreshBeyondPolicy(auth *coreauth.Auth, next, now time.Time, policy QuotaSnapshotRefreshPolicy) bool {
	policy = policy.normalized()
	latest := quotaSnapshotNextRefreshTime(auth, now, policy).Add(quotaSnapshotRefreshPollInterval)
	return next.After(latest)
}

func quotaSnapshotStartupCatchUpNeeded(auth *coreauth.Auth, now time.Time, policy QuotaSnapshotRefreshPolicy, hasNext bool) bool {
	policy = policy.normalized()
	if !policy.StartupCatchUp {
		return false
	}
	if !hasNext {
		return true
	}
	if auth == nil {
		return false
	}
	if _, ok := auth.Metadata[quotaSnapshotMetadataKey].(map[string]any); !ok {
		return true
	}
	if strings.EqualFold(metadataString(auth.Metadata, quotaRefreshStatusMetadataKey), quotaRefreshStatusStale) {
		return true
	}
	lastRefreshedAt, ok := metadataTime(auth.Metadata, quotaLastRefreshedMetadataKey)
	if !ok {
		return true
	}
	return policy.StartupMaxStaleness > 0 && now.Sub(lastRefreshedAt) >= policy.StartupMaxStaleness
}

func quotaSnapshotStartupCatchUpRefreshTime(auth *coreauth.Auth, now time.Time, policy QuotaSnapshotRefreshPolicy) time.Time {
	policy = policy.normalized()
	jitterMax := policy.Jitter
	if jitterMax > quotaSnapshotStartupJitterMax {
		jitterMax = quotaSnapshotStartupJitterMax
	}
	return now.Add(quotaSnapshotJitter(auth, now, jitterMax))
}

func quotaSnapshotJitter(auth *coreauth.Auth, now time.Time, max time.Duration) time.Duration {
	if max <= 0 {
		return 0
	}
	h := fnv.New64a()
	if auth != nil {
		_, _ = h.Write([]byte(auth.ID))
		_, _ = h.Write([]byte(auth.Provider))
	}
	_, _ = h.Write([]byte(now.UTC().Format("200601021504")))
	return time.Duration(int64(h.Sum64() % uint64(max)))
}

func (h *Handler) currentAuthManager() *coreauth.Manager {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	manager := h.authManager
	h.mu.Unlock()
	return manager
}
