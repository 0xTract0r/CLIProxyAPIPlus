package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	baseauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth"
)

// PostAuthHook defines a function that is called after an Auth record is created
// but before it is persisted to storage. This allows for modification of the
// Auth record (e.g., injecting metadata) based on external context.
type PostAuthHook func(context.Context, *Auth) error

// RequestInfo holds information extracted from the HTTP request.
// It is injected into the context passed to PostAuthHook.
type RequestInfo struct {
	Query   url.Values
	Headers http.Header
}

type requestInfoKey struct{}

// WithRequestInfo returns a new context with the given RequestInfo attached.
func WithRequestInfo(ctx context.Context, info *RequestInfo) context.Context {
	return context.WithValue(ctx, requestInfoKey{}, info)
}

// GetRequestInfo retrieves the RequestInfo from the context, if present.
func GetRequestInfo(ctx context.Context) *RequestInfo {
	if val, ok := ctx.Value(requestInfoKey{}).(*RequestInfo); ok {
		return val
	}
	return nil
}

// Auth encapsulates the runtime state and metadata associated with a single credential.
type Auth struct {
	// ID uniquely identifies the auth record across restarts.
	ID string `json:"id"`
	// Index is a stable runtime identifier derived from auth metadata (not persisted).
	Index string `json:"-"`
	// Provider is the upstream provider key (e.g. "gemini", "claude").
	Provider string `json:"provider"`
	// Prefix optionally namespaces models for routing (e.g., "teamA/gemini-3-pro-preview").
	Prefix string `json:"prefix,omitempty"`
	// FileName stores the relative or absolute path of the backing auth file.
	FileName string `json:"-"`
	// Storage holds the token persistence implementation used during login flows.
	Storage baseauth.TokenStorage `json:"-"`
	// Label is an optional human readable label for logging.
	Label string `json:"label,omitempty"`
	// Status is the lifecycle status managed by the AuthManager.
	Status Status `json:"status"`
	// StatusMessage holds a short description for the current status.
	StatusMessage string `json:"status_message,omitempty"`
	// Disabled indicates the auth is intentionally disabled by operator.
	Disabled bool `json:"disabled"`
	// Unavailable flags transient provider unavailability (e.g. quota exceeded).
	Unavailable bool `json:"unavailable"`
	// ProxyURL overrides the global proxy setting for this auth if provided.
	ProxyURL string `json:"proxy_url,omitempty"`
	// Attributes stores provider specific metadata needed by executors (immutable configuration).
	Attributes map[string]string `json:"attributes,omitempty"`
	// Metadata stores runtime mutable provider state (e.g. tokens, cookies).
	Metadata map[string]any `json:"metadata,omitempty"`
	// Quota captures recent quota information for load balancers.
	Quota QuotaState `json:"quota"`
	// LastError stores the last failure encountered while executing or refreshing.
	LastError *Error `json:"last_error,omitempty"`
	// CreatedAt is the creation timestamp in UTC.
	CreatedAt time.Time `json:"created_at"`
	// UpdatedAt is the last modification timestamp in UTC.
	UpdatedAt time.Time `json:"updated_at"`
	// LastRefreshedAt records the last successful refresh time in UTC.
	LastRefreshedAt time.Time `json:"last_refreshed_at"`
	// NextRefreshAfter is the earliest time a refresh should retrigger.
	NextRefreshAfter time.Time `json:"next_refresh_after"`
	// NextRetryAfter is the earliest time a retry should retrigger.
	NextRetryAfter time.Time `json:"next_retry_after"`
	// ModelStates tracks per-model runtime availability data.
	ModelStates map[string]*ModelState `json:"model_states,omitempty"`

	// CyberPolicyFlagCount counts how many times the upstream returned a
	// cyber_policy flag for this auth (currently Codex /v1/responses).
	CyberPolicyFlagCount int `json:"cyber_policy_flag_count,omitempty"`
	// LastCyberPolicyAt records the timestamp of the most recent cyber_policy hit.
	LastCyberPolicyAt time.Time `json:"last_cyber_policy_at,omitempty"`

	// AutoQuarantined marks that this credential was automatically quarantined
	// by the auth manager after repeated terminal authentication failures
	// (e.g. a revoked OAuth token producing HTTP 401 authentication_error)
	// within a short rolling window with zero intervening successes. It is
	// the canonical gating flag consulted by the selector/scheduler and is
	// intentionally distinct from the operator-controlled Disabled flag:
	// operators must explicitly flip Disabled and core never auto-clears it,
	// whereas AutoQuarantined is a heuristic safety net that core lifts by
	// itself the moment this credential is re-authenticated (see
	// saveTokenRecord in the management API) or produces a real successful
	// request (see MarkResult). See markAutoQuarantine / clearAutoQuarantine
	// in conductor.go.
	AutoQuarantined bool `json:"auto_quarantined,omitempty"`
	// QuarantineReason is a short, sanitized classification code describing
	// why AutoQuarantined was set (e.g. "terminal_auth_failure"). It never
	// echoes raw upstream error bodies, mirroring the sanitization used for
	// terminal refresh failures elsewhere in this file.
	QuarantineReason string `json:"quarantine_reason,omitempty"`
	// QuarantinedAt records when AutoQuarantined was most recently set.
	QuarantinedAt time.Time `json:"quarantined_at,omitempty"`

	// Runtime carries non-serialisable data used during execution (in-memory only).
	Runtime any `json:"-"`

	Success int64 `json:"-"`
	Failed  int64 `json:"-"`

	recentRequests recentRequestRing `json:"-"`
	indexAssigned  bool              `json:"-"`

	// terminalAuthFailureStreak / terminalAuthFailureStreakStartAt track a
	// rolling in-memory streak of terminal auth/permission failures (see
	// isTerminalAuthQuarantineResultError) with zero intervening successes.
	// They are intentionally not persisted (like Success/Failed above): a
	// process restart simply starts the streak fresh, which is a strict
	// safety bias (never quarantines more eagerly than a live process would).
	// Once AutoQuarantined is set it IS persisted, so the quarantine itself
	// survives restarts even though the streak bookkeeping does not.
	terminalAuthFailureStreak        int       `json:"-"`
	terminalAuthFailureStreakStartAt time.Time `json:"-"`

	// quarantineStateAt is an in-memory-only (never persisted) freshness clock
	// for the AutoQuarantined lock: markAutoQuarantine/clearAutoQuarantine
	// always stamp it to the real wall-clock time they run at, whether or not
	// the lock's value actually changed. Unlike QuarantinedAt (which is
	// exported/persisted and intentionally zeroed on every clear, so it can
	// only ever mean "currently active since"), this field only ever moves
	// forward and survives Clone unchanged -- exactly like LastRefreshedAt
	// does for token freshness (see tokenOwnedFreshness). Manager.Update's
	// stale write-back guard (preserveQuarantineFieldsOnStaleWriteback in
	// conductor.go) compares it against the live entry's own value to tell
	// "this record's quarantine fields are at least as current as the live
	// entry's" apart from "this is a clone that predates a concurrent
	// mark/clear" -- both a stale unaware clone and a legitimate clear reach
	// byte-identical zero-value AutoQuarantined/QuarantineReason/QuarantinedAt,
	// so a real timestamp comparison is the only reliable way to tell them
	// apart.
	quarantineStateAt time.Time `json:"-"`
}

