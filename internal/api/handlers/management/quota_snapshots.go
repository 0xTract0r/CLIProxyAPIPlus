package management

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const (
	quotaSnapshotMetadataKey      = "quota_snapshot"
	quotaRefreshStatusMetadataKey = "quota_refresh_status"
	quotaRefreshErrorMetadataKey  = "quota_refresh_error"
	quotaLastRefreshedMetadataKey = "quota_last_refreshed_at"
	quotaNextRefreshMetadataKey   = "quota_next_refresh_after"
	quotaSnapshotPlanTypeKey      = "plan_type"

	defaultQuotaSnapshotRefreshInterval = 45 * time.Minute
	quotaSnapshotRefreshJitterMax       = 10 * time.Minute
	quotaSnapshotRefreshScanInterval    = time.Minute
)

type quotaSnapshotEntry struct {
	AuthID          string         `json:"auth_id"`
	AuthIndex       string         `json:"auth_index,omitempty"`
	Name            string         `json:"name,omitempty"`
	Provider        string         `json:"provider"`
	Label           string         `json:"label,omitempty"`
	Status          string         `json:"status"`
	Error           string         `json:"error,omitempty"`
	PlanType        string         `json:"plan_type,omitempty"`
	LastRefreshedAt time.Time      `json:"last_refreshed_at,omitempty"`
	NextRefreshAt   time.Time      `json:"next_refresh_at,omitempty"`
	Snapshot        map[string]any `json:"snapshot,omitempty"`
}

type quotaRefreshRequest struct {
	AuthID   string `json:"auth_id"`
	Name     string `json:"name"`
	Provider string `json:"provider"`
}

type quotaSnapshotPayload struct {
	GeneratedAt time.Time            `json:"generated_at"`
	Entries     []quotaSnapshotEntry `json:"entries"`
}

// StartQuotaSnapshotAutoRefresh launches the core-owned quota refresher. The
// management UI should read these persisted snapshots instead of directly
// fanning out provider quota API calls on page entry.
func (h *Handler) StartQuotaSnapshotAutoRefresh(parent context.Context, interval time.Duration) {
	if h == nil {
		return
	}
	if parent == nil {
		parent = context.Background()
	}
	if interval <= 0 {
		interval = defaultQuotaSnapshotRefreshInterval
	}

	h.mu.Lock()
	cancelPrev := h.quotaRefreshCancel
	h.quotaRefreshCancel = nil
	h.mu.Unlock()
	if cancelPrev != nil {
		cancelPrev()
	}

	ctx, cancel := context.WithCancel(parent)
	h.mu.Lock()
	h.quotaRefreshCancel = cancel
	h.mu.Unlock()

	go h.runQuotaSnapshotAutoRefresh(ctx, interval)
}

func (h *Handler) runQuotaSnapshotAutoRefresh(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(quotaSnapshotRefreshScanInterval)
	defer ticker.Stop()
	h.refreshDueQuotaSnapshots(ctx, interval)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			h.refreshDueQuotaSnapshots(ctx, interval)
		}
	}
}

func (h *Handler) refreshDueQuotaSnapshots(ctx context.Context, interval time.Duration) {
	manager := h.currentAuthManager()
	if manager == nil {
		return
	}
	now := time.Now().UTC()
	for _, auth := range manager.List() {
		if auth == nil || auth.Disabled || !quotaSnapshotProviderSupported(auth.Provider) {
			continue
		}
		if next, ok := quotaSnapshotNextRefresh(auth); ok {
			if next.After(now) {
				continue
			}
		} else {
			if err := h.persistQuotaSnapshotSchedule(ctx, auth, quotaSnapshotInitialRefreshTime(auth, now)); err != nil && !strings.Contains(err.Error(), context.Canceled.Error()) {
				log.WithError(err).Debugf("management quota: schedule failed for %s/%s", auth.Provider, auth.ID)
			}
			continue
		}
		if _, err := h.refreshQuotaSnapshot(ctx, auth, interval); err != nil && !strings.Contains(err.Error(), context.Canceled.Error()) {
			log.WithError(err).Debugf("management quota: refresh failed for %s/%s", auth.Provider, auth.ID)
		}
	}
}

// GetQuotaSnapshots returns persisted core quota snapshots without contacting
// upstream providers.
func (h *Handler) GetQuotaSnapshots(c *gin.Context) {
	c.JSON(http.StatusOK, quotaSnapshotPayload{
		GeneratedAt: time.Now().UTC(),
		Entries:     h.quotaSnapshotEntries(),
	})
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
		c.JSON(http.StatusNotFound, gin.H{"error": "no supported quota auth found"})
		return
	}

	for _, auth := range targets {
		_, _ = h.refreshQuotaSnapshot(c.Request.Context(), auth, defaultQuotaSnapshotRefreshInterval)
	}

	c.JSON(http.StatusOK, quotaSnapshotPayload{
		GeneratedAt: time.Now().UTC(),
		Entries:     h.quotaSnapshotEntries(),
	})
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
		if provider != "" && strings.ToLower(auth.Provider) != provider {
			continue
		}
		targets = append(targets, auth)
	}
	return targets
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
	entry := quotaSnapshotEntry{
		AuthID:    auth.ID,
		AuthIndex: auth.Index,
		Name:      auth.FileName,
		Provider:  auth.Provider,
		Label:     authDisplayName(auth),
		Status:    metadataString(auth.Metadata, quotaRefreshStatusMetadataKey),
		Error:     metadataString(auth.Metadata, quotaRefreshErrorMetadataKey),
		PlanType:  metadataString(auth.Metadata, quotaSnapshotPlanTypeKey),
	}
	if entry.Status == "" {
		entry.Status = "stale"
	}
	if ts, ok := metadataTime(auth.Metadata, quotaLastRefreshedMetadataKey); ok {
		entry.LastRefreshedAt = ts
	}
	if ts, ok := metadataTime(auth.Metadata, quotaNextRefreshMetadataKey); ok {
		entry.NextRefreshAt = ts
	}
	if snapshot, ok := auth.Metadata[quotaSnapshotMetadataKey].(map[string]any); ok {
		entry.Snapshot = snapshot
	}
	return entry
}

