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
	"strings"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const (
	quotaSnapshotMetadataKey      = "quota_snapshot"
	quotaRefreshStatusMetadataKey = "quota_refresh_status"
	quotaRefreshErrorMetadataKey  = "quota_refresh_error"
	quotaLastRefreshedMetadataKey = "quota_last_refreshed_at"
	quotaNextRefreshMetadataKey   = "quota_next_refresh_after"
	quotaSnapshotPlanTypeKey      = "plan_type"

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
	AuthID      string   `json:"auth_id"`
	AuthIndex   string   `json:"auth_index,omitempty"`
	Name        string   `json:"name,omitempty"`
	Provider    string   `json:"provider"`
	Label       string   `json:"label,omitempty"`
	Status      string   `json:"status"`
	Error       string   `json:"error,omitempty"`
	ErrorClass  string   `json:"error_class,omitempty"`
	ElapsedMS   int64    `json:"elapsed_ms"`
	Refreshed   bool     `json:"refreshed"`
	ProxySource string   `json:"proxy_source,omitempty"`
	ProxyHash   string   `json:"proxy_hash,omitempty"`
	TargetURLs  []string `json:"target_urls,omitempty"`
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
		// Recovered (StatusActive) accounts may still carry a stale
		// quota_refresh_status=reauth_required written by an earlier transient
		// 401/403. Mirror the explicit-refresh path (quotaRefreshTargets) so the
		// implicit skip does not pin them forever; the next-refresh schedule below
		// still throttles re-probing so genuinely unauthorized accounts are not
		// hammered.
		if quotaSnapshotImplicitRefreshSkipped(auth) && !quotaSnapshotAuthRecovered(auth) {
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
		if _, err := h.refreshQuotaSnapshot(ctx, auth, policy); err != nil && !strings.Contains(err.Error(), context.Canceled.Error()) {
			log.WithError(err).Debugf("management quota: refresh failed for %s/%s", auth.Provider, auth.ID)
		}
	}
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

func (h *Handler) refreshQuotaSnapshot(ctx context.Context, auth *coreauth.Auth, policy QuotaSnapshotRefreshPolicy) (*coreauth.Auth, error) {
	policy = policy.normalized()
	manager := h.currentAuthManager()
	if manager == nil || auth == nil {
		return auth, fmt.Errorf("auth manager unavailable")
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
		return h.persistQuotaSnapshotError(ctx, auth, quotaRefreshStatusUnsupported, quotaUnsupportedProviderMessage, policy)
	}

	now := time.Now().UTC()
	providerCtx, cancel := context.WithTimeout(ctx, policy.ProviderTimeout)
	defer cancel()
	snapshot, planType, err := fetchProviderQuotaSnapshot(providerCtx, exec, auth)
	if err != nil {
		status, message := quotaSnapshotErrorStatusAndMessage(err)
		return h.persistQuotaSnapshotError(ctx, auth, status, message, policy)
	}

	updated := auth.Clone()
	if updated.Metadata == nil {
		updated.Metadata = make(map[string]any)
	}
	updated.Metadata[quotaSnapshotMetadataKey] = snapshot
	updated.Metadata[quotaRefreshStatusMetadataKey] = quotaRefreshStatusOK
	delete(updated.Metadata, quotaRefreshErrorMetadataKey)
	updated.Metadata[quotaLastRefreshedMetadataKey] = now.Format(time.RFC3339)
	updated.Metadata[quotaNextRefreshMetadataKey] = quotaSnapshotNextRefreshTime(updated, now, policy).Format(time.RFC3339)
	if planType != "" {
		updated.Metadata[quotaSnapshotPlanTypeKey] = planType
	}
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
	if err != nil {
		result.Refreshed = false
		result.ErrorClass = quotaSnapshotErrorClass(err, result.Status)
		if result.Error == "" {
			result.Error = err.Error()
		}
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

func (h *Handler) persistQuotaSnapshotError(ctx context.Context, auth *coreauth.Auth, status, message string, policy QuotaSnapshotRefreshPolicy) (*coreauth.Auth, error) {
	policy = policy.normalized()
	manager := h.currentAuthManager()
	if manager == nil || auth == nil {
		return auth, fmt.Errorf("auth manager unavailable")
	}
	now := time.Now().UTC()
	updated := auth.Clone()
	if updated.Metadata == nil {
		updated.Metadata = make(map[string]any)
	}
	updated.Metadata[quotaRefreshStatusMetadataKey] = status
	updated.Metadata[quotaRefreshErrorMetadataKey] = message
	updated.Metadata[quotaNextRefreshMetadataKey] = quotaSnapshotNextRefreshTime(updated, now, policy).Format(time.RFC3339)
	updated.UpdatedAt = now
	saved, err := manager.Update(ctx, updated)
	if err != nil {
		return saved, err
	}
	return saved, fmt.Errorf("%s", message)
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
		if resp.Body != nil {
			_ = resp.Body.Close()
		}
		return nil, &quotaHTTPError{StatusCode: resp.StatusCode}
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

type quotaHTTPError struct {
	StatusCode int
}

func (e *quotaHTTPError) Error() string {
	return fmt.Sprintf("quota endpoint returned non-success status %d", e.StatusCode)
}

type quotaReauthRequiredError struct {
	Provider   string
	StatusCode int
}

func (e *quotaReauthRequiredError) Error() string {
	return quotaCredentialUnauthorizedMessage(e.Provider)
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
		return &quotaReauthRequiredError{Provider: provider, StatusCode: code}
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