// ClearAutoQuarantine releases the automatic quarantine lock set by
// markAutoQuarantine after repeated terminal authentication failures. It is
// exported so operator-facing recovery actions outside this package (a
// completed OAuth re-auth save, or an explicit re-enable via the management
// API) can lift the lock the instant they represent a legitimate "give this
// credential another chance" signal, without waiting for a live proxied
// request to succeed through MarkResult (which would never happen on its own
// since a quarantined credential is skipped by the selector).
func (a *Auth) ClearAutoQuarantine() {
	if a == nil {
		return
	}
	clearAutoQuarantine(a, time.Now())
}

const (
	recentRequestBucketSeconds int64 = 10 * 60
	recentRequestBucketCount         = 20
)

type recentRequestBucket struct {
	bucketID int64
	success  int64
	failed   int64
}

type recentRequestRing struct {
	buckets [recentRequestBucketCount]recentRequestBucket
}

type RecentRequestBucket struct {
	Time    string `json:"time"`
	Success int64  `json:"success"`
	Failed  int64  `json:"failed"`
}

// QuotaState contains limiter tracking data for a credential.
type QuotaState struct {
	// Exceeded indicates the credential recently hit a quota error.
	Exceeded bool `json:"exceeded"`
	// Reason provides an optional provider specific human readable description.
	Reason string `json:"reason,omitempty"`
	// NextRecoverAt is when the credential may become available again.
	NextRecoverAt time.Time `json:"next_recover_at"`
	// BackoffLevel stores the progressive cooldown exponent used for rate limits.
	BackoffLevel int `json:"backoff_level,omitempty"`
}

// ModelState captures the execution state for a specific model under an auth entry.
type ModelState struct {
	// Status reflects the lifecycle status for this model.
	Status Status `json:"status"`
	// StatusMessage provides an optional short description of the status.
	StatusMessage string `json:"status_message,omitempty"`
	// Unavailable mirrors whether the model is temporarily blocked for retries.
	Unavailable bool `json:"unavailable"`
	// NextRetryAfter defines the per-model retry time.
	NextRetryAfter time.Time `json:"next_retry_after"`
	// LastError records the latest error observed for this model.
	LastError *Error `json:"last_error,omitempty"`
	// Quota retains quota information if this model hit rate limits.
	Quota QuotaState `json:"quota"`
	// UpdatedAt tracks the last update timestamp for this model state.
	UpdatedAt time.Time `json:"updated_at"`
}

func recentRequestBucketID(now time.Time) int64 {
	if now.IsZero() {
		return 0
	}
	return now.Unix() / recentRequestBucketSeconds
}

func recentRequestBucketIndex(bucketID int64) int {
	mod := bucketID % int64(recentRequestBucketCount)
	if mod < 0 {
		mod += int64(recentRequestBucketCount)
	}
	return int(mod)
}

func formatRecentRequestBucketLabel(bucketID int64) string {
	start := time.Unix(bucketID*recentRequestBucketSeconds, 0).In(time.Local)
	end := start.Add(time.Duration(recentRequestBucketSeconds) * time.Second)
	return start.Format("15:04") + "-" + end.Format("15:04")
}

func (a *Auth) recordRecentRequest(now time.Time, success bool) {
	if a == nil {
		return
	}
	bucketID := recentRequestBucketID(now)
	idx := recentRequestBucketIndex(bucketID)
	bucket := &a.recentRequests.buckets[idx]
	if bucket.bucketID != bucketID {
		bucket.bucketID = bucketID
		bucket.success = 0
		bucket.failed = 0
	}
	if success {
		bucket.success++
		return
	}
	bucket.failed++
}

func (a *Auth) RecentRequestsSnapshot(now time.Time) []RecentRequestBucket {
	out := make([]RecentRequestBucket, 0, recentRequestBucketCount)
	if a == nil {
		return out
	}

	currentBucketID := recentRequestBucketID(now)
	for i := recentRequestBucketCount - 1; i >= 0; i-- {
		bucketID := currentBucketID - int64(i)
		idx := recentRequestBucketIndex(bucketID)
		bucket := a.recentRequests.buckets[idx]
		entry := RecentRequestBucket{
			Time: formatRecentRequestBucketLabel(bucketID),
		}
		if bucket.bucketID == bucketID {
			entry.Success = bucket.success
			entry.Failed = bucket.failed
		}
		out = append(out, entry)
	}

	return out
}