func (h *Handler) refreshQuotaSnapshot(ctx context.Context, auth *coreauth.Auth, interval time.Duration) (*coreauth.Auth, error) {
	manager := h.currentAuthManager()
	if manager == nil || auth == nil {
		return auth, fmt.Errorf("auth manager unavailable")
	}
	exec, ok := manager.Executor(auth.Provider)
	if !ok || exec == nil {
		return h.persistQuotaSnapshotError(ctx, auth, "unsupported", "provider does not support quota refresh", interval)
	}

	now := time.Now().UTC()
	snapshot, planType, err := fetchProviderQuotaSnapshot(ctx, exec, auth)
	if err != nil {
		return h.persistQuotaSnapshotError(ctx, auth, "error", err.Error(), interval)
	}

	updated := auth.Clone()
	if updated.Metadata == nil {
		updated.Metadata = make(map[string]any)
	}
	updated.Metadata[quotaSnapshotMetadataKey] = snapshot
	updated.Metadata[quotaRefreshStatusMetadataKey] = "ok"
	delete(updated.Metadata, quotaRefreshErrorMetadataKey)
	updated.Metadata[quotaLastRefreshedMetadataKey] = now.Format(time.RFC3339)
	updated.Metadata[quotaNextRefreshMetadataKey] = quotaSnapshotNextRefreshTime(updated, now, interval).Format(time.RFC3339)
	if planType != "" {
		updated.Metadata[quotaSnapshotPlanTypeKey] = planType
	}
	updated.UpdatedAt = now
	return manager.Update(ctx, updated)
}

func (h *Handler) persistQuotaSnapshotError(ctx context.Context, auth *coreauth.Auth, status, message string, interval time.Duration) (*coreauth.Auth, error) {
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
	updated.Metadata[quotaNextRefreshMetadataKey] = quotaSnapshotNextRefreshTime(updated, now, interval).Format(time.RFC3339)
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
			return nil, "", err
		}
		return map[string]any{"usage": payload}, inferCodexPlanType(auth, payload), nil
	case "claude":
		headers := http.Header{"anthropic-beta": []string{"oauth-2025-04-20"}}
		profile, err := fetchQuotaJSON(ctx, exec, auth, http.MethodGet, "https://api.anthropic.com/api/oauth/profile", headers)
		if err != nil {
			return nil, "", err
		}
		usage, err := fetchQuotaJSON(ctx, exec, auth, http.MethodGet, "https://api.anthropic.com/api/oauth/usage", headers)
		if err != nil {
			return nil, "", err
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
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("quota endpoint returned %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	var payload map[string]any
	if len(data) > 0 {
		if err := json.Unmarshal(data, &payload); err != nil {
			return nil, err
		}
	}
	if payload == nil {
		payload = make(map[string]any)
	}
	return payload, nil
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
	for _, value := range []string{
		metadataString(auth.Metadata, "plan_type"),
		stringValueFromMap(usage, "plan_type"),
		stringValueFromMap(usage, "planType"),
		stringValueFromMap(usage, "chatgpt_plan_type"),
		stringValueFromMap(usage, "chatgptPlanType"),
	} {
		if plan := normalizeClaudePlanType(value); plan != "" {
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

func quotaSnapshotNextRefreshTime(auth *coreauth.Auth, now time.Time, interval time.Duration) time.Time {
	if interval <= 0 {
		interval = defaultQuotaSnapshotRefreshInterval
	}
	return now.Add(interval + quotaSnapshotJitter(auth, now))
}

func quotaSnapshotInitialRefreshTime(auth *coreauth.Auth, now time.Time) time.Time {
	return now.Add(quotaSnapshotJitter(auth, now))
}

func quotaSnapshotJitter(auth *coreauth.Auth, now time.Time) time.Duration {
	if quotaSnapshotRefreshJitterMax <= 0 {
		return 0
	}
	h := fnv.New64a()
	if auth != nil {
		_, _ = h.Write([]byte(auth.ID))
		_, _ = h.Write([]byte(auth.Provider))
	}
	_, _ = h.Write([]byte(now.UTC().Format("200601021504")))
	return time.Duration(int64(h.Sum64() % uint64(quotaSnapshotRefreshJitterMax)))
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
