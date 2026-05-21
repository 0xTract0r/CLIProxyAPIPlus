package management

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
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

	quotaRefreshStatusOK              = "ok"
	quotaRefreshStatusStale           = "stale"
	quotaRefreshStatusError           = "error"
	quotaRefreshStatusReauthRequired  = "reauth_required"
	quotaRefreshStatusRefreshDisabled = "refresh_disabled"

	claudeQuotaCredentialUnauthorizedMessage  = "Claude credential unauthorized; reauthenticate this credential to refresh quota."
	codexQuotaCredentialUnauthorizedMessage   = "Codex credential unauthorized; reauthenticate this credential to refresh quota."
	genericQuotaCredentialUnauthorizedMessage = "Credential unauthorized; reauthenticate this credential to refresh quota."

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
		if quotaSnapshotImplicitRefreshSkipped(auth) {
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
		if quotaRefreshHasImplicitSupportedTargets(manager, req) {
			c.JSON(http.StatusOK, quotaSnapshotPayload{
				GeneratedAt: time.Now().UTC(),
				Entries:     h.quotaSnapshotEntries(),
			})
			return
		}
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
		if quotaSnapshotImplicitRefreshSkipped(auth) {
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
		Status:    status,
		Error:     errMessage,
		PlanType:  metadataString(auth.Metadata, quotaSnapshotPlanTypeKey),
	}
	if entry.Status == "" {
		entry.Status = quotaRefreshStatusStale
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
		status, message := quotaSnapshotErrorStatusAndMessage(err)
		return h.persistQuotaSnapshotError(ctx, auth, status, message, interval)
	}

	updated := auth.Clone()
	if updated.Metadata == nil {
		updated.Metadata = make(map[string]any)
	}
	updated.Metadata[quotaSnapshotMetadataKey] = snapshot
	updated.Metadata[quotaRefreshStatusMetadataKey] = quotaRefreshStatusOK
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
	if status == quotaRefreshStatusReauthRequired {
		delete(updated.Metadata, quotaSnapshotPlanTypeKey)
		delete(updated.Metadata, quotaSnapshotMetadataKey)
	}
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
	return "quota endpoint returned non-success status"
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

func quotaSnapshotImplicitRefreshSkipped(auth *coreauth.Auth) bool {
	if auth == nil {
		return true
	}
	if auth.RefreshDisabled() {
		return true
	}
	if metadataString(auth.Metadata, quotaRefreshStatusMetadataKey) == quotaRefreshStatusReauthRequired {
		return true
	}
	return quotaSnapshotLegacyReauthRequired(auth)
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