// Clone shallow copies the Auth structure, duplicating maps to avoid accidental mutation.
func (a *Auth) Clone() *Auth {
	if a == nil {
		return nil
	}
	copyAuth := *a
	if len(a.Attributes) > 0 {
		copyAuth.Attributes = make(map[string]string, len(a.Attributes))
		for key, value := range a.Attributes {
			copyAuth.Attributes[key] = value
		}
	}
	if len(a.Metadata) > 0 {
		copyAuth.Metadata = make(map[string]any, len(a.Metadata))
		for key, value := range a.Metadata {
			copyAuth.Metadata[key] = value
		}
	}
	if len(a.ModelStates) > 0 {
		copyAuth.ModelStates = make(map[string]*ModelState, len(a.ModelStates))
		for key, state := range a.ModelStates {
			copyAuth.ModelStates[key] = state.Clone()
		}
	}
	copyAuth.Runtime = a.Runtime
	// quarantineStateAt is a plain value copy (like LastRefreshedAt): it must
	// survive Clone unchanged so an internal re-clone within the same request
	// (e.g. syncAuthManagedHeaderState) does not lose the freshness signal
	// preserveQuarantineFieldsOnStaleWriteback relies on. See its field
	// comment above.
	return &copyAuth
}

func stableAuthIndex(seed string) string {
	seed = strings.TrimSpace(seed)
	if seed == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:8])
}

func (a *Auth) indexSeed() string {
	if a == nil {
		return ""
	}

	provider := strings.ToLower(strings.TrimSpace(a.Provider))
	compatName := ""
	baseURL := ""
	apiKey := ""
	filePath := ""
	if a.Attributes != nil {
		compatName = strings.TrimSpace(a.Attributes["compat_name"])
		baseURL = strings.TrimSpace(a.Attributes["base_url"])
		apiKey = strings.TrimSpace(a.Attributes["api_key"])
		filePath = strings.TrimSpace(a.Attributes["path"])
		if filePath == "" {
			filePath = strings.TrimSpace(a.Attributes["source"])
		}
	}

	if filePath == "" {
		filePath = strings.TrimSpace(a.FileName)
	}
	if filePath == "" {
		filePath = strings.TrimSpace(a.ID)
	}

	if filePath != "" && strings.HasSuffix(strings.ToLower(filePath), ".json") {
		abs, errAbs := filepath.Abs(filePath)
		if errAbs == nil && strings.TrimSpace(abs) != "" {
			filePath = abs
		}
		filePath = filepath.Clean(filePath)

		authType := ""
		if a.Metadata != nil {
			if rawType, ok := a.Metadata["type"].(string); ok {
				authType = strings.TrimSpace(rawType)
			}
		}
		if authType == "" {
			authType = strings.TrimSpace(provider)
		}
		authType = strings.ToLower(strings.TrimSpace(authType))
		if authType != "" {
			return authType + ":" + filePath
		}
	}

	apiPrefix := ""
	if apiKey != "" {
		switch {
		case compatName != "" || strings.EqualFold(provider, "openai-compatibility"):
			apiPrefix = "openai-compatibility"
		case strings.EqualFold(provider, "gemini"):
			apiPrefix = "gemini-api-key"
		case strings.EqualFold(provider, "codex"):
			apiPrefix = "codex-api-key"
		case strings.EqualFold(provider, "claude"):
			apiPrefix = "claude-api-key"
		}
	}
	if apiPrefix != "" {
		return apiPrefix + ":" + strings.TrimSpace(baseURL) + "+" + strings.TrimSpace(apiKey)
	}

	if id := strings.TrimSpace(a.ID); id != "" {
		return "id:" + id
	}

	return ""
}

// EnsureIndex returns a stable index derived from the auth file name or credential identity.
func (a *Auth) EnsureIndex() string {
	if a == nil {
		return ""
	}
	if a.indexAssigned && a.Index != "" {
		return a.Index
	}

	seed := a.indexSeed()
	if seed == "" {
		return ""
	}

	idx := stableAuthIndex(seed)
	a.Index = idx
	a.indexAssigned = true
	return idx
}

// Clone duplicates a model state including nested error details.
func (m *ModelState) Clone() *ModelState {
	if m == nil {
		return nil
	}
	copyState := *m
	if m.LastError != nil {
		copyState.LastError = &Error{
			Code:       m.LastError.Code,
			Message:    m.LastError.Message,
			Retryable:  m.LastError.Retryable,
			HTTPStatus: m.LastError.HTTPStatus,
		}
	}
	return &copyState
}

func (a *Auth) ProxyInfo() string {
	if a == nil {
		return ""
	}
	proxyStr := strings.TrimSpace(a.ProxyURL)
	if proxyStr == "" {
		return ""
	}
	if idx := strings.Index(proxyStr, "://"); idx > 0 {
		return "via " + proxyStr[:idx] + " proxy"
	}
	return "via proxy"
}

// DisableCoolingOverride returns the auth scoped disable_cooling override when present.
// The value is read from metadata key "disable_cooling" (or legacy "disable-cooling").
//
// NOTE: This override is intentionally "true-only". When the metadata value is false, it is treated
// as "not set" so the global disable-cooling flag can still take effect.
func (a *Auth) DisableCoolingOverride() (bool, bool) {
	if a == nil || a.Metadata == nil {
		return false, false
	}
	if val, ok := a.Metadata["disable_cooling"]; ok {
		if parsed, okParse := parseBoolAny(val); okParse {
			if !parsed {
				return false, false
			}
			return parsed, true
		}
	}
	if val, ok := a.Metadata["disable-cooling"]; ok {
		if parsed, okParse := parseBoolAny(val); okParse {
			if !parsed {
				return false, false
			}
			return parsed, true
		}
	}
	return false, false
}

// RefreshDisabled reports whether credential refresh is explicitly disabled for this auth.
// It is intended for access-token-only test/migration records where using a refresh token
// would conflict with another runtime holding the same account.
func (a *Auth) RefreshDisabled() bool {
	if a == nil {
		return false
	}
	if refreshDisabledFromMetadata(a.Metadata) {
		return true
	}
	if len(a.Attributes) > 0 {
		for _, key := range []string{"refresh_disabled", "disable_refresh", "auto_refresh_disabled"} {
			if parsed, ok := parseBoolAny(a.Attributes[key]); ok && parsed {
				return true
			}
		}
		for _, key := range []string{"refresh_enabled", "auto_refresh", "auto_refresh_enabled"} {
			if parsed, ok := parseBoolAny(a.Attributes[key]); ok && !parsed {
				return true
			}
		}
	}
	return false
}

// SubscriptionPlanType returns the best-known subscription plan recorded on an
// auth entry. It accepts both canonical metadata (plan_type) and nested provider
// profile/quota shapes so every model capability gate reads the same signal.
func (a *Auth) SubscriptionPlanType() string {
	if a == nil {
		return ""
	}
	if plan := subscriptionPlanTypeFromMetadata(a.Metadata); plan != "" {
		return plan
	}
	if len(a.Attributes) > 0 {
		for _, key := range []string{"plan_type", "planType", "subscription_tier", "subscriptionTier", "chatgpt_plan_type", "chatgptPlanType"} {
			if plan := normalizeSubscriptionPlanSignal(a.Attributes[key]); plan != "" {
				return plan
			}
		}
	}
	return ""
}

func subscriptionPlanTypeFromMetadata(meta map[string]any) string {
	if len(meta) == 0 {
		return ""
	}
	for _, key := range []string{"plan_type", "planType", "subscription_tier", "subscriptionTier", "chatgpt_plan_type", "chatgptPlanType"} {
		if plan := normalizeSubscriptionPlanSignal(meta[key]); plan != "" {
			return plan
		}
	}
	if hasTruthyMetadataKeyDeep(meta, "has_claude_max", "hasClaudeMax", "has_max", "hasMax", "is_max", "isMax", "max") {
		return "max"
	}
	if hasTruthyMetadataKeyDeep(meta, "has_claude_pro", "hasClaudePro", "has_pro", "hasPro", "is_pro", "isPro", "pro") {
		return "pro"
	}
	for key, value := range meta {
		if metadataKeyMayContainPlanType(key) {
			if plan := normalizeSubscriptionPlanSignal(value); plan != "" {
				return plan
			}
		}
		if nested, ok := metadataObject(value); ok {
			if plan := subscriptionPlanTypeFromMetadata(nested); plan != "" {
				return plan
			}
		}
		if list, ok := value.([]any); ok {
			for _, item := range list {
				if nested, ok := metadataObject(item); ok {
					if plan := subscriptionPlanTypeFromMetadata(nested); plan != "" {
						return plan
					}
				}
			}
		}
	}
	return ""
}

func metadataKeyMayContainPlanType(key string) bool {
	normalized := strings.ToLower(strings.TrimSpace(key))
	for _, marker := range []string{"plan", "tier", "subscription", "account_type", "entitlement"} {
		if strings.Contains(normalized, marker) {
			return true
		}
	}
	return false
}

func normalizeSubscriptionPlanSignal(raw any) string {
	switch value := raw.(type) {
	case string:
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			return ""
		}
		normalized := strings.ToLower(strings.NewReplacer("_", "-", " ", "-").Replace(trimmed))
		for _, marker := range []string{"max", "pro", "plus", "team", "business", "enterprise", "free", "go"} {
			if strings.Contains(normalized, marker) {
				return trimmed
			}
		}
		return ""
	case json.Number:
		return ""
	default:
		return ""
	}
}

func hasTruthyMetadataKeyDeep(meta map[string]any, keys ...string) bool {
	if hasTruthyMetadataKey(meta, keys...) {
		return true
	}
	for _, value := range meta {
		if nested, ok := metadataObject(value); ok {
			if hasTruthyMetadataKeyDeep(nested, keys...) {
				return true
			}
			continue
		}
		if list, ok := value.([]any); ok {
			for _, item := range list {
				if nested, ok := metadataObject(item); ok && hasTruthyMetadataKeyDeep(nested, keys...) {
					return true
				}
			}
		}
	}
	return false
}

func hasTruthyMetadataKey(meta map[string]any, keys ...string) bool {
	if len(meta) == 0 {
		return false
	}
	wanted := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		wanted[compactMetadataKey(key)] = struct{}{}
	}
	for key, value := range meta {
		if _, ok := wanted[compactMetadataKey(key)]; ok {
			if parsed, okParse := parseBoolAny(value); okParse && parsed {
				return true
			}
		}
	}
	return false
}

func compactMetadataKey(key string) string {
	return strings.ToLower(strings.NewReplacer("_", "", "-", "").Replace(strings.TrimSpace(key)))
}

func refreshDisabledFromMetadata(meta map[string]any) bool {
	if len(meta) == 0 {
		return false
	}
	if isReauthRequiredMetadata(meta) {
		return true
	}
	for _, key := range []string{"refresh_disabled", "disable_refresh", "auto_refresh_disabled"} {
		if parsed, ok := parseBoolAny(meta[key]); ok && parsed {
			return true
		}
	}
	for _, key := range []string{"refresh_enabled", "auto_refresh", "auto_refresh_enabled"} {
		if parsed, ok := parseBoolAny(meta[key]); ok && !parsed {
			return true
		}
	}
	if settings, ok := metadataObject(meta["account_settings"]); ok {
		for _, key := range []string{"refresh_disabled", "disable_refresh", "auto_refresh_disabled"} {
			if parsed, okParse := parseBoolAny(settings[key]); okParse && parsed {
				return true
			}
		}
		for _, key := range []string{"refresh_enabled", "auto_refresh", "auto_refresh_enabled"} {
			if parsed, okParse := parseBoolAny(settings[key]); okParse && !parsed {
				return true
			}
		}
	}
	return false
}

const refreshReauthRequiredMessage = "refresh token was already used; sign in again to reconnect this account"

// refreshReauthRequiredGenericMessage is the sanitized message persisted when a
// refresh token is rejected as invalid/expired/revoked (e.g. provider returns
// OAuth invalid_grant) rather than specifically reused. It never echoes the raw
// provider body so tokens cannot leak into auth files or the management UI.
const refreshReauthRequiredGenericMessage = "refresh token is no longer valid; sign in again to reconnect this account"

func isReauthRequiredMetadata(meta map[string]any) bool {
	if len(meta) == 0 {
		return false
	}
	if parsed, ok := parseBoolAny(meta["reauth_required"]); ok && parsed {
		return true
	}
	for _, key := range []string{"refresh_status", "refresh_error_code", "refresh_disabled_reason"} {
		if value, ok := meta[key].(string); ok && strings.EqualFold(strings.TrimSpace(value), "reauth_required") {
			return true
		}
	}
	return false
}

// IsReauthRequiredMetadata reports whether the supplied metadata carries the
// terminal reauth-required lock written by markRefreshReauthRequiredWithReason
// (reauth_required / refresh_status / refresh_error_code / refresh_disabled_reason
// == "reauth_required"). It is exported so callers outside this package (e.g.
// the management API's re-auth save path) can distinguish an automatic
// refresh-failure lock from an operator's explicit account_settings.refresh_enabled
// = false, which does not set any of these keys.
func IsReauthRequiredMetadata(meta map[string]any) bool {
	return isReauthRequiredMetadata(meta)
}

func isRefreshTokenReuseError(err error) bool {
	if err == nil {
		return false
	}
	raw := strings.ToLower(err.Error())
	if raw == "" {
		return false
	}
	normalized := strings.NewReplacer("-", "_", " ", "_").Replace(raw)
	if strings.Contains(normalized, "refresh_token_reused") || strings.Contains(normalized, "refresh_token_reuse") || strings.Contains(normalized, "refresh_token_already_used") {
		return true
	}
	return strings.Contains(normalized, "refresh") && strings.Contains(normalized, "already") && (strings.Contains(normalized, "used") || strings.Contains(normalized, "reuse"))
}

// IsRefreshTokenReuseError reports whether a provider refresh error means the
// refresh token has already been rotated and this credential needs reauth.
func IsRefreshTokenReuseError(err error) bool {
	return isRefreshTokenReuseError(err)
}

// terminalRefreshAuthError reports whether a provider refresh error is terminal,
// meaning the refresh token can no longer be exchanged and the credential needs
// the operator to authenticate again. It returns a fixed, sanitized error code
// for diagnostics (never the raw provider body, which may embed token material).
//
// Rationale (no Anthropic/Claude public docs exist for refresh-token rotation):
//   - RFC 6749 §5.2 defines OAuth invalid_grant for a refresh request as the
//     refresh token being invalid / expired / revoked / reused. None of these
//     are recoverable by retrying the same token.
//   - RFC 9700 §4.14 recommends refresh-token rotation with replay detection,
//     where re-submitting a rotated-out token can revoke the whole token family.
//     So retrying a terminal token is not just useless, it can make recovery
//     strictly harder.
//
// This is only ever called on a refresh-call error, so a bare invalid_grant is
// itself terminal. The reuse signatures handled by isRefreshTokenReuseError are
// a subset and keep their dedicated code for backward compatibility.
func terminalRefreshAuthError(err error) (string, bool) {
	if err == nil {
		return "", false
	}
	if isRefreshTokenReuseError(err) {
		return "refresh_token_reused", true
	}
	raw := strings.ToLower(err.Error())
	if raw == "" {
		return "", false
	}
	normalized := strings.NewReplacer("-", "_", " ", "_").Replace(raw)
	if strings.Contains(normalized, "invalid_grant") {
		return "invalid_grant", true
	}
	return "", false
}

// IsTerminalRefreshAuthError reports whether a provider refresh error is
// terminal and the credential must be re-authenticated rather than retried.
func IsTerminalRefreshAuthError(err error) bool {
	_, terminal := terminalRefreshAuthError(err)
	return terminal
}

// Terminal refresh failure classification labels used for structured
// diagnostics (see #164). These are derived purely from the already-computed
// terminalRefreshAuthError code, never from a fresh signal, so they stay
// consistent with the persisted refresh_error_code metadata.
const (
	// classConcurrentReuseRace marks a refresh_token_reused terminal failure:
	// per RFC 9700 §4.14 this is the rotation-replay/race signal for
	// single-use refresh tokens (e.g. two processes refreshing the same
	// credential concurrently, or a retried request after rotation).
	classConcurrentReuseRace = "concurrent_reuse_race"
	// classExpiredOrRevokedGeneric marks a bare invalid_grant terminal
	// failure not recognized as a reuse signature. RFC 6749 §5.2 defines
	// invalid_grant ambiguously (expired/revoked/invalid); no Anthropic
	// public doc distinguishes them, so this label intentionally does not
	// claim more precision than the upstream response provides.
	classExpiredOrRevokedGeneric = "expired_or_revoked_generic"
	// classUnknownTerminal is a defensive fallback for any future terminal
	// code that isn't one of the two recognized above.
	classUnknownTerminal = "unknown_terminal"
)

// classifyTerminalRefreshFailure derives a coarse, human-legible diagnostic
// label from the sanitized terminal refresh error code already computed by
// terminalRefreshAuthError. It never inspects the raw error string itself so
// it cannot introduce a new, undocumented distinction.
func classifyTerminalRefreshFailure(code string) string {
	switch code {
	case "refresh_token_reused":
		return classConcurrentReuseRace
	case "invalid_grant":
		return classExpiredOrRevokedGeneric
	default:
		return classUnknownTerminal
	}
}

// credentialFingerprint returns a short, irreversible fingerprint for a
// credential secret (e.g. a refresh token) so diagnostic logs and alerts can
// correlate the same physical credential across restarts/instances without
// ever exposing the plaintext value. Returns "" for an empty secret.
//
// The returned digest is intentionally truncated (first 16 hex chars of a
// SHA-256 sum, i.e. 64 bits) — enough to correlate occurrences of the same
// token without materially aiding an offline guess of the original secret.
func credentialFingerprint(secret string) string {
	if secret == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])[:16]
}

// refreshTokenFingerprintFromMetadata extracts a credentialFingerprint for
// the refresh token stored in auth metadata, if any. It only ever reads the
// value to hash it; the plaintext token is never returned or logged.
func refreshTokenFingerprintFromMetadata(meta map[string]any) string {
	if len(meta) == 0 {
		return ""
	}
	if token, ok := meta["refresh_token"].(string); ok {
		return credentialFingerprint(token)
	}
	return ""
}

var (
	processInstanceIDOnce sync.Once
	processInstanceIDVal  string
)

// processInstanceID returns a cheap, stable-for-the-process-lifetime
// identifier (hostname:pid) used to distinguish which running instance
// observed a diagnostic event when multiple processes share the same auth
// store. It intentionally avoids introducing new persisted state.
func processInstanceID() string {
	processInstanceIDOnce.Do(func() {
		host, err := os.Hostname()
		if err != nil || host == "" {
			host = "unknown-host"
		}
		processInstanceIDVal = host + ":" + strconv.Itoa(os.Getpid())
	})
	return processInstanceIDVal
}

// anthropicReauthEndpointPath is the existing management API route that
// generates a fresh Anthropic OAuth authorization URL scoped to a single
// existing Claude auth record (see internal/api/handlers/management ->
// RequestAnthropicToken, registered at GET /v0/management/anthropic-auth-url).
// It is kept as a relative path (no host/scheme) because this package has no
// knowledge of the operator-facing external host/port the management API is
// actually served on (only the management package computes that, via
// managementCallbackURL); the operator (or the caller of this log line) is
// expected to prefix it with the reachable management base URL.
const anthropicReauthEndpointPath = "/v0/management/anthropic-auth-url"

// reauthAlertURL builds the copy-pasteable relative path (path+query only,
// see anthropicReauthEndpointPath) for the existing "generate a fresh
// Anthropic OAuth authorization URL for this exact auth record" management
// endpoint. It is only meaningful for the "claude" provider today (the only
// provider with a name-scoped anthropic-auth-url route); callers should not
// emit it for other providers. The id is URL-escaped since auth IDs may
// contain characters (spaces, punctuation from an operator label) that are
// not valid in a raw query string.
func reauthAlertURL(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return ""
	}
	return anthropicReauthEndpointPath + "?auth_name=" + url.QueryEscape(id)
}

// ReauthAlertURL is the exported form of reauthAlertURL so callers outside
// this package (e.g. the management API's auth-file listing) can surface the
// same relative reauth-URL-generation endpoint path without duplicating the
// query-escaping/endpoint-path logic. See reauthAlertURL for details.
func ReauthAlertURL(id string) string {
	return reauthAlertURL(id)
}

// reauthMessageForCode returns the sanitized, user-facing message persisted for
// a given terminal refresh error code.
func reauthMessageForCode(code string) string {
	if code == "refresh_token_reused" {
		return refreshReauthRequiredMessage
	}
	return refreshReauthRequiredGenericMessage
}

func (a *Auth) markRefreshReauthRequired(now time.Time) {
	a.markRefreshReauthRequiredWithReason(now, "refresh_token_reused")
}

// markRefreshReauthRequiredWithReason records the terminal reauth-required state
// using the supplied sanitized error code (e.g. "refresh_token_reused" or
// "invalid_grant"). The persisted message is derived from the code and never
// contains the raw provider body.
func (a *Auth) markRefreshReauthRequiredWithReason(now time.Time, code string) {
	if a == nil {
		return
	}
	if now.IsZero() {
		now = time.Now()
	}
	if strings.TrimSpace(code) == "" {
		code = "refresh_token_reused"
	}
	message := reauthMessageForCode(code)
	if a.Metadata == nil {
		a.Metadata = make(map[string]any)
	}
	a.Metadata["refresh_disabled"] = true
	a.Metadata["refresh_status"] = "reauth_required"
	a.Metadata["refresh_error_code"] = code
	a.Metadata["refresh_disabled_reason"] = "reauth_required"
	a.Metadata["reauth_required"] = true
	a.Metadata["refresh_disabled_at"] = now.UTC().Format(time.RFC3339)
	a.Metadata["last_refresh_error"] = message
	a.NextRefreshAfter = time.Time{}
	a.Status = StatusError
	a.StatusMessage = "reauth_required"
	a.LastError = &Error{
		Code:       "reauth_required",
		Message:    message,
		Retryable:  false,
		HTTPStatus: http.StatusUnauthorized,
	}
	a.UpdatedAt = now
}

// MarkRefreshReauthRequired persists the terminal state for a credential whose
// refresh token was already used by another refresh flow.
func (a *Auth) MarkRefreshReauthRequired(now time.Time) {
	a.markRefreshReauthRequired(now)
}

// tokenOwnedMetadataKeys are metadata fields owned by the OAuth/token lifecycle.
// They must only move forward (a successful refresh or re-auth), never be rolled
// back by a stale clone that carried older token state while writing unrelated
// runtime metadata (e.g. quota status, managed headers).
var tokenOwnedMetadataKeys = map[string]struct{}{
	"access_token":      {},
	"refresh_token":     {},
	"id_token":          {},
	"token":             {},
	"expired":           {},
	"expires_at":        {},
	"oauth_expires_at":  {},
	"expires_in":        {},
	"last_refresh":      {},
	"last_refreshed_at": {},
	"timestamp":         {},
}

// tokenOwnedFreshness returns the most recent timestamp that describes when this
// credential's token-owned state was last issued/updated. A successful refresh
// or re-auth advances it (newer expiry / last_refresh); a stale clone keeps an
// older value. It is used to detect and reject token rollbacks.
func tokenOwnedFreshness(a *Auth) time.Time {
	if a == nil {
		return time.Time{}
	}
	latest := a.LastRefreshedAt
	if exp, ok := a.ExpirationTime(); ok && exp.After(latest) {
		latest = exp
	}
	if a.Metadata != nil {
		for _, key := range []string{"last_refresh", "last_refreshed_at", "timestamp"} {
			if t, ok := parseTimeValue(a.Metadata[key]); ok && t.After(latest) {
				latest = t
			}
		}
	}
	return latest
}

// hasTokenMaterial reports whether the record actually carries OAuth token
// material (access/refresh/id token). It gates the rollback guard so that
// resets, disables, or delete -> re-add flows that intentionally drop tokens are
// never treated as stale write-backs.
func hasTokenMaterial(a *Auth) bool {
	if a == nil || a.Metadata == nil {
		return false
	}
	for _, key := range []string{"refresh_token", "access_token", "id_token", "token"} {
		if v, ok := a.Metadata[key].(string); ok && strings.TrimSpace(v) != "" {
			return true
		}
	}
	return false
}

// preserveNewerTokenOwnedFields guards against a stale write-back rolling token
// state backwards. It only acts when BOTH the existing in-memory record and the
// incoming update carry token material and the existing one is strictly newer
// (e.g. a refresh landed between the caller cloning the auth and calling Update
// with unrelated metadata changes). In that case the incoming record's
// token-owned fields are replaced with the existing ones, while non-token
// metadata (quota / header / status) is left untouched and still applies.
//
// When the incoming record is same-or-newer (a real refresh or re-auth), or when
// either side has no token material (reset / disable / re-add), this is a no-op
// and the update proceeds unchanged.
func preserveNewerTokenOwnedFields(incoming, existing *Auth) bool {
	if incoming == nil || existing == nil {
		return false
	}
	if !hasTokenMaterial(existing) || !hasTokenMaterial(incoming) {
		return false
	}
	if !tokenOwnedFreshness(existing).After(tokenOwnedFreshness(incoming)) {
		return false
	}
	if incoming.Metadata == nil {
		incoming.Metadata = make(map[string]any, len(tokenOwnedMetadataKeys))
	}
	for key := range tokenOwnedMetadataKeys {
		if value, ok := existing.Metadata[key]; ok {
			incoming.Metadata[key] = value
		} else {
			delete(incoming.Metadata, key)
		}
	}
	if existing.LastRefreshedAt.After(incoming.LastRefreshedAt) {
		incoming.LastRefreshedAt = existing.LastRefreshedAt
	}
	return true
}

func metadataObject(raw any) (map[string]any, bool) {
	if raw == nil {
		return nil, false
	}
	switch value := raw.(type) {
	case map[string]any:
		return value, len(value) > 0
	case map[string]string:
		out := make(map[string]any, len(value))
		for key, val := range value {
			out[key] = val
		}
		return out, len(out) > 0
	default:
		data, err := json.Marshal(raw)
		if err != nil {
			return nil, false
		}
		var out map[string]any
		if err = json.Unmarshal(data, &out); err != nil {
			return nil, false
		}
		return out, len(out) > 0
	}
}

// ToolPrefixDisabled returns whether the proxy_ tool name prefix should be
// skipped for this auth. When true, tool names are sent to Anthropic unchanged.
// The value is read from metadata key "tool_prefix_disabled" (or "tool-prefix-disabled").
func (a *Auth) ToolPrefixDisabled() bool {
	if a == nil || a.Metadata == nil {
		return false
	}
	for _, key := range []string{"tool_prefix_disabled", "tool-prefix-disabled"} {
		if val, ok := a.Metadata[key]; ok {
			if parsed, okParse := parseBoolAny(val); okParse {
				return parsed
			}
		}
	}
	return false
}

// RequestRetryOverride returns the auth-file scoped request_retry override when present.
// The value is read from metadata key "request_retry" (or legacy "request-retry").
func (a *Auth) RequestRetryOverride() (int, bool) {
	if a == nil || a.Metadata == nil {
		return 0, false
	}
	if val, ok := a.Metadata["request_retry"]; ok {
		if parsed, okParse := parseIntAny(val); okParse {
			if parsed < 0 {
				parsed = 0
			}
			return parsed, true
		}
	}
	if val, ok := a.Metadata["request-retry"]; ok {
		if parsed, okParse := parseIntAny(val); okParse {
			if parsed < 0 {
				parsed = 0
			}
			return parsed, true
		}
	}
	return 0, false
}

func parseBoolAny(val any) (bool, bool) {
	switch typed := val.(type) {
	case bool:
		return typed, true
	case string:
		trimmed := strings.TrimSpace(typed)
		if trimmed == "" {
			return false, false
		}
		parsed, err := strconv.ParseBool(trimmed)
		if err != nil {
			return false, false
		}
		return parsed, true
	case float64:
		return typed != 0, true
	case json.Number:
		parsed, err := typed.Int64()
		if err != nil {
			return false, false
		}
		return parsed != 0, true
	default:
		return false, false
	}
}

func parseIntAny(val any) (int, bool) {
	switch typed := val.(type) {
	case int:
		return typed, true
	case int32:
		return int(typed), true
	case int64:
		return int(typed), true
	case float64:
		return int(typed), true
	case json.Number:
		parsed, err := typed.Int64()
		if err != nil {
			return 0, false
		}
		return int(parsed), true
	case string:
		trimmed := strings.TrimSpace(typed)
		if trimmed == "" {
			return 0, false
		}
		parsed, err := strconv.Atoi(trimmed)
		if err != nil {
			return 0, false
		}
		return parsed, true
	default:
		return 0, false
	}
}

func (a *Auth) AccountInfo() (string, string) {
	if a == nil {
		return "", ""
	}
	// For Gemini CLI, include project ID in the OAuth account info if present.
	if strings.ToLower(a.Provider) == "gemini-cli" {
		if a.Metadata != nil {
			email, _ := a.Metadata["email"].(string)
			email = strings.TrimSpace(email)
			if email != "" {
				if p, ok := a.Metadata["project_id"].(string); ok {
					p = strings.TrimSpace(p)
					if p != "" {
						return "oauth", email + " (" + p + ")"
					}
				}
				return "oauth", email
			}
		}
	}

	// For GitHub provider (including github-copilot), return username
	if strings.HasPrefix(strings.ToLower(a.Provider), "github") {
		if a.Metadata != nil {
			if username, ok := a.Metadata["username"].(string); ok {
				username = strings.TrimSpace(username)
				if username != "" {
					return "oauth", username
				}
			}
		}
	}

	// Check metadata for email first (OAuth-style auth)
	if a.Metadata != nil {
		if method, ok := a.Metadata["auth_method"].(string); ok {
			switch strings.ToLower(strings.TrimSpace(method)) {
			case "oauth":
				for _, key := range []string{"email", "username", "name"} {
					if value, okValue := a.Metadata[key].(string); okValue {
						if trimmed := strings.TrimSpace(value); trimmed != "" {
							return "oauth", trimmed
						}
					}
				}
			case "pat", "personal_access_token":
				for _, key := range []string{"username", "email", "name", "token_preview"} {
					if value, okValue := a.Metadata[key].(string); okValue {
						if trimmed := strings.TrimSpace(value); trimmed != "" {
							return "personal_access_token", trimmed
						}
					}
				}
				return "personal_access_token", ""
			}
		}
		if v, ok := a.Metadata["email"].(string); ok {
			email := strings.TrimSpace(v)
			if email != "" {
				return "oauth", email
			}
		}
	}
	// Fall back to API key (API-key auth)
	if a.Attributes != nil {
		if v := a.Attributes["api_key"]; v != "" {
			return "api_key", v
		}
	}
	return "", ""
}

// ExpirationTime attempts to extract the credential expiration timestamp from metadata.
// It inspects common keys such as "expired", "expire", "expires_at", and also
// nested "token" objects to remain compatible with legacy auth file formats.
func (a *Auth) ExpirationTime() (time.Time, bool) {
	if a == nil {
		return time.Time{}, false
	}
	if ts, ok := expirationFromMap(a.Metadata); ok {
		return ts, true
	}
	return time.Time{}, false
}

var (
	refreshLeadMu        sync.RWMutex
	refreshLeadFactories = make(map[string]func() *time.Duration)
)

func RegisterRefreshLeadProvider(provider string, factory func() *time.Duration) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" || factory == nil {
		return
	}
	refreshLeadMu.Lock()
	refreshLeadFactories[provider] = factory
	refreshLeadMu.Unlock()
}

var expireKeys = [...]string{"expired", "expire", "expires_at", "expiresAt", "expiry", "expires"}

func expirationFromMap(meta map[string]any) (time.Time, bool) {
	if meta == nil {
		return time.Time{}, false
	}
	for _, key := range expireKeys {
		if v, ok := meta[key]; ok {
			if ts, ok1 := parseTimeValue(v); ok1 {
				return ts, true
			}
		}
	}
	for _, nestedKey := range []string{"token", "Token"} {
		if nested, ok := meta[nestedKey]; ok {
			switch val := nested.(type) {
			case map[string]any:
				if ts, ok1 := expirationFromMap(val); ok1 {
					return ts, true
				}
			case map[string]string:
				temp := make(map[string]any, len(val))
				for k, v := range val {
					temp[k] = v
				}
				if ts, ok1 := expirationFromMap(temp); ok1 {
					return ts, true
				}
			}
		}
	}
	return time.Time{}, false
}

func ProviderRefreshLead(provider string, runtime any) *time.Duration {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if runtime != nil {
		if eval, ok := runtime.(interface{ RefreshLead() *time.Duration }); ok {
			if lead := eval.RefreshLead(); lead != nil && *lead > 0 {
				return lead
			}
		}
	}
	refreshLeadMu.RLock()
	factory := refreshLeadFactories[provider]
	refreshLeadMu.RUnlock()
	if factory == nil {
		return nil
	}
	if lead := factory(); lead != nil && *lead > 0 {
		return lead
	}
	return nil
}

func parseTimeValue(v any) (time.Time, bool) {
	switch value := v.(type) {
	case string:
		s := strings.TrimSpace(value)
		if s == "" {
			return time.Time{}, false
		}
		layouts := []string{
			time.RFC3339,
			time.RFC3339Nano,
			"2006-01-02 15:04:05",
			"2006-01-02 15:04",
			"2006-01-02T15:04:05Z07:00",
		}
		for _, layout := range layouts {
			if ts, err := time.Parse(layout, s); err == nil {
				return ts, true
			}
		}
		if unix, err := strconv.ParseInt(s, 10, 64); err == nil {
			return normaliseUnix(unix), true
		}
	case float64:
		return normaliseUnix(int64(value)), true
	case int64:
		return normaliseUnix(value), true
	case json.Number:
		if i, err := value.Int64(); err == nil {
			return normaliseUnix(i), true
		}
		if f, err := value.Float64(); err == nil {
			return normaliseUnix(int64(f)), true
		}
	}
	return time.Time{}, false
}

func normaliseUnix(raw int64) time.Time {
	if raw <= 0 {
		return time.Time{}
	}
	// Heuristic: treat values with millisecond precision (>1e12) accordingly.
	if raw > 1_000_000_000_000 {
		return time.UnixMilli(raw)
	}
	return time.Unix(raw, 0)
}
