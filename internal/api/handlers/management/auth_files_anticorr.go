package management

// This file is fork-owned. It carries the fork's anti-correlation state machine
// (managed-header projection, runtime-identity tracking, reauth locks, account
// settings) plus fork-only management endpoints and OAuth providers that the
// upstream per-concern split of auth_files.go does not contain. Upstream owns
// auth_files.go (slim) and auth_files_{crud,fields,oauth_callback,provider_oauth}.go;
// shared helpers live there. Keep additions here to preserve them across future
// upstream syncs.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/claude"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/copilot"
	geminiAuth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/gemini"
	gitlabauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/gitlab"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/kilo"
	kiroauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/kiro"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/misc"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	runtimehelps "github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/proxyutil"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// Fork-only constants. Shared ports (anthropicCallbackPort, codexCallbackPort) are
// defined by upstream in auth_files_provider_oauth.go; only the fork-only ones live here.
const (
	anthropicOAuthExchangeTimeout = 2 * time.Minute
	anthropicOAuthExchangeRetries = 3
	geminiCallbackPort            = 8085
	geminiCLIEndpoint             = "https://cloudcode-pa.googleapis.com"
	geminiCLIVersion              = "v1internal"
	gitLabLoginModeOAuth          = "oauth"
	gitLabLoginModePAT            = "pat"
	accountSettingsSchemaVersion  = 1
)

var errAuthRefreshDisabled = errors.New("credential refresh is disabled for this account")

// gcpProjectList / gcpProject / gcpProjectLabels mirror the Google Cloud project-list
// response consumed by the fork Gemini CLI onboarding flow. Upstream v7.2.101 deleted
// interfaces.GCPProject when it dropped Gemini CLI support; these fork-local copies keep
// fetchGCPProjects / onboardAllGeminiProjects working without reintroducing a cross-package
// dependency on a type upstream no longer ships.
type gcpProjectList struct {
	Projects []gcpProject `json:"projects"`
}

type gcpProject struct {
	ProjectNumber  string           `json:"projectNumber"`
	ProjectID      string           `json:"projectId"`
	LifecycleState string           `json:"lifecycleState"`
	Name           string           `json:"name"`
	Labels         gcpProjectLabels `json:"labels"`
	CreateTime     time.Time        `json:"createTime"`
}

type gcpProjectLabels struct {
	GenerativeLanguage string `json:"generative-language"`
}

type authFileManagedHeaderProjection struct {
	GeneratedAt           string            `json:"generated_at,omitempty"`
	Source                string            `json:"source,omitempty"`
	SourceURL             string            `json:"source_url,omitempty"`
	CheckedAt             string            `json:"checked_at,omitempty"`
	Completeness          string            `json:"completeness,omitempty"`
	VariantPolicy         string            `json:"variant_policy,omitempty"`
	VersionVariant        string            `json:"version_variant,omitempty"`
	BrandOrderVariant     string            `json:"brand_order_variant,omitempty"`
	SummaryHeaders        map[string]string `json:"summary_headers,omitempty"`
	VersionedCapabilities map[string]string `json:"versioned_capabilities,omitempty"`
	StableIdentity        map[string]string `json:"stable_identity,omitempty"`
	RuntimeFingerprint    map[string]string `json:"runtime_fingerprint,omitempty"`
}

type authFileManagedHeaderHistoryEntry struct {
	RecordedAt                    string            `json:"recorded_at,omitempty"`
	PolicyVersion                 string            `json:"policy_version,omitempty"`
	Reason                        string            `json:"reason,omitempty"`
	ChangedFields                 []string          `json:"changed_fields,omitempty"`
	PreviousSource                string            `json:"previous_source,omitempty"`
	PreviousSourceURL             string            `json:"previous_source_url,omitempty"`
	NextSource                    string            `json:"next_source,omitempty"`
	NextSourceURL                 string            `json:"next_source_url,omitempty"`
	PreviousSummaryHeaders        map[string]string `json:"previous_summary_headers,omitempty"`
	NextSummaryHeaders            map[string]string `json:"next_summary_headers,omitempty"`
	PreviousVersionedCapabilities map[string]string `json:"previous_versioned_capabilities,omitempty"`
	NextVersionedCapabilities     map[string]string `json:"next_versioned_capabilities,omitempty"`
	PreviousStableIdentity        map[string]string `json:"previous_stable_identity,omitempty"`
	NextStableIdentity            map[string]string `json:"next_stable_identity,omitempty"`
	PreviousRuntimeFingerprint    map[string]string `json:"previous_runtime_fingerprint,omitempty"`
	NextRuntimeFingerprint        map[string]string `json:"next_runtime_fingerprint,omitempty"`
}

type authFileManagedHeaderState struct {
	PolicyVersion string                              `json:"policy_version,omitempty"`
	Current       *authFileManagedHeaderProjection    `json:"current,omitempty"`
	History       []authFileManagedHeaderHistoryEntry `json:"history,omitempty"`
}

type authFileRuntimeIdentitySnapshot struct {
	IdentityID       string            `json:"identity_id,omitempty"`
	Provider         string            `json:"provider,omitempty"`
	PolicyVersion    string            `json:"policy_version,omitempty"`
	Source           string            `json:"source,omitempty"`
	Revision         int               `json:"revision,omitempty"`
	CreatedAt        string            `json:"created_at,omitempty"`
	UpdatedAt        string            `json:"updated_at,omitempty"`
	SeedHash         string            `json:"seed_hash,omitempty"`
	AuthIDHash       string            `json:"auth_id_hash,omitempty"`
	AccountHash      string            `json:"account_hash,omitempty"`
	BaseURLHost      string            `json:"base_url_host,omitempty"`
	ProxyHash        string            `json:"proxy_hash,omitempty"`
	ProfileID        string            `json:"profile_id,omitempty"`
	TLSProfileID     string            `json:"tls_profile_id,omitempty"`
	Family           string            `json:"family,omitempty"`
	TLSFamily        string            `json:"tls_family,omitempty"`
	CoreManaged      bool              `json:"core_managed"`
	RuntimeEnforced  bool              `json:"runtime_enforced"`
	StableIdentity   map[string]string `json:"stable_identity,omitempty"`
	RuntimeSemantics map[string]string `json:"runtime_semantics,omitempty"`
}

type authFileRuntimeIdentityHistoryEntry struct {
	RecordedAt    string                           `json:"recorded_at,omitempty"`
	Reason        string                           `json:"reason,omitempty"`
	ChangedFields []string                         `json:"changed_fields,omitempty"`
	Previous      *authFileRuntimeIdentitySnapshot `json:"previous,omitempty"`
	Next          *authFileRuntimeIdentitySnapshot `json:"next,omitempty"`
}

type authFileRuntimeIdentityState struct {
	PolicyVersion string                                `json:"policy_version,omitempty"`
	Current       *authFileRuntimeIdentitySnapshot      `json:"current,omitempty"`
	History       []authFileRuntimeIdentityHistoryEntry `json:"history,omitempty"`
}

type authFileAccountSettingsStored struct {
	SchemaVersion         int               `json:"schema_version"`
	ManagedHeaderSeedHash string            `json:"managed_header_seed_hash,omitempty"`
	ExtraHeaders          map[string]string `json:"extra_headers,omitempty"`
	RefreshEnabled        *bool             `json:"refresh_enabled,omitempty"`
	// FarmEnrolled mirrors the account-level device-farm enrollment flag
	// (telemetry-device-farm TR1). coreauth.AuthFarmEnrolled(auth), which
	// reads Metadata[coreauth.FarmEnrolledMetadataKey] directly, remains the
	// single source of truth; this pointer only lets accountSettingsFarmEnrolled
	// echo an explicit PATCH intent (see farmEnrolledStorageValue) the same
	// way RefreshEnabled does for refresh_enabled.
	FarmEnrolled         *bool                         `json:"farm_enrolled,omitempty"`
	TransportProfile     any                           `json:"transport_profile,omitempty"`
	TLSProfile           any                           `json:"tls_profile,omitempty"`
	ManagedHeaderState   *authFileManagedHeaderState   `json:"managed_header_state,omitempty"`
	RuntimeIdentityState *authFileRuntimeIdentityState `json:"runtime_identity_state,omitempty"`
}

type authFileAccountSettingsView struct {
	ProxyURL       string            `json:"proxy_url"`
	Note           string            `json:"note"`
	Disabled       bool              `json:"disabled"`
	ManagedHeaders map[string]string `json:"managed_headers"`
	ExtraHeaders   map[string]string `json:"extra_headers"`
	RefreshEnabled bool              `json:"refresh_enabled"`
	// Fast reports whether the Codex priority/fast Responses websocket flow is
	// currently enabled for this account (a non-empty fast_models allowlist,
	// including "*"). It is only meaningful for codex accounts; non-codex
	// accounts always report false. Mirrors executor.codexFastEnabled's
	// "is fast configured at all" determination so the management card badge
	// matches the runtime gate.
	Fast               bool                                          `json:"fast"`
	TransportProfile   any                                           `json:"transport_profile"`
	TLSProfile         any                                           `json:"tls_profile"`
	RuntimeProfile     *runtimehelps.RuntimeTransportProfile         `json:"runtime_profile,omitempty"`
	RuntimeIdentity    *authFileRuntimeIdentityState                 `json:"runtime_identity,omitempty"`
	ManagedHeaderState *authFileManagedHeaderState                   `json:"managed_header_state,omitempty"`
	ClientObservations []runtimehelps.ClaudeDeviceProfileObservation `json:"client_version_observations,omitempty"`
	// SyntheticDeviceID is a read-only, masked representation of the per-account
	// synthetic device_id derived by ① (helps.SyntheticDeviceID). Only the first 16
	// hex characters are exposed, followed by an ellipsis, so that callers can
	// compare stability across requests without recovering the full derived value.
	// It is never persisted and PATCH requests must not include this field.
	SyntheticDeviceID string `json:"synthetic_device_id,omitempty"`
	// DeviceIDSource and FarmBound project the farm telemetry contract
	// (P2 "咬合"): DeviceIDSource is one of container_synced / synthetic / drift /
	// unknown, and FarmBound reports whether the account is bound to a real
	// container device_id. Both are derived from coreauth.ClaudeDeviceIDSource,
	// which reuses the exact supply-atomicity gate predicate so FarmBound==true is
	// precisely the set the gate would allow. Additive, read-only fields; PATCH
	// must not include them. Non-Claude accounts report unknown / false.
	DeviceIDSource string `json:"device_id_source"`
	FarmBound      bool   `json:"farm_bound"`
	// FarmEnrolled is the writable device-farm enrollment intent
	// (telemetry-device-farm TR1, coreauth.AuthFarmEnrolled), distinct from
	// the read-only FarmBound above: an account can be enrolled but not yet
	// bound to a container (pending provisioning), or bound while enrollment
	// metadata is absent on a legacy record. PATCH may set this field.
	FarmEnrolled bool                              `json:"farm_enrolled"`
	Activation   authFileAccountSettingsActivation `json:"activation"`
	Warnings     []string                          `json:"warnings"`
}

type authFileAccountSettingsActivation struct {
	Summary   string `json:"summary"`
	State     string `json:"state,omitempty"`
	Source    string `json:"source,omitempty"`
	Effective bool   `json:"effective"`
}

type authFileAccountSettingsResponse struct {
	Name            string                      `json:"name"`
	AccountSettings authFileAccountSettingsView `json:"account_settings"`
}

func (h *Handler) findAuthByNameOrID(name string) *coreauth.Auth {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	if h != nil && h.authManager != nil {
		if auth, ok := h.authManager.GetByID(name); ok && auth != nil {
			return auth
		}
		for _, auth := range h.authManager.List() {
			if auth == nil {
				continue
			}
			if strings.TrimSpace(auth.FileName) == name {
				return auth
			}
			if filepath.Base(strings.TrimSpace(authAttribute(auth, "path"))) == name {
				return auth
			}
		}
	}
	if h == nil || h.cfg == nil || isUnsafeAuthFileName(name) {
		return nil
	}
	path := filepath.Join(h.cfg.AuthDir, filepath.Base(name))
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	auth, err := h.buildAuthFromFileData(path, data)
	if err != nil {
		return nil
	}
	return auth
}

// MERGE-REVIEW: fork-only helper now unreferenced after adopting upstream's
// PatchAuthFileFields (which uses applyAuthFileHeadersPatch). Kept to avoid
// dropping fork behavior silently; safe to delete if confirmed unused.
func replaceAuthMetadataHeaders(auth *coreauth.Auth, headers map[string]string) {
	if auth == nil {
		return
	}
	existingHeaders := coreauth.ExtractCustomHeadersFromMetadata(auth.Metadata)
	nextHeaders := make(map[string]string, len(existingHeaders))
	for key, value := range existingHeaders {
		nextHeaders[key] = value
	}

	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}

	for rawKey, rawValue := range headers {
		key := strings.TrimSpace(rawKey)
		if key == "" {
			continue
		}
		value := strings.TrimSpace(rawValue)
		attrKey := "header:" + key
		if value == "" {
			delete(nextHeaders, key)
			delete(auth.Attributes, attrKey)
			continue
		}
		nextHeaders[key] = value
		auth.Attributes[attrKey] = value
	}

	if len(nextHeaders) == 0 {
		delete(auth.Metadata, "headers")
		return
	}

	metaHeaders := make(map[string]any, len(nextHeaders))
	for key, value := range nextHeaders {
		metaHeaders[key] = value
	}
	auth.Metadata["headers"] = metaHeaders
}

func overwriteAuthMetadataHeaders(auth *coreauth.Auth, headers map[string]string) {
	if auth == nil {
		return
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	for key := range auth.Attributes {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(key)), "header:") {
			delete(auth.Attributes, key)
		}
	}

	normalized := normalizeHeaderMap(headers)
	if len(normalized) == 0 {
		delete(auth.Metadata, "headers")
		return
	}

	metaHeaders := make(map[string]any, len(normalized))
	for key, value := range normalized {
		metaHeaders[key] = value
		auth.Attributes["header:"+key] = value
	}
	auth.Metadata["headers"] = metaHeaders
}

func normalizeHeaderMap(headers map[string]string) map[string]string {
	if len(headers) == 0 {
		return nil
	}
	normalized := make(map[string]string)
	for rawKey, rawValue := range headers {
		key := strings.TrimSpace(rawKey)
		value := strings.TrimSpace(rawValue)
		if key == "" || value == "" {
			continue
		}
		normalized[key] = value
	}
	if len(normalized) == 0 {
		return nil
	}
	return normalized
}

func authProxyURL(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if trimmed := strings.TrimSpace(auth.ProxyURL); trimmed != "" {
		return trimmed
	}
	if auth.Metadata == nil {
		return ""
	}
	if rawProxyURL, ok := auth.Metadata["proxy_url"].(string); ok {
		return strings.TrimSpace(rawProxyURL)
	}
	return ""
}

func authDisplayName(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if trimmed := strings.TrimSpace(auth.FileName); trimmed != "" {
		return trimmed
	}
	if trimmed := strings.TrimSpace(auth.ID); trimmed != "" {
		return trimmed
	}
	if path := strings.TrimSpace(authAttribute(auth, "path")); path != "" {
		if base := strings.TrimSpace(filepath.Base(path)); base != "" && base != "." {
			return base
		}
	}
	return strings.TrimSpace(auth.Label)
}

func authNote(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if trimmed := strings.TrimSpace(authAttribute(auth, "note")); trimmed != "" {
		return trimmed
	}
	if auth.Metadata == nil {
		return ""
	}
	if rawNote, ok := auth.Metadata["note"].(string); ok {
		return strings.TrimSpace(rawNote)
	}
	return ""
}

func normalizeAccountProfileValue(raw any) any {
	switch value := raw.(type) {
	case nil:
		return nil
	case string:
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			return nil
		}
		return trimmed
	case map[string]any:
		if len(value) == 0 {
			return nil
		}
		return value
	case map[string]string:
		if len(value) == 0 {
			return nil
		}
		out := make(map[string]any, len(value))
		for key, item := range value {
			if trimmedKey := strings.TrimSpace(key); trimmedKey != "" {
				out[trimmedKey] = strings.TrimSpace(item)
			}
		}
		if len(out) == 0 {
			return nil
		}
		return out
	default:
		return nil
	}
}

func authManagedHeaderNames(provider string) map[string]struct{} {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "claude":
		return map[string]struct{}{
			"user-agent":                  {},
			"x-app":                       {},
			"x-stainless-package-version": {},
			"x-stainless-runtime-version": {},
			"x-stainless-timeout":         {},
		}
	case "codex":
		return map[string]struct{}{
			"user-agent":            {},
			"version":               {},
			"originator":            {},
			"x-codex-beta-features": {},
		}
	default:
		return nil
	}
}

func authHeaderReservedForExtras(provider string, headerName string) bool {
	lowerName := strings.ToLower(strings.TrimSpace(headerName))
	if lowerName == "" {
		return false
	}
	if managedNames := authManagedHeaderNames(provider); managedNames != nil {
		if _, ok := managedNames[lowerName]; ok {
			return true
		}
	}
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "claude":
		switch lowerName {
		case "authorization", "x-api-key", "anthropic-version", "anthropic-beta", "content-type",
			"accept", "accept-encoding", "connection", "x-client-request-id",
			"x-claude-code-session-id", "x-stainless-retry-count", "x-stainless-runtime",
			"x-stainless-lang", "x-stainless-os", "x-stainless-arch",
			"anthropic-dangerous-direct-browser-access":
			return true
		}
	case "codex":
		switch lowerName {
		case "authorization", "openai-beta", "originator", "session_id", "chatgpt-account-id",
			"x-client-request-id", "x-codex-turn-state", "x-codex-turn-metadata",
			"x-responsesapi-include-timing-metrics", "content-type", "accept", "connection":
			return true
		}
		if strings.HasPrefix(lowerName, "x-stainless-") {
			return true
		}
		if strings.HasPrefix(lowerName, "x-codex-") && lowerName != "x-codex-beta-features" {
			return true
		}
	}
	return false
}

func managedHeadersForAuth(auth *coreauth.Auth, cfg *config.Config) map[string]string {
	return managedHeaderProjectionForAuth(auth, cfg).SummaryHeaders
}

func managedHeaderProjectionForAuth(auth *coreauth.Auth, cfg *config.Config) authFileManagedHeaderProjection {
	if auth == nil {
		return authFileManagedHeaderProjection{}
	}
	generatedAt := time.Now().UTC().Format(time.RFC3339)
	var projection authFileManagedHeaderProjection
	switch strings.ToLower(strings.TrimSpace(auth.Provider)) {
	case "claude":
		profile := runtimehelps.ResolveClaudeDeviceProfile(auth, "", nil, cfg)
		timeoutValue := "600"
		if cfg != nil {
			if trimmed := strings.TrimSpace(cfg.ClaudeHeaderDefaults.Timeout); trimmed != "" {
				timeoutValue = trimmed
			}
		}
		projection = authFileManagedHeaderProjection{
			GeneratedAt: generatedAt,
			SummaryHeaders: normalizeHeaderMap(map[string]string{
				"User-Agent":                  profile.UserAgent,
				"X-App":                       "cli",
				"X-Stainless-Package-Version": profile.PackageVersion,
				"X-Stainless-Runtime-Version": profile.RuntimeVersion,
				"X-Stainless-Timeout":         timeoutValue,
			}),
			VersionedCapabilities: normalizeHeaderMap(map[string]string{
				"User-Agent":                  profile.UserAgent,
				"X-Stainless-Package-Version": profile.PackageVersion,
				"X-Stainless-Runtime-Version": profile.RuntimeVersion,
				"X-Stainless-Timeout":         timeoutValue,
			}),
			StableIdentity: normalizeHeaderMap(map[string]string{
				"X-App": "cli",
			}),
			RuntimeFingerprint: normalizeHeaderMap(map[string]string{
				"X-Stainless-Os":   profile.OS,
				"X-Stainless-Arch": profile.Arch,
			}),
			Source:       profile.Source.Source,
			SourceURL:    profile.Source.SourceURL,
			CheckedAt:    profile.Source.CheckedAt,
			Completeness: profile.Source.Completeness,
		}
	case "codex":
		profile := runtimehelps.ResolveCodexClientProfile(auth, nil, cfg)
		projection = authFileManagedHeaderProjection{
			GeneratedAt:           generatedAt,
			SummaryHeaders:        runtimehelps.CodexManagedHeaders(profile),
			VersionedCapabilities: runtimehelps.CodexManagedVersionedCapabilities(profile),
			StableIdentity:        runtimehelps.CodexManagedStableIdentity(profile),
			RuntimeFingerprint:    runtimehelps.CodexManagedRuntimeFingerprint(profile),
			Source:                profile.Source.Source,
			SourceURL:             profile.Source.SourceURL,
			CheckedAt:             profile.Source.CheckedAt,
			Completeness:          profile.Source.Completeness,
		}
	default:
		return authFileManagedHeaderProjection{}
	}
	return personalizeManagedHeaderProjectionForAuth(auth, projection)
}

func mergeAccountHeaders(managedHeaders map[string]string, extraHeaders map[string]string) map[string]string {
	merged := make(map[string]string, len(managedHeaders)+len(extraHeaders))
	for key, value := range normalizeHeaderMap(managedHeaders) {
		merged[key] = value
	}
	for key, value := range normalizeHeaderMap(extraHeaders) {
		if _, exists := merged[key]; exists {
			continue
		}
		merged[key] = value
	}
	if len(merged) == 0 {
		return nil
	}
	return merged
}

func readAccountSettingsMetadata(auth *coreauth.Auth, cfg *config.Config) authFileAccountSettingsStored {
	stored := authFileAccountSettingsStored{
		SchemaVersion: accountSettingsSchemaVersion,
	}
	if auth == nil || auth.Metadata == nil {
		stored.ExtraHeaders = legacyExtraHeaders(auth)
		return stored
	}
	rawSettings, ok := auth.Metadata["account_settings"]
	if !ok || rawSettings == nil {
		stored.ExtraHeaders = legacyExtraHeaders(auth)
		return stored
	}

	if data, errMarshal := json.Marshal(rawSettings); errMarshal == nil {
		_ = json.Unmarshal(data, &stored)
	}
	if stored.SchemaVersion == 0 {
		stored.SchemaVersion = accountSettingsSchemaVersion
	}
	stored.ExtraHeaders = normalizeHeaderMap(stored.ExtraHeaders)
	stored.TransportProfile = normalizeAccountProfileValue(stored.TransportProfile)
	stored.TLSProfile = normalizeAccountProfileValue(stored.TLSProfile)
	stored.ManagedHeaderState = normalizeManagedHeaderState(stored.ManagedHeaderState)
	stored.RuntimeIdentityState = normalizeRuntimeIdentityState(stored.RuntimeIdentityState)
	return stored
}

func legacyExtraHeaders(auth *coreauth.Auth) map[string]string {
	if auth == nil {
		return nil
	}
	allHeaders := normalizeHeaderMap(coreauth.ExtractCustomHeadersFromMetadata(auth.Metadata))
	if len(allHeaders) == 0 {
		return nil
	}
	managedNames := authManagedHeaderNames(auth.Provider)
	extraHeaders := make(map[string]string)
	for key, value := range allHeaders {
		lowerKey := strings.ToLower(strings.TrimSpace(key))
		if _, ok := managedNames[lowerKey]; ok {
			continue
		}
		if authHeaderReservedForExtras(auth.Provider, key) {
			continue
		}
		extraHeaders[key] = value
	}
	return normalizeHeaderMap(extraHeaders)
}

// accountSettingsFastEnabled reports whether Codex fast (priority Responses
// websocket transport) is currently enabled for this account. It is a read-only
// view derived from the same sources executor.codexFastEnabled consumes: the
// synthesizer-written Attributes["fast_models"] (config API-key codex auths) and
// the OAuth token-json top-level fast_models flattened into
// Metadata["fast_models"] by the file store loader. A non-empty allowlist
// (including "*") means fast is enabled. Fast is codex-only; every other provider
// reports false so the management card badge never claims fast for accounts where
// the runtime gate can never fire.
func accountSettingsFastEnabled(auth *coreauth.Auth) bool {
	if auth == nil || providerKey(auth) != "codex" {
		return false
	}
	if len(auth.Attributes) > 0 && strings.TrimSpace(auth.Attributes["fast_models"]) != "" {
		return true
	}
	if len(auth.Metadata) == 0 {
		return false
	}
	// Mirror codexFastEnabled's accepted metadata shapes. We only ever write the
	// string scalar form ("*"/""), but tolerate the legacy []string/[]interface{}
	// shapes defensively so a hand-authored account still reads correctly.
	switch typed := auth.Metadata["fast_models"].(type) {
	case string:
		return strings.TrimSpace(typed) != ""
	case []string:
		for _, entry := range typed {
			if strings.TrimSpace(entry) != "" {
				return true
			}
		}
	case []any:
		for _, entry := range typed {
			if str, ok := entry.(string); ok && strings.TrimSpace(str) != "" {
				return true
			}
		}
	}
	return false
}

// applyAuthFastModelsMetadata writes the codex fast toggle to the account's
// top-level fast_models metadata key (round-tripped to the account JSON top level
// by FileTokenStore.Save, the exact location executor.codexFastEnabled and the
// config synthesizer read). Enabling writes the string scalar "*" (all models);
// disabling writes the empty string "" rather than deleting the key, matching the
// "disabled" convention already used elsewhere.
//
// Critical: the value MUST be a JSON string scalar. codexFastEnabled's type switch
// only recognizes string / []string; a JSON array would decode to []interface{}
// and be silently treated as fast=false, so callers must never write an array here.
func applyAuthFastModelsMetadata(auth *coreauth.Auth, enabled bool) {
	if auth == nil {
		return
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	if enabled {
		auth.Metadata["fast_models"] = "*"
		return
	}
	auth.Metadata["fast_models"] = ""
}

func accountSettingsRefreshEnabled(auth *coreauth.Auth, stored authFileAccountSettingsStored) bool {
	if stored.RefreshEnabled != nil {
		return *stored.RefreshEnabled
	}
	if auth != nil && auth.RefreshDisabled() {
		return false
	}
	return true
}

// accountSettingsFarmEnrolled echoes the persisted account_settings.farm_enrolled
// intent (telemetry-device-farm TR1). The account_settings blob only stores an
// explicit prior PATCH value (see farmEnrolledStorageValue); when it has never
// been set through this endpoint, fall back to the actual single source of
// truth, coreauth.AuthFarmEnrolled(auth), which reads
// Metadata[coreauth.FarmEnrolledMetadataKey] directly. This mirrors
// accountSettingsRefreshEnabled falling back to auth.RefreshDisabled().
func accountSettingsFarmEnrolled(auth *coreauth.Auth, stored authFileAccountSettingsStored) bool {
	if stored.FarmEnrolled != nil {
		return *stored.FarmEnrolled
	}
	return coreauth.AuthFarmEnrolled(auth)
}

// refreshEnabledStorageValue always returns a non-nil pointer so the stored
// account_settings.refresh_enabled faithfully echoes the user's explicit intent.
//
// Returning nil for enabled=true would drop the stored value (omitempty) and force
// accountSettingsRefreshEnabled to fall back to RefreshDisabled() inference. When a
// record carries reauth-required locks (e.g. refresh_status=reauth_required), that
// inference would keep reporting refresh_enabled=false even after the user manually
// re-enabled refresh, so the toggle would not persist on GET/view. Persisting the
// explicit true is safe: account_settings.refresh_enabled=true is checked as !parsed
// in refreshDisabledFromMetadata, so it does not itself trigger RefreshDisabled.
func refreshEnabledStorageValue(enabled bool) *bool {
	value := enabled
	return &value
}

// farmEnrolledStorageValue always returns a non-nil pointer, for the same
// round-trip-fidelity reason as refreshEnabledStorageValue: persisting the
// explicit intent (rather than nil-for-the-default-false, which omitempty
// would drop) means accountSettingsFarmEnrolled echoes exactly what the
// operator last set on GET/view, instead of re-deriving it from
// coreauth.AuthFarmEnrolled every time.
func farmEnrolledStorageValue(enrolled bool) *bool {
	value := enrolled
	return &value
}

func applyAuthRefreshEnabledMetadata(auth *coreauth.Auth, enabled bool) {
	if auth == nil {
		return
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	if enabled {
		delete(auth.Metadata, "refresh_disabled")
		delete(auth.Metadata, "disable_refresh")
		delete(auth.Metadata, "auto_refresh_disabled")
		delete(auth.Metadata, "refresh_enabled")
		// Re-enabling refresh must also release any terminal reauth-required lock
		// written by markRefreshReauthRequiredWithReason. Otherwise refresh gating
		// keeps skipping this account and the management UI keeps showing it as
		// reauth-required even though the user explicitly opted back in. If the
		// underlying token is truly dead, the next refresh attempt will re-write
		// the lock, which is the correct feedback path.
		clearAuthReauthRequiredLock(auth)
		return
	}
	auth.Metadata["refresh_disabled"] = true
	auth.Metadata["refresh_enabled"] = false
	auth.NextRefreshAfter = time.Time{}
}

// applyAuthFarmEnrolledMetadata writes/clears the single-source-of-truth
// account-level farm enrollment flag on Metadata[coreauth.FarmEnrolledMetadataKey]
// (telemetry-device-farm TR1, coreauth.AuthFarmEnrolled). Unlike
// applyAuthRefreshEnabledMetadata there is no independent lock to release
// here: enrolled writes the key true, and not-enrolled deletes it so
// coreauth.AuthFarmEnrolled falls back to its documented false default
// instead of persisting a redundant explicit-false marker.
func applyAuthFarmEnrolledMetadata(auth *coreauth.Auth, enrolled bool) {
	if auth == nil {
		return
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	if !enrolled {
		delete(auth.Metadata, coreauth.FarmEnrolledMetadataKey)
		return
	}
	auth.Metadata[coreauth.FarmEnrolledMetadataKey] = true
}

// clearAuthReauthRequiredLock releases the terminal reauth-required state that
// markRefreshReauthRequiredWithReason (sdk/cliproxy/auth/types.go) writes when a
// refresh token is rejected. It removes the metadata lock keys, normalizes any
// refresh_disabled-style attributes that would still make RefreshDisabled() true,
// and resets the runtime status fields only when they specifically represent a
// reauth_required state. Non-reauth states (e.g. disabled-via-management) are left
// untouched so this stays scoped to "user re-enabled refresh on a reauth account".
func clearAuthReauthRequiredLock(auth *coreauth.Auth) {
	if auth == nil {
		return
	}
	if auth.Metadata != nil {
		for _, key := range []string{
			"reauth_required",
			"refresh_status",
			"refresh_error_code",
			"refresh_disabled_reason",
		} {
			delete(auth.Metadata, key)
		}
	}
	if len(auth.Attributes) > 0 {
		for _, key := range []string{"refresh_disabled", "disable_refresh", "auto_refresh_disabled"} {
			delete(auth.Attributes, key)
		}
		// Only flip attribute values that already exist; do not introduce new keys.
		for _, key := range []string{"refresh_enabled", "auto_refresh", "auto_refresh_enabled"} {
			if _, ok := auth.Attributes[key]; ok {
				auth.Attributes[key] = "true"
			}
		}
	}
	if auth.StatusMessage == "reauth_required" {
		auth.StatusMessage = ""
		if auth.Status == coreauth.StatusError {
			auth.Status = coreauth.StatusActive
		}
		// ApplyReauthRequiredStateFromMetadata (sdk/cliproxy/auth/types.go) sets
		// Unavailable=true when it restores this lock on load, so releasing the
		// lock must clear it back on the live record too. A subsequent reload
		// already recomputes Unavailable from the (now-cleared) metadata, but
		// this keeps the in-memory record from lingering as "unavailable" while
		// its Status reads active between the re-auth and the next reload.
		auth.Unavailable = false
		if auth.LastError != nil && auth.LastError.Code == "reauth_required" {
			auth.LastError = nil
		}
	}
}

// clearStaleReauthLockOnSave releases an automatic reauth-required lock that
// re-auth (management "reauth" action) or an OAuth callback is about to
// persist on record. markRefreshReauthRequiredWithReason
// (sdk/cliproxy/auth/types.go) writes reauth_required / refresh_status /
// refresh_error_code / refresh_disabled_reason / refresh_disabled /
// refresh_disabled_at / last_refresh_error onto a credential the first time a
// refresh is rejected as terminal. Before this fix, a completed reauth /
// OAuth callback still carried that lock forward:
//   - mergeUserDefinedAuthMetadataInto treats refresh_disabled as an
//     operator-controlled key and copies it from the previous record when the
//     new record does not already set it, and every other lock key
//     (reauth_required, refresh_status, refresh_error_code,
//     refresh_disabled_reason, refresh_disabled_at, last_refresh_error) falls
//     through its generic "not token/runtime owned -> inherit as custom
//     operator data" branch.
//   - cloneClaudeReauthMetadata (Claude's own reauth path) only strips
//     token-owned keys, so the same lock keys and the record's
//     StatusMessage ("reauth_required") survive into the freshly-built
//     record even before the merge above runs.
//
// The credential therefore kept RefreshDisabled() == true after a successful
// reauth, forcing an operator to also flip the management UI toggle by hand
// (two-step recovery). This helper runs on the final record right before
// store.Save so it always sees the fully merged metadata, and only clears the
// lock when the record still carries the automatic markers
// (coreauth.IsReauthRequiredMetadata). An operator's explicit
// account_settings.refresh_enabled = false never sets those markers, so that
// case is left untouched and recovery stays a no-op for it.
func clearStaleReauthLockOnSave(record *coreauth.Auth) {
	if record == nil || !coreauth.IsReauthRequiredMetadata(record.Metadata) {
		return
	}
	clearAuthReauthRequiredLock(record)
	// clearAuthReauthRequiredLock intentionally only clears the lock keys it
	// shares with the account-settings "re-enable refresh" path. The reauth
	// save path also owns two additional breadcrumb keys that
	// markRefreshReauthRequiredWithReason writes and that account-settings
	// never touches, plus the refresh_disabled boolean itself (account-settings
	// clears that one via applyAuthRefreshEnabledMetadata before calling
	// clearAuthReauthRequiredLock; the reauth path must do the same here).
	delete(record.Metadata, "refresh_disabled")
	delete(record.Metadata, "refresh_disabled_at")
	delete(record.Metadata, "last_refresh_error")
}

func buildAuthFileAccountSettingsView(auth *coreauth.Auth, cfg *config.Config) authFileAccountSettingsView {
	stored := readAccountSettingsMetadata(auth, cfg)
	projection := managedHeaderProjectionForAuth(auth, cfg)
	managedHeaders := projection.SummaryHeaders
	extraHeaders := normalizeHeaderMap(stored.ExtraHeaders)
	refreshEnabled := accountSettingsRefreshEnabled(auth, stored)
	var managedHeaderState *authFileManagedHeaderState
	if coreauth.HasStructuredAccountSettingsMetadata(auth) || stored.ManagedHeaderState != nil {
		managedHeaderState = mergeManagedHeaderState(stored.ManagedHeaderState, projection, providerKey(auth))
	}
	warnings := make([]string, 0, 3)
	runtimeProfile := runtimehelps.ResolveRuntimeTransportProfile(auth)
	runtimeIdentity := mergeRuntimeIdentityState(stored.RuntimeIdentityState, auth, cfg, runtimeProfile)
	transportRuntimeEnforced := runtimeProfile != nil && runtimeProfile.SupportsTransportRuntime()
	tlsRuntimeEnforced := runtimeProfile != nil && runtimeProfile.SupportsTLSRuntime()
	if !refreshEnabled {
		warnings = append(warnings, "refresh_enabled is false: core will not use refresh tokens for this account; use access-token-only records only for short-lived testing or controlled migration")
	}
	if stored.TransportProfile != nil && !transportRuntimeEnforced {
		warnings = append(warnings, "transport_profile is not runtime-enforced for this provider/preset; core default transport remains active unless an explicit supported preset is selected")
	} else if stored.TransportProfile != nil && strings.EqualFold(providerKey(auth), "codex") {
		warnings = append(warnings, "codex transport_profile is runtime-enforced for account-scoped transport isolation only; it does not emulate the official Codex rustls TLS fingerprint yet")
	}
	if stored.TLSProfile != nil && !tlsRuntimeEnforced {
		warnings = append(warnings, "tls_profile preset is not supported by this provider/runtime yet and is not enforced")
	} else if stored.TLSProfile != nil && strings.EqualFold(providerKey(auth), "claude") {
		if runtimeProfile != nil && strings.EqualFold(runtimeProfile.TLSProfileID, "claude_reqwest_rustls_compatible_v1") {
			warnings = append(warnings, "claude tls_profile uses the Claude reqwest/rustls-compatible community profile and is runtime-enforced via Go approximation; exact Rust reqwest/rustls wire parity requires the future Rust sidecar")
		} else {
			warnings = append(warnings, "claude tls_profile is runtime-enforced with project-managed Chrome-like uTLS ClientHello presets for Anthropic API hosts; this is advanced explicit opt-in, not the default Claude Code CLI fingerprint or provider-edge parity claim")
		}
	} else if stored.TLSProfile != nil && strings.EqualFold(providerKey(auth), "codex") {
		warnings = append(warnings, "codex tls_profile is runtime-enforced only for Go transport knobs such as account-scoped pooling, ALPN, and HTTP/1.1 forcing; it is not the Codex Desktop rustls native transport yet")
	}
	var clientObservations []runtimehelps.ClaudeDeviceProfileObservation
	if strings.EqualFold(providerKey(auth), "claude") {
		clientObservations = runtimehelps.ClaudeDeviceProfileObservations(auth, "")
	}

	// Derive the masked synthetic device_id for read-only display (Epic B/P2.B1).
	// The full 64-hex value is intentionally not exposed; only the first 16 hex
	// characters followed by an ellipsis are returned so observers can confirm
	// per-account stability without reconstructing the full derived value.
	var syntheticDeviceIDMasked string
	if auth != nil {
		authDir := ""
		if cfg != nil {
			authDir = cfg.AuthDir
		}
		full := runtimehelps.SyntheticDeviceID(authDir, auth, "")
		if len(full) >= 16 {
			syntheticDeviceIDMasked = full[:16] + "…"
		} else if full != "" {
			syntheticDeviceIDMasked = full + "…"
		}
	}

	// Farm telemetry contract (P2): classify the account's device_id provenance
	// and farm-bound state. Derivation is centralized in coreauth.ClaudeDeviceIDSource
	// (single source of truth, reuses the supply-atomicity gate predicate) rather
	// than recomputed here. Non-Claude accounts return unknown / false.
	deviceIDSource, farmBound := coreauth.ClaudeDeviceIDSource(auth)
	farmEnrolled := accountSettingsFarmEnrolled(auth, stored)

	return authFileAccountSettingsView{
		ProxyURL:           authProxyURL(auth),
		Note:               authNote(auth),
		Disabled:           auth != nil && auth.Disabled,
		ManagedHeaders:     managedHeaders,
		ExtraHeaders:       extraHeaders,
		RefreshEnabled:     refreshEnabled,
		Fast:               accountSettingsFastEnabled(auth),
		TransportProfile:   stored.TransportProfile,
		TLSProfile:         stored.TLSProfile,
		RuntimeProfile:     runtimeProfile,
		RuntimeIdentity:    runtimeIdentity,
		ManagedHeaderState: managedHeaderState,
		ClientObservations: clientObservations,
		SyntheticDeviceID:  syntheticDeviceIDMasked,
		DeviceIDSource:     deviceIDSource,
		FarmBound:          farmBound,
		FarmEnrolled:       farmEnrolled,
		Activation: authFileAccountSettingsActivation{
			Summary:   accountSettingsActivationSummary(auth, managedHeaders, extraHeaders, refreshEnabled, transportRuntimeEnforced, tlsRuntimeEnforced),
			State:     accountSettingsActivationState(auth, stored.TransportProfile, stored.TLSProfile, refreshEnabled, transportRuntimeEnforced, tlsRuntimeEnforced),
			Source:    "core",
			Effective: auth != nil && !auth.Disabled,
		},
		Warnings: warnings,
	}
}

func providerKey(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(auth.Provider))
}

func (h *Handler) syncAuthManagedHeaderState(ctx context.Context, auth *coreauth.Auth) *coreauth.Auth {
	if h == nil || auth == nil {
		return auth
	}
	ensureManagedHeaderSeedForOAuthCredential(auth)
	stored := readAccountSettingsMetadata(auth, h.cfg)
	projection := managedHeaderProjectionForAuth(auth, h.cfg)
	var nextState *authFileManagedHeaderState
	if len(projection.SummaryHeaders) > 0 ||
		len(projection.VersionedCapabilities) > 0 ||
		len(projection.StableIdentity) > 0 ||
		len(projection.RuntimeFingerprint) > 0 {
		nextState = mergeManagedHeaderState(stored.ManagedHeaderState, projection, providerKey(auth))
	} else {
		nextState = normalizeManagedHeaderState(stored.ManagedHeaderState)
	}
	runtimeProfile := runtimehelps.ResolveRuntimeTransportProfile(auth)
	nextRuntimeIdentity := mergeRuntimeIdentityState(stored.RuntimeIdentityState, auth, h.cfg, runtimeProfile)
	extraHeaders := normalizeHeaderMap(stored.ExtraHeaders)
	runtimeHeaders := mergeAccountHeaders(projection.SummaryHeaders, extraHeaders)
	currentHeaders := normalizeHeaderMap(coreauth.ExtractCustomHeadersFromMetadata(auth.Metadata))
	shouldSyncHeaders := len(projection.SummaryHeaders) > 0 || len(extraHeaders) > 0
	headersEquivalent := true
	if shouldSyncHeaders {
		headersEquivalent = reflect.DeepEqual(runtimeHeaders, currentHeaders)
	}
	if managedHeaderStateEquivalent(nextState, normalizeManagedHeaderState(stored.ManagedHeaderState)) &&
		runtimeIdentityStateEquivalent(nextRuntimeIdentity, normalizeRuntimeIdentityState(stored.RuntimeIdentityState)) &&
		headersEquivalent {
		return auth
	}

	updated := auth.Clone()
	if updated.Metadata == nil {
		updated.Metadata = make(map[string]any)
	}
	if updated.Attributes == nil {
		updated.Attributes = make(map[string]string)
	}
	stored.ManagedHeaderState = nextState
	stored.RuntimeIdentityState = nextRuntimeIdentity
	stored.ManagedHeaderSeedHash = accountManagedHeaderSeedHash(updated)
	updated.Metadata["account_settings"] = stored
	if shouldSyncHeaders {
		overwriteAuthMetadataHeaders(updated, runtimeHeaders)
	}
	updated.UpdatedAt = time.Now()
	if h.authManager == nil {
		return updated
	}
	persisted, errUpdate := h.authManager.Update(ctx, updated)
	if errUpdate != nil {
		log.WithError(errUpdate).WithField("auth_id", updated.ID).Warn("failed to persist managed header state sync")
		return updated
	}
	if persisted != nil {
		return persisted
	}
	return updated
}

func ensureManagedHeaderSeedForOAuthCredential(auth *coreauth.Auth) {
	if auth == nil || auth.Metadata == nil {
		return
	}
	switch providerKey(auth) {
	case "codex", "claude":
	default:
		return
	}
	if strings.TrimSpace(metadataString(auth.Metadata, "managed_header_seed")) != "" {
		return
	}
	for _, key := range []string{"refresh_token", "access_token", "id_token", "token"} {
		if !isEmptyMetadataValue(auth.Metadata[key]) {
			ensureManagedHeaderSeed(auth)
			return
		}
	}
}

func normalizeRuntimeIdentityState(state *authFileRuntimeIdentityState) *authFileRuntimeIdentityState {
	if state == nil {
		return nil
	}
	normalized := &authFileRuntimeIdentityState{
		PolicyVersion: strings.TrimSpace(state.PolicyVersion),
		Current:       normalizeRuntimeIdentitySnapshot(state.Current),
	}
	for _, entry := range state.History {
		normalizedEntry := authFileRuntimeIdentityHistoryEntry{
			RecordedAt:    strings.TrimSpace(entry.RecordedAt),
			Reason:        strings.TrimSpace(entry.Reason),
			ChangedFields: append([]string(nil), entry.ChangedFields...),
			Previous:      normalizeRuntimeIdentitySnapshot(entry.Previous),
			Next:          normalizeRuntimeIdentitySnapshot(entry.Next),
		}
		sort.Strings(normalizedEntry.ChangedFields)
		normalized.History = append(normalized.History, normalizedEntry)
	}
	if normalized.PolicyVersion == "" && normalized.Current == nil && len(normalized.History) == 0 {
		return nil
	}
	if normalized.PolicyVersion == "" {
		normalized.PolicyVersion = runtimeIdentityPolicyVersion("")
	}
	return normalized
}

func normalizeRuntimeIdentitySnapshot(snapshot *authFileRuntimeIdentitySnapshot) *authFileRuntimeIdentitySnapshot {
	if snapshot == nil {
		return nil
	}
	normalized := *snapshot
	normalized.IdentityID = strings.TrimSpace(normalized.IdentityID)
	normalized.Provider = strings.ToLower(strings.TrimSpace(normalized.Provider))
	normalized.PolicyVersion = strings.TrimSpace(normalized.PolicyVersion)
	normalized.Source = strings.TrimSpace(normalized.Source)
	normalized.CreatedAt = strings.TrimSpace(normalized.CreatedAt)
	normalized.UpdatedAt = strings.TrimSpace(normalized.UpdatedAt)
	normalized.SeedHash = strings.TrimSpace(normalized.SeedHash)
	normalized.AuthIDHash = strings.TrimSpace(normalized.AuthIDHash)
	normalized.AccountHash = strings.TrimSpace(normalized.AccountHash)
	normalized.BaseURLHost = strings.TrimSpace(normalized.BaseURLHost)
	normalized.ProxyHash = strings.TrimSpace(normalized.ProxyHash)
	normalized.ProfileID = strings.TrimSpace(normalized.ProfileID)
	normalized.TLSProfileID = strings.TrimSpace(normalized.TLSProfileID)
	normalized.Family = strings.TrimSpace(normalized.Family)
	normalized.TLSFamily = strings.TrimSpace(normalized.TLSFamily)
	normalized.StableIdentity = normalizeHeaderMap(normalized.StableIdentity)
	normalized.RuntimeSemantics = normalizeHeaderMap(normalized.RuntimeSemantics)
	return &normalized
}

func mergeRuntimeIdentityState(previous *authFileRuntimeIdentityState, auth *coreauth.Auth, cfg *config.Config, profile *runtimehelps.RuntimeTransportProfile) *authFileRuntimeIdentityState {
	if auth == nil || profile == nil || !profile.SupportsRuntime() {
		return normalizeRuntimeIdentityState(previous)
	}
	normalizedPrev := normalizeRuntimeIdentityState(previous)
	now := time.Now().UTC().Format(time.RFC3339)
	next := buildRuntimeIdentitySnapshot(auth, cfg, profile, normalizedPrev, now)
	if next == nil {
		return normalizedPrev
	}
	if normalizedPrev != nil && runtimeIdentitySnapshotEquivalentForRevision(normalizedPrev.Current, next) {
		return normalizedPrev
	}

	state := &authFileRuntimeIdentityState{
		PolicyVersion: runtimeIdentityPolicyVersion(profile.Provider),
		Current:       next,
	}
	if normalizedPrev == nil {
		return state
	}
	if state.PolicyVersion == "" {
		state.PolicyVersion = normalizedPrev.PolicyVersion
	}
	state.History = append(state.History, normalizedPrev.History...)
	if normalizedPrev.Current != nil {
		state.History = append(state.History, authFileRuntimeIdentityHistoryEntry{
			RecordedAt:    next.UpdatedAt,
			Reason:        "runtime-identity-refresh",
			ChangedFields: diffRuntimeIdentitySnapshotFields(normalizedPrev.Current, next),
			Previous:      normalizedPrev.Current,
			Next:          next,
		})
	}
	if len(state.History) > 12 {
		state.History = append([]authFileRuntimeIdentityHistoryEntry(nil), state.History[len(state.History)-12:]...)
	}
	return state
}

func buildRuntimeIdentitySnapshot(auth *coreauth.Auth, cfg *config.Config, profile *runtimehelps.RuntimeTransportProfile, previous *authFileRuntimeIdentityState, now string) *authFileRuntimeIdentitySnapshot {
	if auth == nil || profile == nil || !profile.SupportsRuntime() {
		return nil
	}
	provider := strings.ToLower(strings.TrimSpace(profile.Provider))
	authID := runtimeIdentityAuthID(auth)
	accountKey := runtimeIdentityAccountKey(auth)
	baseURLHost := runtimeIdentityBaseURLHost(auth, provider)
	proxyURL := strings.TrimSpace(authProxyURL(auth))
	if proxyURL == "" && cfg != nil {
		proxyURL = strings.TrimSpace(cfg.ProxyURL)
	}
	seed := strings.Join([]string{
		"account-runtime-identity/v2",
		provider,
		authID,
		accountKey,
		baseURLHost,
		strings.TrimSpace(profile.ProfileID),
		strings.TrimSpace(profile.TLSProfileID),
	}, "|")
	seedHash := sha256Hex(seed)
	createdAt := now
	revision := 1
	identityID := "ari_" + shortHash(seedHash, 20)
	if previous != nil && previous.Current != nil {
		if strings.TrimSpace(previous.Current.IdentityID) == identityID {
			createdAt = strings.TrimSpace(previous.Current.CreatedAt)
			if createdAt == "" {
				createdAt = now
			}
			revision = previous.Current.Revision + 1
			if revision <= 1 {
				revision = 2
			}
		}
	}
	return &authFileRuntimeIdentitySnapshot{
		IdentityID:      identityID,
		Provider:        provider,
		PolicyVersion:   runtimeIdentityPolicyVersion(provider),
		Source:          strings.TrimSpace(profile.Source),
		Revision:        revision,
		CreatedAt:       createdAt,
		UpdatedAt:       now,
		SeedHash:        "sha256:" + seedHash,
		AuthIDHash:      "sha256:" + sha256Hex(authID),
		AccountHash:     "sha256:" + sha256Hex(accountKey),
		BaseURLHost:     baseURLHost,
		ProxyHash:       optionalSHA256(proxyURL),
		ProfileID:       strings.TrimSpace(profile.ProfileID),
		TLSProfileID:    strings.TrimSpace(profile.TLSProfileID),
		Family:          strings.TrimSpace(profile.Family),
		TLSFamily:       strings.TrimSpace(profile.TLSFamily),
		CoreManaged:     profile.CoreManaged,
		RuntimeEnforced: profile.SupportsRuntime(),
		StableIdentity: map[string]string{
			"auth_id_hash":  "sha256:" + sha256Hex(authID),
			"account_hash":  "sha256:" + sha256Hex(accountKey),
			"base_url_host": baseURLHost,
			"proxy_hash":    optionalSHA256(proxyURL),
		},
		RuntimeSemantics: map[string]string{
			"transport": profile.TransportStatus,
			"tls":       profile.TLSStatus,
		},
	}
}

func runtimeIdentityPolicyVersion(provider string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "claude":
		return "claude-runtime-identity/v2"
	case "codex":
		return "codex-runtime-identity/v2"
	case "gemini", "gemini-cli":
		return "gemini-runtime-identity/v2"
	default:
		return "account-runtime-identity/v2"
	}
}

func runtimeIdentityAuthID(auth *coreauth.Auth) string {
	if auth == nil {
		return "anonymous"
	}
	for _, value := range []string{auth.ID, auth.FileName, auth.Label, authDisplayName(auth)} {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return "anonymous"
}

func runtimeIdentityAccountKey(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if accountType, accountValue := auth.AccountInfo(); strings.TrimSpace(accountValue) != "" {
		if strings.TrimSpace(accountType) != "" {
			return strings.TrimSpace(accountType) + ":" + strings.TrimSpace(accountValue)
		}
		return strings.TrimSpace(accountValue)
	}
	if auth.Metadata != nil {
		for _, key := range []string{"email", "username", "name", "account_id", "subject", "user_id"} {
			if value, ok := auth.Metadata[key].(string); ok {
				if trimmed := strings.TrimSpace(value); trimmed != "" {
					return key + ":" + trimmed
				}
			}
		}
	}
	return ""
}

func runtimeIdentityBaseURLHost(auth *coreauth.Auth, provider string) string {
	if auth != nil && auth.Attributes != nil {
		if baseURL := strings.TrimSpace(auth.Attributes["base_url"]); baseURL != "" {
			if parsed, err := url.Parse(baseURL); err == nil && parsed.Hostname() != "" {
				return strings.ToLower(parsed.Hostname())
			}
			if parsed, err := url.Parse("https://" + strings.TrimLeft(baseURL, "/")); err == nil && parsed.Hostname() != "" {
				return strings.ToLower(parsed.Hostname())
			}
		}
	}
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "claude":
		return "api.anthropic.com"
	case "codex":
		return "chatgpt.com"
	case "gemini", "gemini-cli":
		return "cloudcode-pa.googleapis.com"
	default:
		return ""
	}
}

func runtimeIdentitySnapshotEquivalentForRevision(left *authFileRuntimeIdentitySnapshot, right *authFileRuntimeIdentitySnapshot) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	leftNormalized := *left
	rightNormalized := *right
	leftNormalized.Revision = 0
	rightNormalized.Revision = 0
	leftNormalized.CreatedAt = ""
	rightNormalized.CreatedAt = ""
	leftNormalized.UpdatedAt = ""
	rightNormalized.UpdatedAt = ""
	return reflect.DeepEqual(normalizeRuntimeIdentitySnapshot(&leftNormalized), normalizeRuntimeIdentitySnapshot(&rightNormalized))
}

func runtimeIdentityStateEquivalent(left *authFileRuntimeIdentityState, right *authFileRuntimeIdentityState) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return strings.TrimSpace(left.PolicyVersion) == strings.TrimSpace(right.PolicyVersion) &&
		runtimeIdentitySnapshotEquivalentForRevision(left.Current, right.Current) &&
		reflect.DeepEqual(left.History, right.History)
}

func diffRuntimeIdentitySnapshotFields(previous *authFileRuntimeIdentitySnapshot, next *authFileRuntimeIdentitySnapshot) []string {
	if previous == nil || next == nil {
		return nil
	}
	changed := make(map[string]struct{})
	addRuntimeIdentityChange(changed, "identity_id", previous.IdentityID, next.IdentityID)
	addRuntimeIdentityChange(changed, "provider", previous.Provider, next.Provider)
	addRuntimeIdentityChange(changed, "policy_version", previous.PolicyVersion, next.PolicyVersion)
	addRuntimeIdentityChange(changed, "source", previous.Source, next.Source)
	addRuntimeIdentityChange(changed, "seed_hash", previous.SeedHash, next.SeedHash)
	addRuntimeIdentityChange(changed, "auth_id_hash", previous.AuthIDHash, next.AuthIDHash)
	addRuntimeIdentityChange(changed, "account_hash", previous.AccountHash, next.AccountHash)
	addRuntimeIdentityChange(changed, "base_url_host", previous.BaseURLHost, next.BaseURLHost)
	addRuntimeIdentityChange(changed, "proxy_hash", previous.ProxyHash, next.ProxyHash)
	addRuntimeIdentityChange(changed, "profile_id", previous.ProfileID, next.ProfileID)
	addRuntimeIdentityChange(changed, "tls_profile_id", previous.TLSProfileID, next.TLSProfileID)
	addRuntimeIdentityChange(changed, "family", previous.Family, next.Family)
	addRuntimeIdentityChange(changed, "tls_family", previous.TLSFamily, next.TLSFamily)
	addRuntimeIdentityChange(changed, "core_managed", strconv.FormatBool(previous.CoreManaged), strconv.FormatBool(next.CoreManaged))
	addRuntimeIdentityChange(changed, "runtime_enforced", strconv.FormatBool(previous.RuntimeEnforced), strconv.FormatBool(next.RuntimeEnforced))
	for _, field := range diffManagedHeaderFields(previous.StableIdentity, next.StableIdentity) {
		changed["stable_identity."+field] = struct{}{}
	}
	for _, field := range diffManagedHeaderFields(previous.RuntimeSemantics, next.RuntimeSemantics) {
		changed["runtime_semantics."+field] = struct{}{}
	}
	fields := make([]string, 0, len(changed))
	for field := range changed {
		fields = append(fields, field)
	}
	sort.Strings(fields)
	return fields
}

func addRuntimeIdentityChange(out map[string]struct{}, field string, previous string, next string) {
	if strings.TrimSpace(previous) != strings.TrimSpace(next) {
		out[field] = struct{}{}
	}
}

func optionalSHA256(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	return "sha256:" + sha256Hex(value)
}

func sha256Hex(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func shortHash(value string, length int) string {
	value = strings.TrimSpace(value)
	if length <= 0 || len(value) <= length {
		return value
	}
	return value[:length]
}

func normalizeManagedHeaderState(state *authFileManagedHeaderState) *authFileManagedHeaderState {
	if state == nil {
		return nil
	}
	normalized := &authFileManagedHeaderState{
		PolicyVersion: strings.TrimSpace(state.PolicyVersion),
	}
	if state.Current != nil {
		normalized.Current = &authFileManagedHeaderProjection{
			GeneratedAt:           strings.TrimSpace(state.Current.GeneratedAt),
			Source:                strings.TrimSpace(state.Current.Source),
			SourceURL:             strings.TrimSpace(state.Current.SourceURL),
			CheckedAt:             strings.TrimSpace(state.Current.CheckedAt),
			Completeness:          strings.TrimSpace(state.Current.Completeness),
			VariantPolicy:         strings.TrimSpace(state.Current.VariantPolicy),
			VersionVariant:        strings.TrimSpace(state.Current.VersionVariant),
			BrandOrderVariant:     strings.TrimSpace(state.Current.BrandOrderVariant),
			SummaryHeaders:        normalizeHeaderMap(state.Current.SummaryHeaders),
			VersionedCapabilities: normalizeHeaderMap(state.Current.VersionedCapabilities),
			StableIdentity:        normalizeHeaderMap(state.Current.StableIdentity),
			RuntimeFingerprint:    normalizeHeaderMap(state.Current.RuntimeFingerprint),
		}
		if normalized.Current.GeneratedAt == "" {
			normalized.Current.GeneratedAt = time.Now().UTC().Format(time.RFC3339)
		}
	}
	if len(state.History) > 0 {
		normalized.History = make([]authFileManagedHeaderHistoryEntry, 0, len(state.History))
		for _, entry := range state.History {
			changedFields := append([]string(nil), entry.ChangedFields...)
			sort.Strings(changedFields)
			normalized.History = append(normalized.History, authFileManagedHeaderHistoryEntry{
				RecordedAt:                    strings.TrimSpace(entry.RecordedAt),
				PolicyVersion:                 strings.TrimSpace(entry.PolicyVersion),
				Reason:                        strings.TrimSpace(entry.Reason),
				ChangedFields:                 changedFields,
				PreviousSource:                strings.TrimSpace(entry.PreviousSource),
				PreviousSourceURL:             strings.TrimSpace(entry.PreviousSourceURL),
				NextSource:                    strings.TrimSpace(entry.NextSource),
				NextSourceURL:                 strings.TrimSpace(entry.NextSourceURL),
				PreviousSummaryHeaders:        normalizeHeaderMap(entry.PreviousSummaryHeaders),
				NextSummaryHeaders:            normalizeHeaderMap(entry.NextSummaryHeaders),
				PreviousVersionedCapabilities: normalizeHeaderMap(entry.PreviousVersionedCapabilities),
				NextVersionedCapabilities:     normalizeHeaderMap(entry.NextVersionedCapabilities),
				PreviousStableIdentity:        normalizeHeaderMap(entry.PreviousStableIdentity),
				NextStableIdentity:            normalizeHeaderMap(entry.NextStableIdentity),
				PreviousRuntimeFingerprint:    normalizeHeaderMap(entry.PreviousRuntimeFingerprint),
				NextRuntimeFingerprint:        normalizeHeaderMap(entry.NextRuntimeFingerprint),
			})
		}
	}
	if normalized.PolicyVersion == "" {
		normalized.PolicyVersion = managedHeaderPolicyVersion("")
	}
	if normalized.Current == nil && len(normalized.History) == 0 {
		return nil
	}
	return normalized
}

func managedHeaderPolicyVersion(provider string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "claude":
		return "claude-managed/v2"
	case "codex":
		return "codex-managed/v2"
	default:
		return "managed/v2"
	}
}

func mergeManagedHeaderState(previous *authFileManagedHeaderState, projection authFileManagedHeaderProjection, provider string) *authFileManagedHeaderState {
	if len(projection.SummaryHeaders) == 0 &&
		len(projection.VersionedCapabilities) == 0 &&
		len(projection.StableIdentity) == 0 &&
		len(projection.RuntimeFingerprint) == 0 {
		return normalizeManagedHeaderState(previous)
	}

	normalizedPrev := normalizeManagedHeaderState(previous)
	current := &authFileManagedHeaderProjection{
		GeneratedAt:           projection.GeneratedAt,
		Source:                strings.TrimSpace(projection.Source),
		SourceURL:             strings.TrimSpace(projection.SourceURL),
		CheckedAt:             strings.TrimSpace(projection.CheckedAt),
		Completeness:          strings.TrimSpace(projection.Completeness),
		VariantPolicy:         strings.TrimSpace(projection.VariantPolicy),
		VersionVariant:        strings.TrimSpace(projection.VersionVariant),
		BrandOrderVariant:     strings.TrimSpace(projection.BrandOrderVariant),
		SummaryHeaders:        normalizeHeaderMap(projection.SummaryHeaders),
		VersionedCapabilities: normalizeHeaderMap(projection.VersionedCapabilities),
		StableIdentity:        normalizeHeaderMap(projection.StableIdentity),
		RuntimeFingerprint:    normalizeHeaderMap(projection.RuntimeFingerprint),
	}
	if strings.TrimSpace(current.GeneratedAt) == "" {
		current.GeneratedAt = time.Now().UTC().Format(time.RFC3339)
	}

	if normalizedPrev != nil && managedHeaderProjectionEquivalent(normalizedPrev.Current, current) {
		return normalizedPrev
	}

	state := &authFileManagedHeaderState{
		PolicyVersion: managedHeaderPolicyVersion(provider),
		Current:       current,
	}
	if normalizedPrev == nil {
		return state
	}

	if state.PolicyVersion == "" {
		state.PolicyVersion = normalizedPrev.PolicyVersion
	}
	state.History = append(state.History, normalizedPrev.History...)
	if normalizedPrev.Current != nil && !managedHeaderProjectionEquivalent(normalizedPrev.Current, current) {
		state.History = append(state.History, authFileManagedHeaderHistoryEntry{
			RecordedAt:                    current.GeneratedAt,
			PolicyVersion:                 state.PolicyVersion,
			Reason:                        "managed-header-refresh",
			ChangedFields:                 diffManagedHeaderProjectionFields(normalizedPrev.Current, current),
			PreviousSource:                strings.TrimSpace(normalizedPrev.Current.Source),
			PreviousSourceURL:             strings.TrimSpace(normalizedPrev.Current.SourceURL),
			NextSource:                    strings.TrimSpace(current.Source),
			NextSourceURL:                 strings.TrimSpace(current.SourceURL),
			PreviousSummaryHeaders:        normalizeHeaderMap(normalizedPrev.Current.SummaryHeaders),
			NextSummaryHeaders:            normalizeHeaderMap(current.SummaryHeaders),
			PreviousVersionedCapabilities: normalizeHeaderMap(normalizedPrev.Current.VersionedCapabilities),
			NextVersionedCapabilities:     normalizeHeaderMap(current.VersionedCapabilities),
			PreviousStableIdentity:        normalizeHeaderMap(normalizedPrev.Current.StableIdentity),
			NextStableIdentity:            normalizeHeaderMap(current.StableIdentity),
			PreviousRuntimeFingerprint:    normalizeHeaderMap(normalizedPrev.Current.RuntimeFingerprint),
			NextRuntimeFingerprint:        normalizeHeaderMap(current.RuntimeFingerprint),
		})
	}
	if len(state.History) > 12 {
		state.History = append([]authFileManagedHeaderHistoryEntry(nil), state.History[len(state.History)-12:]...)
	}
	return state
}

func diffManagedHeaderProjectionFields(previous *authFileManagedHeaderProjection, next *authFileManagedHeaderProjection) []string {
	if previous == nil || next == nil {
		return nil
	}
	seen := make(map[string]struct{})
	addChangedManagedHeaderFields(seen, previous.SummaryHeaders, next.SummaryHeaders)
	addChangedManagedHeaderFields(seen, previous.StableIdentity, next.StableIdentity)
	addChangedManagedHeaderFields(seen, previous.RuntimeFingerprint, next.RuntimeFingerprint)
	addChangedManagedHeaderFields(seen, previous.VersionedCapabilities, next.VersionedCapabilities)
	fields := make([]string, 0, len(seen))
	for key := range seen {
		fields = append(fields, key)
	}
	sort.Strings(fields)
	return fields
}

func addChangedManagedHeaderFields(out map[string]struct{}, previous map[string]string, next map[string]string) {
	for _, field := range diffManagedHeaderFields(previous, next) {
		out[field] = struct{}{}
	}
}

func diffManagedHeaderFields(previous map[string]string, next map[string]string) []string {
	seen := make(map[string]struct{}, len(previous)+len(next))
	fields := make([]string, 0, len(previous)+len(next))
	for key := range previous {
		seen[key] = struct{}{}
	}
	for key := range next {
		seen[key] = struct{}{}
	}
	for key := range seen {
		if strings.TrimSpace(previous[key]) == strings.TrimSpace(next[key]) {
			continue
		}
		fields = append(fields, key)
	}
	sort.Strings(fields)
	return fields
}

func managedHeaderProjectionEquivalent(left *authFileManagedHeaderProjection, right *authFileManagedHeaderProjection) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return reflect.DeepEqual(left.SummaryHeaders, right.SummaryHeaders) &&
		reflect.DeepEqual(left.VersionedCapabilities, right.VersionedCapabilities) &&
		reflect.DeepEqual(left.StableIdentity, right.StableIdentity) &&
		reflect.DeepEqual(left.RuntimeFingerprint, right.RuntimeFingerprint) &&
		strings.TrimSpace(left.Source) == strings.TrimSpace(right.Source) &&
		strings.TrimSpace(left.SourceURL) == strings.TrimSpace(right.SourceURL) &&
		strings.TrimSpace(left.VariantPolicy) == strings.TrimSpace(right.VariantPolicy) &&
		strings.TrimSpace(left.VersionVariant) == strings.TrimSpace(right.VersionVariant) &&
		strings.TrimSpace(left.BrandOrderVariant) == strings.TrimSpace(right.BrandOrderVariant)
}

func managedHeaderStateEquivalent(left *authFileManagedHeaderState, right *authFileManagedHeaderState) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return strings.TrimSpace(left.PolicyVersion) == strings.TrimSpace(right.PolicyVersion) &&
		managedHeaderProjectionEquivalent(left.Current, right.Current) &&
		reflect.DeepEqual(left.History, right.History)
}

func accountSettingsActivationSummary(auth *coreauth.Auth, managedHeaders map[string]string, extraHeaders map[string]string, refreshEnabled bool, transportRuntimeEnforced bool, tlsRuntimeEnforced bool) string {
	if auth != nil && auth.Disabled {
		return "disabled"
	}
	switch {
	case authProxyURL(auth) != "":
		return "proxy override active"
	case transportRuntimeEnforced:
		return "transport profile active"
	case tlsRuntimeEnforced:
		return "TLS profile active"
	case !refreshEnabled:
		return "refresh disabled"
	case len(extraHeaders) > 0:
		return "custom extra headers active"
	case len(managedHeaders) > 0:
		return "managed headers active"
	default:
		return "core defaults active"
	}
}

func accountSettingsActivationState(auth *coreauth.Auth, transportProfile any, tlsProfile any, refreshEnabled bool, transportRuntimeEnforced bool, tlsRuntimeEnforced bool) string {
	if auth != nil && auth.Disabled {
		return "disabled"
	}
	if !refreshEnabled {
		return "refresh-disabled"
	}
	if transportRuntimeEnforced {
		return "transport-profile-active"
	}
	if tlsRuntimeEnforced {
		return "tls-profile-active"
	}
	if transportProfile != nil || tlsProfile != nil {
		return "reserved-profiles-present"
	}
	return "active"
}

func findAuthByName(authManager *coreauth.Manager, name string) *coreauth.Auth {
	if authManager == nil {
		return nil
	}
	trimmedName := strings.TrimSpace(name)
	if trimmedName == "" {
		return nil
	}
	if auth, ok := authManager.GetByID(trimmedName); ok {
		return auth
	}
	for _, auth := range authManager.List() {
		if auth != nil && authDisplayName(auth) == trimmedName {
			return auth
		}
	}
	return nil
}

type refreshAuthFileStatusRequest struct {
	Name    string `json:"name"`
	Trigger string `json:"trigger"`
}

type testAuthFileMessageRequest struct {
	Name      string `json:"name"`
	Model     string `json:"model"`
	Message   string `json:"message"`
	MaxTokens int    `json:"max_tokens"`
}

func (h *Handler) TestAuthFileMessage(c *gin.Context) {
	if h == nil || h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "auth manager unavailable"})
		return
	}

	var req testAuthFileMessageRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
		return
	}

	auth := findAuthByName(h.authManager, name)
	if auth == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "auth file not found"})
		return
	}
	if auth.Disabled || auth.Status == coreauth.StatusDisabled {
		c.JSON(http.StatusBadRequest, gin.H{"error": "auth file is disabled"})
		return
	}

	model := strings.TrimSpace(req.Model)
	modelFromProviderDefault := false
	if model == "" {
		models := registry.GetGlobalRegistry().GetModelsForClient(auth.ID)
		for _, info := range models {
			if info != nil && strings.TrimSpace(info.ID) != "" {
				model = strings.TrimSpace(info.ID)
				break
			}
		}
	}
	if model == "" {
		model = defaultAuthFileTestMessageModel(auth)
		modelFromProviderDefault = model != ""
	}
	if model == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "model is required because this auth has no registered models yet"})
		return
	}

	message := strings.TrimSpace(req.Message)
	if message == "" {
		message = "Reply with OK."
	}
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 16
	}
	if maxTokens > 64 {
		maxTokens = 64
	}

	payload, errMarshal := json.Marshal(map[string]any{
		"model": model,
		"messages": []map[string]string{
			{"role": "user", "content": message},
		},
		"stream":     false,
		"max_tokens": maxTokens,
	})
	if errMarshal != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to build test request"})
		return
	}

	selectedAuthID := ""
	ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Minute)
	defer cancel()

	provider := providerKey(auth)
	if provider == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "auth file provider is required"})
		return
	}
	if modelFromProviderDefault {
		registerAuthFileTestMessageModel(auth.ID, provider, model)
	}
	metadata := map[string]any{
		cliproxyexecutor.RequestedModelMetadataKey: model,
		cliproxyexecutor.PinnedAuthMetadataKey:     auth.ID,
		cliproxyexecutor.SelectedAuthCallbackMetadataKey: func(authID string) {
			selectedAuthID = strings.TrimSpace(authID)
		},
	}
	execReq := cliproxyexecutor.Request{
		Model:    model,
		Payload:  payload,
		Metadata: metadata,
	}
	opts := cliproxyexecutor.Options{
		Stream:          false,
		OriginalRequest: payload,
		SourceFormat:    sdktranslator.FromString("openai"),
		Metadata:        metadata,
	}
	metadata[cliproxyexecutor.SelectedAuthCallbackMetadataKey] = func(authID string) {
		selectedAuthID = strings.TrimSpace(authID)
	}

	startedAt := time.Now()
	resp, errExecute := h.authManager.Execute(ctx, []string{provider}, execReq, opts)
	latencyMs := time.Since(startedAt).Milliseconds()
	if errExecute != nil {
		status := http.StatusBadGateway
		if statusCoder, ok := errExecute.(interface{ StatusCode() int }); ok && statusCoder != nil {
			if code := statusCoder.StatusCode(); code > 0 {
				status = code
			}
		}
		if status <= 0 {
			status = http.StatusBadGateway
		}
		responseSelectedAuthID := selectedAuthID
		if responseSelectedAuthID == "" {
			responseSelectedAuthID = auth.ID
		}
		c.JSON(status, gin.H{
			"error":            errExecute.Error(),
			"auth_id":          auth.ID,
			"selected_auth_id": responseSelectedAuthID,
			"provider":         provider,
			"model":            model,
			"latency_ms":       latencyMs,
		})
		return
	}
	if selectedAuthID != "" && selectedAuthID != auth.ID {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":            "test request selected a different auth account",
			"auth_id":          auth.ID,
			"selected_auth_id": selectedAuthID,
		})
		return
	}

	responseSelectedAuthID := selectedAuthID
	if responseSelectedAuthID == "" {
		responseSelectedAuthID = auth.ID
	}
	c.JSON(http.StatusOK, gin.H{
		"status":           "ok",
		"name":             authDisplayName(auth),
		"auth_id":          auth.ID,
		"selected_auth_id": responseSelectedAuthID,
		"provider":         provider,
		"model":            model,
		"latency_ms":       latencyMs,
		"output_preview":   truncateTestMessagePreview(extractTestMessagePreview(resp.Payload), 240),
	})
}

func defaultAuthFileTestMessageModel(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	provider := providerKey(auth)
	var models []*registry.ModelInfo
	preferred := []string{}
	switch provider {
	case "codex":
		planType := registry.NormalizeCodexSubscriptionPlan(authFileSubscriptionPlanType(auth))
		models = registry.GetCodexModelsForPlan(planType)
		preferred = []string{"gpt-5.4-mini", "gpt-5.2", "gpt-5.3-codex"}
	case "claude", "anthropic":
		planType := registry.NormalizeClaudeSubscriptionPlan(authFileSubscriptionPlanType(auth))
		models = registry.GetClaudeModelsForPlan(planType, claudeUsageCreditsEnabledFromQuotaSnapshot(auth.Metadata))
		preferred = []string{"claude-haiku-4-5-20251001", "claude-3-5-haiku-20241022"}
	case "gemini-cli":
		models = registry.GetGeminiCLIModels()
		preferred = []string{"gemini-2.5-flash-lite", "gemini-2.5-flash"}
	case "kimi":
		models = registry.GetKimiModels()
	case "antigravity":
		models = registry.GetAntigravityModels()
		preferred = []string{"claude-sonnet-4-6", "gemini-3-flash"}
	default:
		return ""
	}
	if id := firstPreferredModelID(models, preferred); id != "" {
		return id
	}
	for _, info := range models {
		if info == nil {
			continue
		}
		id := strings.TrimSpace(info.ID)
		if id == "" || strings.EqualFold(id, "gpt-5.3-codex-spark") {
			continue
		}
		return id
	}
	return ""
}

func authFileSubscriptionPlanType(auth *coreauth.Auth) string {
	return auth.SubscriptionPlanType()
}

func registerAuthFileTestMessageModel(authID, provider, model string) {
	authID = strings.TrimSpace(authID)
	provider = strings.TrimSpace(provider)
	model = strings.TrimSpace(model)
	if authID == "" || provider == "" || model == "" {
		return
	}
	info := registry.LookupStaticModelInfo(model)
	if info == nil {
		info = &registry.ModelInfo{
			ID:      model,
			Object:  "model",
			OwnedBy: provider,
			Type:    provider,
			Version: model,
		}
	}
	registry.GetGlobalRegistry().RegisterClient(authID, provider, []*registry.ModelInfo{info})
}

func firstPreferredModelID(models []*registry.ModelInfo, preferred []string) string {
	if len(models) == 0 || len(preferred) == 0 {
		return ""
	}
	available := make(map[string]string, len(models))
	for _, info := range models {
		if info == nil {
			continue
		}
		id := strings.TrimSpace(info.ID)
		if id == "" {
			continue
		}
		available[strings.ToLower(id)] = id
	}
	for _, want := range preferred {
		if id := available[strings.ToLower(strings.TrimSpace(want))]; id != "" {
			return id
		}
	}
	return ""
}

func extractTestMessagePreview(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	for _, path := range []string{
		"choices.0.message.content",
		"choices.0.text",
		"output_text",
		"response.output_text",
		"response.output.0.content.0.text",
		"output.0.content.0.text",
	} {
		if value := strings.TrimSpace(gjson.GetBytes(payload, path).String()); value != "" {
			return value
		}
	}
	return strings.TrimSpace(string(payload))
}

func truncateTestMessagePreview(value string, maxRunes int) string {
	value = strings.TrimSpace(value)
	if maxRunes <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= maxRunes {
		return value
	}
	return string(runes[:maxRunes]) + "..."
}

const authStatusRefreshFailureBackoff = time.Minute

// RefreshAuthFileStatus manually re-checks an auth entry by invoking the provider refresh flow.
func (h *Handler) RefreshAuthFileStatus(c *gin.Context) {
	if h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}

	var req refreshAuthFileStatusRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	name := strings.TrimSpace(req.Name)
	if name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
		return
	}

	targetAuth := h.findAuthByNameOrID(name)
	if targetAuth == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "auth file not found"})
		return
	}

	trigger := normalizeAuthStatusHistoryTrigger(req.Trigger)
	before := authStatusHistorySnapshotFromAuth(targetAuth)
	updated, errRefresh := h.refreshAuthStatus(c.Request.Context(), targetAuth)
	if updated == nil {
		updated = targetAuth
	}
	after := authStatusHistorySnapshotFromAuth(updated)
	entry := h.buildAuthFileEntry(updated)
	if entry == nil {
		entry = gin.H{
			"name":     name,
			"provider": after.Provider,
			"status":   after.Status,
		}
	}

	h.appendAuthStatusHistoryEvent(authStatusHistoryEvent{
		EventType:       deriveAuthStatusHistoryEventType(before, after, errRefresh),
		AuthName:        after.AuthName,
		Provider:        after.Provider,
		Trigger:         trigger,
		PreviousStatus:  before.Status,
		PreviousMessage: before.StatusMessage,
		Status:          after.Status,
		StatusMessage:   after.StatusMessage,
		Error:           errorString(errRefresh),
	})

	resp := gin.H{
		"status": "ok",
		"file":   entry,
	}
	if errRefresh != nil {
		resp["status"] = "warning"
		resp["error"] = errRefresh.Error()
	}
	c.JSON(http.StatusOK, resp)
}

func (h *Handler) refreshAuthStatus(ctx context.Context, current *coreauth.Auth) (*coreauth.Auth, error) {
	if h == nil || h.authManager == nil || current == nil {
		return current, fmt.Errorf("core auth manager unavailable")
	}
	if current.RefreshDisabled() {
		updated := current.Clone()
		updated.NextRefreshAfter = time.Time{}
		updated.UpdatedAt = time.Now()
		saved, errUpdate := h.authManager.Update(ctx, updated)
		if errUpdate != nil {
			return saved, errUpdate
		}
		return saved, errAuthRefreshDisabled
	}
	exec, ok := h.authManager.Executor(current.Provider)
	if !ok || exec == nil {
		return current.Clone(), fmt.Errorf("provider does not support status refresh")
	}

	now := time.Now()
	candidate := current.Clone()
	attemptStartedAt := now
	attemptRefreshToken := authRefreshTokenValue(candidate)
	updated, errRefresh := exec.Refresh(ctx, candidate)
	if updated == nil {
		updated = candidate
	}
	if updated == nil {
		updated = current.Clone()
	}
	preserveAuthIdentity(updated, current)

	if errRefresh != nil {
		authErr := normalizeAuthRefreshError(errRefresh)
		latest := h.latestAuthSnapshot(current)
		if latest == nil {
			latest = current.Clone()
		}
		preserveAuthIdentity(latest, current)
		if refreshAttemptSuperseded(latest, attemptRefreshToken, attemptStartedAt) {
			return latest, nil
		}
		if coreauth.IsRefreshTokenReuseError(errRefresh) {
			latest.MarkRefreshReauthRequired(now)
			saved, errUpdate := h.authManager.Update(ctx, latest)
			if errUpdate != nil {
				return saved, errUpdate
			}
			if saved != nil && saved.LastError != nil {
				return saved, saved.LastError
			}
			return saved, authErr
		}
		latest.LastError = authErr
		latest.Status = coreauth.StatusError
		latest.StatusMessage = strings.TrimSpace(authErr.Message)
		latest.Unavailable = true
		latest.NextRefreshAfter = now.Add(authStatusRefreshFailureBackoff)
		latest.UpdatedAt = now
		if authErr.HTTPStatus == http.StatusTooManyRequests {
			latest.Quota.Exceeded = true
			latest.Quota.Reason = "quota"
			latest.Quota.NextRecoverAt = now.Add(authStatusRefreshFailureBackoff)
		}
		saved, errUpdate := h.authManager.Update(ctx, latest)
		if errUpdate != nil {
			return saved, errUpdate
		}
		return saved, authErr
	}

	latest := h.latestAuthSnapshot(current)
	if refreshAttemptSuperseded(latest, attemptRefreshToken, attemptStartedAt) {
		return latest, nil
	}

	updated.LastError = nil
	updated.Quota.Exceeded = false
	updated.Quota.Reason = ""
	updated.Quota.NextRecoverAt = time.Time{}
	updated.NextRetryAfter = time.Time{}
	updated.NextRefreshAfter = time.Time{}
	updated.LastRefreshedAt = now
	updated.UpdatedAt = now
	// Bug fix: a successful OAuth token refresh only proves the
	// refresh_token itself is still valid; it does NOT prove the account can
	// serve real model requests, which is the actual signal the automatic
	// terminal-auth quarantine gate (AutoQuarantined; see
	// evaluateAutoQuarantineLocked / markAutoQuarantine) guards against. A
	// revoked/banned account can keep refreshing its access token
	// successfully indefinitely while every real model request still
	// returns a terminal 401. The previous "T3 (telemetry-farm-ux-hardening)"
	// behavior here called Auth.ClearAutoQuarantine() unconditionally on
	// refresh success (both the manual "recheck status" action and the
	// periodic auto-refresh trigger share this same code path), which let a
	// routine token refresh silently lift a genuine quarantine lock with no
	// evidence the account actually recovered. The two sanctioned recovery
	// signals are unchanged: a real model request succeeding
	// (Manager.MarkResult -> evaluateAutoQuarantineLocked) and a completed
	// re-authentication (saveTokenRecord -> Auth.ClearAutoQuarantine).
	if updated.AutoQuarantined {
		// Keep the persisted Status/StatusMessage/Unavailable in sync with
		// the still-active quarantine lock instead of reporting the
		// self-contradictory "status=active" that an unconditional reset
		// here would otherwise produce for a credential the selector
		// (isAuthBlockedForModel) still blocks on AutoQuarantined alone.
		updated.Status = coreauth.StatusQuarantined
		updated.StatusMessage = coreauth.AutoQuarantineStatusMessage
		updated.Unavailable = true
	} else {
		updated.Status = coreauth.StatusActive
		updated.StatusMessage = ""
		updated.Unavailable = false
	}

	return h.authManager.Update(ctx, updated)
}

func (h *Handler) latestAuthSnapshot(current *coreauth.Auth) *coreauth.Auth {
	if h == nil || h.authManager == nil || current == nil {
		return nil
	}
	if current.ID != "" {
		if latest, ok := h.authManager.GetByID(current.ID); ok && latest != nil {
			return latest
		}
	}
	name := strings.TrimSpace(current.FileName)
	if name == "" {
		name = filepath.Base(strings.TrimSpace(authAttribute(current, "path")))
	}
	if name != "" {
		return h.findAuthByNameOrID(name)
	}
	return nil
}

func authRefreshTokenValue(auth *coreauth.Auth) string {
	if auth == nil || auth.Metadata == nil {
		return ""
	}
	if token, ok := auth.Metadata["refresh_token"].(string); ok {
		return strings.TrimSpace(token)
	}
	return ""
}

func refreshAttemptSuperseded(latest *coreauth.Auth, attemptedRefreshToken string, attemptStartedAt time.Time) bool {
	if latest == nil {
		return false
	}
	latestRefreshToken := authRefreshTokenValue(latest)
	if attemptedRefreshToken != "" && latestRefreshToken != "" && latestRefreshToken != attemptedRefreshToken {
		return true
	}
	if attemptStartedAt.IsZero() {
		return false
	}
	if !latest.LastRefreshedAt.IsZero() && latest.LastRefreshedAt.After(attemptStartedAt) {
		return true
	}
	if latest.Metadata != nil {
		if refreshedAt, ok := extractLastRefreshTimestamp(latest.Metadata); ok && refreshedAt.After(attemptStartedAt) {
			return true
		}
	}
	return false
}

func preserveAuthIdentity(updated, current *coreauth.Auth) {
	if updated == nil || current == nil {
		return
	}
	if updated.ID == "" {
		updated.ID = current.ID
	}
	if updated.Provider == "" {
		updated.Provider = current.Provider
	}
	if updated.FileName == "" {
		updated.FileName = current.FileName
	}
	if updated.Label == "" {
		updated.Label = current.Label
	}
	if updated.Runtime == nil {
		updated.Runtime = current.Runtime
	}
	if updated.CreatedAt.IsZero() {
		updated.CreatedAt = current.CreatedAt
	}
	if updated.Attributes == nil {
		updated.Attributes = current.Attributes
	}
}

func normalizeAuthRefreshError(err error) *coreauth.Error {
	if err == nil {
		return nil
	}
	var authErr *coreauth.Error
	if errors.As(err, &authErr) && authErr != nil {
		return &coreauth.Error{
			Code:       authErr.Code,
			Message:    strings.TrimSpace(authErr.Message),
			Retryable:  authErr.Retryable,
			HTTPStatus: authErr.HTTPStatus,
		}
	}
	return &coreauth.Error{
		Code:    "refresh_failed",
		Message: strings.TrimSpace(err.Error()),
	}
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return strings.TrimSpace(err.Error())
}

func (h *Handler) GetAuthFileAccountSettings(c *gin.Context) {
	if h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}

	name := strings.TrimSpace(c.Query("name"))
	if name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
		return
	}

	targetAuth := findAuthByName(h.authManager, name)
	if targetAuth == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "auth file not found"})
		return
	}
	targetAuth = h.syncAuthManagedHeaderState(c.Request.Context(), targetAuth)

	c.JSON(http.StatusOK, authFileAccountSettingsResponse{
		Name:            authDisplayName(targetAuth),
		AccountSettings: buildAuthFileAccountSettingsView(targetAuth, h.cfg),
	})
}

func (h *Handler) PatchAuthFileAccountSettings(c *gin.Context) {
	if h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}

	var req struct {
		Name             string            `json:"name"`
		ProxyURL         *string           `json:"proxy_url"`
		Note             *string           `json:"note"`
		Disabled         *bool             `json:"disabled"`
		ExtraHeaders     map[string]string `json:"extra_headers"`
		RefreshEnabled   *bool             `json:"refresh_enabled"`
		Fast             *bool             `json:"fast"`
		FarmEnrolled     *bool             `json:"farm_enrolled"`
		TransportProfile any               `json:"transport_profile"`
		TLSProfile       any               `json:"tls_profile"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	name := strings.TrimSpace(req.Name)
	if name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
		return
	}
	if req.Disabled == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "disabled is required"})
		return
	}

	targetAuth := findAuthByName(h.authManager, name)
	if targetAuth == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "auth file not found"})
		return
	}

	extraHeaders := normalizeHeaderMap(req.ExtraHeaders)
	for key := range extraHeaders {
		if authHeaderReservedForExtras(targetAuth.Provider, key) {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("extra header %q conflicts with managed or protocol-reserved headers", key)})
			return
		}
	}

	if targetAuth.Metadata == nil {
		targetAuth.Metadata = make(map[string]any)
	}
	if targetAuth.Attributes == nil {
		targetAuth.Attributes = make(map[string]string)
	}

	proxyURL := ""
	if req.ProxyURL != nil {
		proxyURL = strings.TrimSpace(*req.ProxyURL)
	}
	// Reject saving an account without a usable proxy_url. An empty proxy_url would
	// force a direct connection to the upstream provider and expose the real server
	// IP, so the value must be non-empty and a valid proxy setting. The explicit
	// "direct"/"none" sentinels remain accepted as an intentional operator choice.
	if !*req.Disabled {
		if proxyURL == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "proxy_url is required: empty proxy_url would expose the real server IP via direct egress"})
			return
		}
		if _, errProxy := proxyutil.Parse(proxyURL); errProxy != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("invalid proxy_url: %v", errProxy)})
			return
		}
	}
	noteValue := ""
	if req.Note != nil {
		noteValue = strings.TrimSpace(*req.Note)
	}
	transportProfile := normalizeAccountProfileValue(req.TransportProfile)
	tlsProfile := normalizeAccountProfileValue(req.TLSProfile)
	existingStored := readAccountSettingsMetadata(targetAuth, h.cfg)
	refreshEnabled := accountSettingsRefreshEnabled(targetAuth, existingStored)
	if req.RefreshEnabled != nil {
		refreshEnabled = *req.RefreshEnabled
	}
	farmEnrolled := accountSettingsFarmEnrolled(targetAuth, existingStored)
	if req.FarmEnrolled != nil {
		farmEnrolled = *req.FarmEnrolled
	}

	// Bug fix: capture the pre-mutation Disabled value so the auto-quarantine
	// clear below can be gated on a genuine disabled=true -> disabled=false
	// transition instead of firing on every save that merely resends
	// disabled=false (see the wasDisabled comment further down).
	wasDisabled := targetAuth.Disabled
	targetAuth.Disabled = *req.Disabled
	if *req.Disabled {
		targetAuth.Status = coreauth.StatusDisabled
		targetAuth.StatusMessage = "disabled via management API"
	} else {
		if targetAuth.Status == coreauth.StatusDisabled {
			targetAuth.Status = coreauth.StatusActive
			targetAuth.StatusMessage = ""
		}
		// See PatchAuthFileStatus above: an explicit operator "not disabled"
		// intent is also a valid recovery signal for core's automatic
		// terminal-auth quarantine (T3).
		//
		// Bug fix: only do this on a genuine disabled=true -> disabled=false
		// transition (wasDisabled). Auto-quarantine never sets Disabled=true
		// (it is tracked independently via AutoQuarantined/Status), so an
		// already-enabled, auto-quarantined account has Disabled=false both
		// before and after this call. Without the wasDisabled guard, an
		// operator merely editing an unrelated field (proxy_url/note/
		// extra_headers) on such an account and saving -- which resends the
		// unchanged disabled=false -- would silently and incorrectly lift a
		// real quarantine lock as a side effect.
		if wasDisabled {
			targetAuth.ClearAutoQuarantine()
		}
	}

	targetAuth.ProxyURL = proxyURL
	if proxyURL == "" {
		delete(targetAuth.Metadata, "proxy_url")
	} else {
		targetAuth.Metadata["proxy_url"] = proxyURL
	}

	if noteValue == "" {
		delete(targetAuth.Metadata, "note")
		delete(targetAuth.Attributes, "note")
	} else {
		targetAuth.Metadata["note"] = noteValue
		targetAuth.Attributes["note"] = noteValue
	}

	targetAuth.Metadata["account_settings"] = authFileAccountSettingsStored{
		SchemaVersion:         accountSettingsSchemaVersion,
		ManagedHeaderSeedHash: accountManagedHeaderSeedHash(targetAuth),
		ExtraHeaders:          extraHeaders,
		RefreshEnabled:        refreshEnabledStorageValue(refreshEnabled),
		FarmEnrolled:          farmEnrolledStorageValue(farmEnrolled),
		TransportProfile:      transportProfile,
		TLSProfile:            tlsProfile,
		ManagedHeaderState:    existingStored.ManagedHeaderState,
		RuntimeIdentityState:  existingStored.RuntimeIdentityState,
	}
	applyAuthRefreshEnabledMetadata(targetAuth, refreshEnabled)
	applyAuthFarmEnrolledMetadata(targetAuth, farmEnrolled)

	// Codex fast is a first-class, codex-only account setting. Only mutate
	// fast_models when the field is present in the request AND this is a codex
	// account; a non-codex account's fast toggle is a no-op (the runtime gate can
	// never fire there) so we never write dead fast_models metadata onto it. The
	// value is a top-level metadata key (not part of account_settings), so the
	// account_settings assignment above does not clobber it, and it is written
	// after that assignment for clarity. syncAuthManagedHeaderState clones the auth
	// and preserves arbitrary top-level metadata keys, so fast_models survives the
	// sync and reaches the store the same way refresh_disabled/refresh_enabled do.
	if req.Fast != nil && strings.EqualFold(providerKey(targetAuth), "codex") {
		applyAuthFastModelsMetadata(targetAuth, *req.Fast)
	}

	targetAuth = h.syncAuthManagedHeaderState(c.Request.Context(), targetAuth)
	managedHeaders := managedHeadersForAuth(targetAuth, h.cfg)
	runtimeHeaders := mergeAccountHeaders(managedHeaders, extraHeaders)
	overwriteAuthMetadataHeaders(targetAuth, runtimeHeaders)

	targetAuth.UpdatedAt = time.Now()
	if _, err := h.authManager.Update(c.Request.Context(), targetAuth); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to update auth: %v", err)})
		return
	}

	c.JSON(http.StatusOK, authFileAccountSettingsResponse{
		Name:            authDisplayName(targetAuth),
		AccountSettings: buildAuthFileAccountSettingsView(targetAuth, h.cfg),
	})
}

func (h *Handler) disableAuth(ctx context.Context, id string) {
	if h == nil || h.authManager == nil {
		return
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return
	}
	if auth, ok := h.authManager.GetByID(id); ok {
		auth.Disabled = true
		auth.Status = coreauth.StatusDisabled
		auth.StatusMessage = "removed via management API"
		auth.UpdatedAt = time.Now()
		_, _ = h.authManager.Update(ctx, auth)
		return
	}
	authID := h.authIDForPath(id)
	if authID == "" {
		return
	}
	if auth, ok := h.authManager.GetByID(authID); ok {
		auth.Disabled = true
		auth.Status = coreauth.StatusDisabled
		auth.StatusMessage = "removed via management API"
		auth.UpdatedAt = time.Now()
		_, _ = h.authManager.Update(ctx, auth)
	}
}

// validateAuthFileClaudeDeviceIDPatch validates a claude_device_id PATCH
// value before it is written to Metadata. An empty string is a valid "clear
// the override" request (the account falls back to the derived synthetic
// device_id); any non-empty value must be a well-formed 64-char lowercase
// hex string matching the wire shape of metadata.user_id.device_id.
func validateAuthFileClaudeDeviceIDPatch(value any) error {
	str, ok := value.(string)
	if !ok {
		return fmt.Errorf("claude_device_id must be a string")
	}
	if strings.TrimSpace(str) == "" {
		return nil
	}
	if !coreauth.IsValidClaudeDeviceID(str) {
		return fmt.Errorf("claude_device_id must be a 64-character lowercase hex string")
	}
	return nil
}

// syncAuthFileClaudeDeviceIDAttribute mirrors a just-patched
// claude_device_id Metadata value into the live Attributes map (same pattern
// as syncAuthFilePriorityAttribute / syncAuthFileNoteAttribute) so the
// in-memory Auth object used by the current process reflects the change
// immediately, without waiting for a full store reload. It clears the
// mirrored attribute when the persisted value is missing, empty, or invalid
// so the account reliably falls back to the derived synthetic device_id.
func syncAuthFileClaudeDeviceIDAttribute(auth *coreauth.Auth) {
	if auth == nil {
		return
	}
	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	deviceID, ok := auth.Metadata[coreauth.ClaudeDeviceIDMetadataKey].(string)
	deviceID = strings.TrimSpace(deviceID)
	if !ok || !coreauth.IsValidClaudeDeviceID(deviceID) {
		delete(auth.Attributes, coreauth.ClaudeDeviceIDAttributeKey)
		return
	}
	auth.Attributes[coreauth.ClaudeDeviceIDAttributeKey] = deviceID
}

// validateFarmAliveAtPatch validates a farm_container_alive_at PATCH value
// before it is written to Metadata (write-side guard mirroring
// validateAuthFileClaudeDeviceIDPatch). An empty string is a valid "clear the
// heartbeat" request (the account is then treated as not-alive by the
// container-liveness gate); any non-empty value MUST parse as an RFC3339
// timestamp so a malformed heartbeat can never be persisted and later mis-read
// as fresh by the gate predicate.
func validateFarmAliveAtPatch(value any) error {
	str, ok := value.(string)
	if !ok {
		return fmt.Errorf("farm_container_alive_at must be a string")
	}
	trimmed := strings.TrimSpace(str)
	if trimmed == "" {
		return nil
	}
	if _, err := time.Parse(time.RFC3339, trimmed); err != nil {
		return fmt.Errorf("farm_container_alive_at must be an RFC3339 timestamp")
	}
	return nil
}

// syncAuthFileContainerAliveAtAttribute mirrors a just-patched
// farm_container_alive_at Metadata value into the live Attributes map (same
// immediate-visibility pattern as syncAuthFileClaudeDeviceIDAttribute) so the
// container-liveness gate (coreauth.authContainerRecentlyAlive, which reads
// Attributes) observes the new heartbeat in-process without waiting for a full
// store reload. It clears the mirrored attribute when the persisted value is
// missing, empty (an explicit "clear"), or not a string, so a cleared heartbeat
// reliably falls back to not-alive.
func syncAuthFileContainerAliveAtAttribute(auth *coreauth.Auth) {
	if auth == nil {
		return
	}
	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	aliveAt, ok := auth.Metadata[coreauth.FarmContainerAliveAtMetadataKey].(string)
	aliveAt = strings.TrimSpace(aliveAt)
	if !ok || aliveAt == "" {
		delete(auth.Attributes, coreauth.FarmContainerAliveAtAttributeKey)
		return
	}
	auth.Attributes[coreauth.FarmContainerAliveAtAttributeKey] = aliveAt
}

// lookupExistingAuthForReauth attempts to find a prior auth record that maps to
// the same account as the supplied re-auth record. The returned previous record
// is used to merge user-defined metadata; orphanPath is non-empty when the
// previous record lives at a different filename (typically because the OAuth
// claims caused the filename to change), in which case the caller is expected
// to delete that file after the new record is persisted.
func (h *Handler) lookupExistingAuthForReauth(record *coreauth.Auth) (previous *coreauth.Auth, orphanPath string) {
	if h == nil || h.authManager == nil || record == nil {
		return nil, ""
	}
	if existing, ok := h.authManager.GetByID(record.ID); ok && existing != nil {
		return existing, ""
	}
	// Fall back to provider + email/account_id matching so we still inherit user
	// fields when the credential filename changed (e.g. codex plan-type change).
	newEmail := strings.ToLower(strings.TrimSpace(authEmail(record)))
	newAccountID := strings.ToLower(strings.TrimSpace(metadataString(record.Metadata, "account_id")))
	if newEmail == "" && newAccountID == "" {
		return nil, ""
	}
	provider := strings.ToLower(strings.TrimSpace(record.Provider))
	if provider == "" {
		return nil, ""
	}
	var candidate *coreauth.Auth
	for _, candAuth := range h.authManager.List() {
		if candAuth == nil {
			continue
		}
		if candAuth.ID == record.ID {
			continue
		}
		if strings.ToLower(strings.TrimSpace(candAuth.Provider)) != provider {
			continue
		}
		candEmail := strings.ToLower(strings.TrimSpace(authEmail(candAuth)))
		candAccountID := strings.ToLower(strings.TrimSpace(metadataString(candAuth.Metadata, "account_id")))
		matched := false
		if newEmail != "" && candEmail == newEmail {
			matched = true
		}
		if !matched && newAccountID != "" && candAccountID == newAccountID {
			matched = true
		}
		if !matched {
			continue
		}
		// Prefer the candidate that still has user-defined data we want to keep.
		if candidate == nil || hasUserDefinedAuthMetadata(candAuth) && !hasUserDefinedAuthMetadata(candidate) {
			candidate = candAuth
		}
	}
	if candidate == nil {
		return nil, ""
	}
	orphan := strings.TrimSpace(authAttribute(candidate, "path"))
	return candidate, orphan
}

// reauthUserDefinedMetadataKeys lists metadata keys that originate from the
// operator (or were previously persisted by the management UI) and must
// survive an OAuth re-auth round-trip even when the OAuth response does not
// echo them back.
var reauthUserDefinedMetadataKeys = []string{
	"proxy_url",
	"note",
	"label",
	"tags",
	"headers",
	"extra_headers",
	"refresh_disabled",
	"refresh_enabled",
	"disable_refresh",
	"auto_refresh_disabled",
	"auto_refresh",
	"auto_refresh_enabled",
	"websockets",
	"websocket",
	"websocket_settings",
	"disabled",
	"account_settings",
	"runtime_only",
	"priority",
	"prefix",
	"claude_device_id",
}

// reauthTokenMetadataKeys lists metadata keys whose values are owned by the
// OAuth response itself and therefore must always come from the new record,
// never from the previous one. Anything not in this list and not in
// reauthUserDefinedMetadataKeys is treated as custom data and inherited so we
// do not lose forward-compatible operator additions.
var reauthTokenMetadataKeys = map[string]struct{}{
	"access_token":           {},
	"refresh_token":          {},
	"id_token":               {},
	"token":                  {},
	"email":                  {},
	"account_id":             {},
	"username":               {},
	"chatgpt_account_id":     {},
	"plan_type":              {},
	"expired":                {},
	"expires_at":             {},
	"oauth_expires_at":       {},
	"expires_in":             {},
	"last_refresh":           {},
	"lastrefresh":            {},
	"last_refreshed_at":      {},
	"lastrefreshedat":        {},
	"timestamp":              {},
	"type":                   {},
	"project_id":             {},
	"auto":                   {},
	"checked":                {},
	"scope":                  {},
	"token_type":             {},
	"auth_method":            {},
	"duo_gateway_base_url":   {},
	"duo_gateway_token":      {},
	"duo_gateway_headers":    {},
	"duo_gateway_expires_at": {},
	"model_provider":         {},
	"model_name":             {},
	"model_details":          {},
}

// reauthRuntimeMetadataKeys lists derived runtime state that should not survive
// an OAuth re-auth round-trip. Keeping stale quota errors across re-auth can
// make a recovered credential continue to look unauthorized in the management UI.
var reauthRuntimeMetadataKeys = map[string]struct{}{
	quotaRefreshStatusMetadataKey: {},
	quotaRefreshErrorMetadataKey:  {},
	quotaNextRefreshMetadataKey:   {},
}

func isReauthUserDefinedMetadataKey(key string) bool {
	key = strings.ToLower(strings.TrimSpace(key))
	if key == "" {
		return false
	}
	for _, candidate := range reauthUserDefinedMetadataKeys {
		if key == candidate {
			return true
		}
	}
	return false
}

func isReauthTokenMetadataKey(key string) bool {
	if _, ok := reauthTokenMetadataKeys[strings.ToLower(strings.TrimSpace(key))]; ok {
		return true
	}
	return false
}

func isReauthRuntimeMetadataKey(key string) bool {
	if _, ok := reauthRuntimeMetadataKeys[strings.ToLower(strings.TrimSpace(key))]; ok {
		return true
	}
	return false
}

// mergeUserDefinedAuthMetadataInto copies operator-controlled metadata from the
// previous auth record onto the new record without overwriting OAuth/token
// owned keys. The new record retains precedence whenever it already has a
// non-empty value for a given key so callers can still intentionally update
// preserved fields (e.g. supply a new note during re-auth).
func mergeUserDefinedAuthMetadataInto(record, previous *coreauth.Auth) {
	if record == nil || previous == nil || len(previous.Metadata) == 0 {
		return
	}
	if record.Metadata == nil {
		record.Metadata = make(map[string]any, len(previous.Metadata))
	}
	for key, value := range previous.Metadata {
		trimmedKey := strings.TrimSpace(key)
		if trimmedKey == "" {
			continue
		}
		if isReauthTokenMetadataKey(trimmedKey) {
			continue
		}
		if isReauthRuntimeMetadataKey(trimmedKey) {
			continue
		}
		if existing, ok := record.Metadata[trimmedKey]; ok && !isEmptyMetadataValue(existing) {
			continue
		}
		record.Metadata[trimmedKey] = value
	}
	// Make sure ProxyURL field on the record mirrors the persisted metadata so
	// downstream consumers (account_settings projection, runtime executors)
	// see the same value the operator previously configured.
	if record.ProxyURL == "" {
		if proxy := strings.TrimSpace(metadataString(record.Metadata, "proxy_url")); proxy != "" {
			record.ProxyURL = proxy
		} else if proxy := strings.TrimSpace(previous.ProxyURL); proxy != "" {
			record.ProxyURL = proxy
			if record.Metadata == nil {
				record.Metadata = make(map[string]any)
			}
			if _, ok := record.Metadata["proxy_url"]; !ok {
				record.Metadata["proxy_url"] = proxy
			}
		}
	}
	if strings.TrimSpace(record.Label) == "" && strings.TrimSpace(previous.Label) != "" {
		record.Label = previous.Label
	}
	if strings.TrimSpace(record.Prefix) == "" && strings.TrimSpace(previous.Prefix) != "" {
		record.Prefix = previous.Prefix
	}
}

// inheritUserDefinedAuthAttributesInto copies operator-controlled attributes
// (custom headers, note, runtime_only marker, etc.) from the previous record
// when the new record omits them. OAuth handlers typically only populate the
// `path` attribute, so without this step every re-auth would drop the
// management-UI configured header overrides.
func inheritUserDefinedAuthAttributesInto(record, previous *coreauth.Auth) {
	if record == nil || previous == nil || len(previous.Attributes) == 0 {
		return
	}
	if record.Attributes == nil {
		record.Attributes = make(map[string]string, len(previous.Attributes))
	}
	for key, value := range previous.Attributes {
		trimmedKey := strings.TrimSpace(key)
		if trimmedKey == "" {
			continue
		}
		lowerKey := strings.ToLower(trimmedKey)
		// Identity attributes are rebuilt from the new auth record. Skip them
		// so we do not accidentally restore stale paths or emails.
		if lowerKey == "path" || lowerKey == "email" || lowerKey == "account_email" {
			continue
		}
		if existing, ok := record.Attributes[trimmedKey]; ok && strings.TrimSpace(existing) != "" {
			continue
		}
		record.Attributes[trimmedKey] = value
	}
}

func hasUserDefinedAuthMetadata(auth *coreauth.Auth) bool {
	if auth == nil || len(auth.Metadata) == 0 {
		return false
	}
	for key := range auth.Metadata {
		if isReauthUserDefinedMetadataKey(key) {
			value := auth.Metadata[key]
			if !isEmptyMetadataValue(value) {
				return true
			}
		}
	}
	return false
}

func isEmptyMetadataValue(value any) bool {
	switch v := value.(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(v) == ""
	case map[string]any:
		return len(v) == 0
	case map[string]string:
		return len(v) == 0
	case []any:
		return len(v) == 0
	case []string:
		return len(v) == 0
	default:
		return false
	}
}

func metadataString(meta map[string]any, key string) string {
	if len(meta) == 0 {
		return ""
	}
	value, ok := meta[key]
	if !ok {
		return ""
	}
	if str, isStr := value.(string); isStr {
		return str
	}
	return ""
}

func gitLabBaseURLFromRequest(c *gin.Context) string {
	if c != nil {
		if raw := strings.TrimSpace(c.Query("base_url")); raw != "" {
			return gitlabauth.NormalizeBaseURL(raw)
		}
	}
	if raw := strings.TrimSpace(os.Getenv("GITLAB_BASE_URL")); raw != "" {
		return gitlabauth.NormalizeBaseURL(raw)
	}
	return gitlabauth.DefaultBaseURL
}

func buildGitLabAuthMetadata(baseURL, mode string, tokenResp *gitlabauth.TokenResponse, direct *gitlabauth.DirectAccessResponse) map[string]any {
	metadata := map[string]any{
		"type":                     "gitlab",
		"auth_method":              strings.TrimSpace(mode),
		"base_url":                 gitlabauth.NormalizeBaseURL(baseURL),
		"last_refresh":             time.Now().UTC().Format(time.RFC3339),
		"refresh_interval_seconds": 240,
	}
	if tokenResp != nil {
		metadata["access_token"] = strings.TrimSpace(tokenResp.AccessToken)
		if refreshToken := strings.TrimSpace(tokenResp.RefreshToken); refreshToken != "" {
			metadata["refresh_token"] = refreshToken
		}
		if tokenType := strings.TrimSpace(tokenResp.TokenType); tokenType != "" {
			metadata["token_type"] = tokenType
		}
		if scope := strings.TrimSpace(tokenResp.Scope); scope != "" {
			metadata["scope"] = scope
		}
		if expiry := gitlabauth.TokenExpiry(time.Now(), tokenResp); !expiry.IsZero() {
			metadata["oauth_expires_at"] = expiry.Format(time.RFC3339)
		}
	}
	mergeGitLabDirectAccessMetadata(metadata, direct)
	return metadata
}

func mergeGitLabDirectAccessMetadata(metadata map[string]any, direct *gitlabauth.DirectAccessResponse) {
	if metadata == nil || direct == nil {
		return
	}
	if base := strings.TrimSpace(direct.BaseURL); base != "" {
		metadata["duo_gateway_base_url"] = base
	}
	if token := strings.TrimSpace(direct.Token); token != "" {
		metadata["duo_gateway_token"] = token
	}
	if direct.ExpiresAt > 0 {
		expiry := time.Unix(direct.ExpiresAt, 0).UTC()
		metadata["duo_gateway_expires_at"] = expiry.Format(time.RFC3339)
		now := time.Now().UTC()
		if ttl := expiry.Sub(now); ttl > 0 {
			interval := int(ttl.Seconds()) / 2
			switch {
			case interval < 60:
				interval = 60
			case interval > 240:
				interval = 240
			}
			metadata["refresh_interval_seconds"] = interval
		}
	}
	if len(direct.Headers) > 0 {
		headers := make(map[string]string, len(direct.Headers))
		for key, value := range direct.Headers {
			key = strings.TrimSpace(key)
			value = strings.TrimSpace(value)
			if key == "" || value == "" {
				continue
			}
			headers[key] = value
		}
		if len(headers) > 0 {
			metadata["duo_gateway_headers"] = headers
		}
	}
	if direct.ModelDetails != nil {
		modelDetails := map[string]any{}
		if provider := strings.TrimSpace(direct.ModelDetails.ModelProvider); provider != "" {
			modelDetails["model_provider"] = provider
			metadata["model_provider"] = provider
		}
		if model := strings.TrimSpace(direct.ModelDetails.ModelName); model != "" {
			modelDetails["model_name"] = model
			metadata["model_name"] = model
		}
		if len(modelDetails) > 0 {
			metadata["model_details"] = modelDetails
		}
	}
}

func primaryGitLabEmail(user *gitlabauth.User) string {
	if user == nil {
		return ""
	}
	if value := strings.TrimSpace(user.Email); value != "" {
		return value
	}
	return strings.TrimSpace(user.PublicEmail)
}

func gitLabAccountIdentifier(user *gitlabauth.User) string {
	if user == nil {
		return "user"
	}
	for _, value := range []string{user.Username, primaryGitLabEmail(user), user.Name} {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return "user"
}

func sanitizeGitLabFileName(value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	if value == "" {
		return "user"
	}
	var builder strings.Builder
	lastDash := false
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
			builder.WriteRune(r)
			lastDash = false
		case r >= '0' && r <= '9':
			builder.WriteRune(r)
			lastDash = false
		case r == '-' || r == '_' || r == '.':
			builder.WriteRune(r)
			lastDash = false
		default:
			if !lastDash {
				builder.WriteRune('-')
				lastDash = true
			}
		}
	}
	result := strings.Trim(builder.String(), "-")
	if result == "" {
		return "user"
	}
	return result
}

func maskGitLabToken(token string) string {
	trimmed := strings.TrimSpace(token)
	if trimmed == "" {
		return ""
	}
	if len(trimmed) <= 8 {
		return trimmed
	}
	return trimmed[:4] + "..." + trimmed[len(trimmed)-4:]
}

func maskOAuthStateForLog(state string) string {
	return maskGitLabToken(state)
}

func safeAuthNameForLog(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	if len(name) <= 16 {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	return name[:8] + "..." + hex.EncodeToString(sum[:])[:8]
}

func cloneAuthStringMap(input map[string]string) map[string]string {
	if len(input) == 0 {
		return nil
	}
	out := make(map[string]string, len(input))
	for key, value := range input {
		out[key] = value
	}
	return out
}

func cloneClaudeReauthMetadata(target *coreauth.Auth, email string) map[string]any {
	metadata := make(map[string]any)
	if target != nil {
		for key, value := range target.Metadata {
			switch strings.ToLower(strings.TrimSpace(key)) {
			case "access_token", "refresh_token", "id_token", "token", "expired", "expires_at", "last_refresh", "lastrefresh", "email", "type":
				continue
			default:
				metadata[key] = value
			}
		}
		metadata["disabled"] = target.Disabled
	}
	metadata["email"] = email
	metadata["type"] = "claude"
	return metadata
}

func buildClaudeOAuthTokenRecord(target *coreauth.Auth, tokenStorage *claude.ClaudeTokenStorage) *coreauth.Auth {
	email := ""
	if tokenStorage != nil {
		email = strings.TrimSpace(tokenStorage.Email)
	}
	defaultName := fmt.Sprintf("claude-%s.json", email)
	if email == "" {
		defaultName = "claude-auth.json"
	}
	record := &coreauth.Auth{
		ID:       defaultName,
		Provider: "claude",
		FileName: defaultName,
		Storage:  tokenStorage,
		Metadata: cloneClaudeReauthMetadata(nil, email),
	}
	if target == nil {
		return record
	}
	record.ID = target.ID
	record.FileName = authDisplayName(target)
	record.Label = target.Label
	record.Disabled = target.Disabled
	record.Status = target.Status
	record.StatusMessage = target.StatusMessage
	record.ProxyURL = authProxyURL(target)
	record.Attributes = cloneAuthStringMap(target.Attributes)
	if record.Attributes == nil {
		record.Attributes = make(map[string]string)
	}
	if path := strings.TrimSpace(authAttribute(target, "path")); path != "" {
		record.Attributes["path"] = path
	}
	record.Metadata = cloneClaudeReauthMetadata(target, email)
	if record.ProxyURL != "" {
		record.Metadata["proxy_url"] = record.ProxyURL
	}
	return record
}

// claudeReauthHighWaterUserAgent returns the persisted claude device-profile
// high-water User-Agent for target with its suffix entrypoint folded
// (sdk-cli -> cli, gated by config.NormalizeSdkCliEntrypointEnabled) so the
// reauth token-endpoint identity matches the serving outbound UA suffix aligned
// by helps.AlignClaudeDeviceProfileUserAgentSuffix. It returns "" when target
// has no usable high-water metadata yet (e.g. a brand-new account with no prior
// serving observation). Callers pass the result to ClaudeAuth.WithUserAgent,
// which no-ops on an empty value and leaves the constructor's floor untouched.
func claudeReauthHighWaterUserAgent(cfg *config.Config, target *coreauth.Auth) string {
	if target == nil {
		return ""
	}
	hw, ok := coreauth.ClaudeDeviceHighWaterFromMetadata(target.Metadata)
	if !ok {
		return ""
	}
	ua := strings.TrimSpace(hw.UserAgent)
	if ua == "" {
		return ""
	}
	return runtimehelps.NormalizeClaudeUserAgentEntrypoint(cfg, ua)
}

func (h *Handler) newClaudeOAuthAuth(ctx context.Context, target *coreauth.Auth) *claude.ClaudeAuth {
	if target == nil {
		return claude.NewClaudeAuth(h.cfg)
	}
	client := h.oauthIdentityHTTPClient(ctx, "api.anthropic.com", target, anthropicOAuthExchangeTimeout)
	// Raise the reauth User-Agent to this account's persisted device-profile
	// high-water mark (when present) so the OAuth exchange/refresh presents
	// the same claude-cli identity this account's serving requests use,
	// instead of falling back to the generic claudeOAuthUserAgent floor.
	return claude.NewClaudeAuthWithHTTPClient(client).WithUserAgent(claudeReauthHighWaterUserAgent(h.cfg, target))
}

func (h *Handler) newClaudeOAuthAccountProxyFallbackAuth(target *coreauth.Auth) *claude.ClaudeAuth {
	if target == nil {
		return nil
	}
	proxyURL := authProxyURL(target)
	if proxyURL == "" {
		return nil
	}
	return claude.NewClaudeAuthWithProxyURL(h.cfg, proxyURL).WithUserAgent(claudeReauthHighWaterUserAgent(h.cfg, target))
}

func (h *Handler) claudeOAuthTransportSummary(target *coreauth.Auth) map[string]string {
	summary := map[string]string{
		"proxy_source":       "core-global",
		"transport_profile":  "",
		"tls_profile":        "",
		"runtime_profile_id": "",
	}
	if target == nil {
		if h != nil && h.cfg != nil && strings.TrimSpace(h.cfg.ProxyURL) == "" {
			summary["proxy_source"] = "direct"
		}
		return summary
	}
	if authProxyURL(target) != "" {
		summary["proxy_source"] = "account"
	} else if h != nil && h.cfg != nil && strings.TrimSpace(h.cfg.ProxyURL) != "" {
		summary["proxy_source"] = "core-global"
	} else {
		summary["proxy_source"] = "direct"
	}
	if profile := runtimehelps.ResolveRuntimeTransportProfile(target); profile != nil {
		summary["transport_profile"] = profile.ProfileID
		summary["tls_profile"] = profile.TLSProfileID
		summary["runtime_profile_id"] = profile.ProfileID
	}
	return summary
}

func exchangeClaudeOAuthWithRetry(ctx context.Context, auth *claude.ClaudeAuth, code string, state string, pkceCodes *claude.PKCECodes, safeState string, targetAuthName string) (*claude.ClaudeAuthBundle, error) {
	if auth == nil {
		return nil, fmt.Errorf("Claude OAuth service unavailable")
	}
	var lastErr error
	for attempt := 1; attempt <= anthropicOAuthExchangeRetries; attempt++ {
		bundle, err := auth.ExchangeCodeForTokens(ctx, code, state, pkceCodes)
		if err == nil {
			return bundle, nil
		}
		lastErr = err
		if attempt >= anthropicOAuthExchangeRetries || !isRetriableClaudeOAuthExchangeError(err) {
			return nil, err
		}
		delay := time.Duration(attempt) * time.Second
		log.WithFields(log.Fields{
			"state":     safeState,
			"auth_name": safeAuthNameForLog(targetAuthName),
			"attempt":   attempt,
			"next_in":   delay.String(),
			"error":     summarizeClaudeOAuthExchangeLogError(err),
		}).Warn("Claude OAuth token exchange hit a transient network/proxy error; retrying")
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
	}
	return nil, lastErr
}

func isRetriableClaudeOAuthExchangeError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	lower := strings.ToLower(err.Error())
	if !strings.Contains(lower, "token exchange request failed") {
		return false
	}
	if strings.Contains(lower, "context canceled") || strings.Contains(lower, "context deadline exceeded") {
		return false
	}
	for _, marker := range []string{
		"socks connect",
		"proxyconnect",
		"connection not allowed",
		"connection refused",
		"connection reset",
		"network is unreachable",
		"no such host",
		"i/o timeout",
		"tls handshake timeout",
		"timeout awaiting response headers",
		"temporary failure",
		"unexpected eof",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	if strings.HasSuffix(strings.TrimSpace(lower), ": eof") || strings.Contains(lower, ": eof\n") {
		return true
	}
	return false
}

func summarizeClaudeOAuthExchangeLogError(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if len(msg) > 240 {
		msg = msg[:240] + "..."
	}
	return msg
}

func tokenStorageEmailForLog(bundle *claude.ClaudeAuthBundle) string {
	if bundle == nil {
		return ""
	}
	return strings.TrimSpace(bundle.TokenData.Email)
}

func (h *Handler) RequestGeminiCLIToken(c *gin.Context) {
	ctx := context.Background()
	ctx = PopulateAuthContext(ctx, c)
	setup, errSetup := parseOAuthAccountSetupFromRequest(c)
	if errSetup != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errSetup.Error()})
		return
	}
	effectiveCfg := h.configForOAuthSetup(setup)
	proxyHTTPClient := util.SetProxy(&effectiveCfg.SDKConfig, &http.Client{})
	ctx = context.WithValue(ctx, oauth2.HTTPClient, proxyHTTPClient)

	// Optional project ID from query
	projectID := c.Query("project_id")

	fmt.Println("Initializing Google authentication...")

	// OAuth2 configuration using exported constants from internal/auth/gemini
	conf := &oauth2.Config{
		ClientID:     geminiAuth.ClientID,
		ClientSecret: geminiAuth.ClientSecret,
		RedirectURL:  fmt.Sprintf("http://localhost:%d/oauth2callback", geminiAuth.DefaultCallbackPort),
		Scopes:       geminiAuth.Scopes,
		Endpoint:     google.Endpoint,
	}

	// Build authorization URL and return it immediately
	state := fmt.Sprintf("gem-%d", time.Now().UnixNano())
	authURL := conf.AuthCodeURL(state, oauth2.AccessTypeOffline, oauth2.SetAuthURLParam("prompt", "consent"))

	RegisterOAuthSession(state, "gemini")

	isWebUI := isWebUIRequest(c)
	var forwarder *callbackForwarder
	if isWebUI {
		targetURL, errTarget := h.managementCallbackURL("/google/callback")
		if errTarget != nil {
			log.WithError(errTarget).Error("failed to compute gemini callback target")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "callback server unavailable"})
			return
		}
		var errStart error
		if forwarder, errStart = startCallbackForwarder(geminiCallbackPort, "gemini", targetURL); errStart != nil {
			log.WithError(errStart).Error("failed to start gemini callback forwarder")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to start callback server"})
			return
		}
	}

	go func() {
		if isWebUI {
			defer stopCallbackForwarderInstance(geminiCallbackPort, forwarder)
		}

		// Wait for callback file written by server route
		waitFile := filepath.Join(h.cfg.AuthDir, fmt.Sprintf(".oauth-gemini-%s.oauth", state))
		fmt.Println("Waiting for authentication callback...")
		deadline := time.Now().Add(oauthCallbackWaitTimeout)
		var authCode string
		for {
			if !IsOAuthSessionPending(state, "gemini") {
				return
			}
			if time.Now().After(deadline) {
				log.Error("oauth flow timed out")
				SetOAuthSessionError(state, "OAuth flow timed out")
				return
			}
			if data, errR := os.ReadFile(waitFile); errR == nil {
				var m map[string]string
				_ = json.Unmarshal(data, &m)
				_ = os.Remove(waitFile)
				if errStr := m["error"]; errStr != "" {
					log.Errorf("Authentication failed: %s", errStr)
					SetOAuthSessionError(state, "Authentication failed")
					return
				}
				authCode = m["code"]
				if authCode == "" {
					log.Errorf("Authentication failed: code not found")
					SetOAuthSessionError(state, "Authentication failed: code not found")
					return
				}
				break
			}
			time.Sleep(500 * time.Millisecond)
		}

		// Exchange authorization code for token
		token, err := conf.Exchange(ctx, authCode)
		if err != nil {
			log.Errorf("Failed to exchange token: %v", err)
			SetOAuthSessionError(state, "Failed to exchange token")
			return
		}

		requestedProjectID := strings.TrimSpace(projectID)

		// Create token storage (mirrors internal/auth/gemini createTokenStorage)
		authHTTPClient := conf.Client(ctx, token)
		req, errNewRequest := http.NewRequestWithContext(ctx, "GET", "https://www.googleapis.com/oauth2/v1/userinfo?alt=json", nil)
		if errNewRequest != nil {
			log.Errorf("Could not get user info: %v", errNewRequest)
			SetOAuthSessionError(state, "Could not get user info")
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token.AccessToken))

		resp, errDo := authHTTPClient.Do(req)
		if errDo != nil {
			log.Errorf("Failed to execute request: %v", errDo)
			SetOAuthSessionError(state, "Failed to execute request")
			return
		}
		defer func() {
			if errClose := resp.Body.Close(); errClose != nil {
				log.Printf("warn: failed to close response body: %v", errClose)
			}
		}()

		bodyBytes, _ := io.ReadAll(resp.Body)
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			log.Errorf("Get user info request failed with status %d: %s", resp.StatusCode, string(bodyBytes))
			SetOAuthSessionError(state, fmt.Sprintf("Get user info request failed with status %d", resp.StatusCode))
			return
		}

		email := gjson.GetBytes(bodyBytes, "email").String()
		if email != "" {
			fmt.Printf("Authenticated user email: %s\n", email)
		} else {
			fmt.Println("Failed to get user email from token")
		}

		// Marshal/unmarshal oauth2.Token to generic map and enrich fields
		var ifToken map[string]any
		jsonData, _ := json.Marshal(token)
		if errUnmarshal := json.Unmarshal(jsonData, &ifToken); errUnmarshal != nil {
			log.Errorf("Failed to unmarshal token: %v", errUnmarshal)
			SetOAuthSessionError(state, "Failed to unmarshal token")
			return
		}

		ifToken["token_uri"] = "https://oauth2.googleapis.com/token"
		ifToken["client_id"] = geminiAuth.ClientID
		ifToken["client_secret"] = geminiAuth.ClientSecret
		ifToken["scopes"] = geminiAuth.Scopes
		ifToken["universe_domain"] = "googleapis.com"

		ts := geminiAuth.GeminiTokenStorage{
			Token:     ifToken,
			ProjectID: requestedProjectID,
			Email:     email,
			Auto:      requestedProjectID == "",
		}

		// Initialize authenticated HTTP client via GeminiAuth to honor proxy settings
		gemAuth := geminiAuth.NewGeminiAuth()
		gemClient, errGetClient := gemAuth.GetAuthenticatedClient(ctx, &ts, effectiveCfg, &geminiAuth.WebLoginOptions{
			NoBrowser: true,
		})
		if errGetClient != nil {
			log.Errorf("failed to get authenticated client: %v", errGetClient)
			SetOAuthSessionError(state, "Failed to get authenticated client")
			return
		}
		fmt.Println("Authentication successful.")

		if strings.EqualFold(requestedProjectID, "ALL") {
			ts.Auto = false
			projects, errAll := onboardAllGeminiProjects(ctx, gemClient, &ts)
			if errAll != nil {
				log.Errorf("Failed to complete Gemini CLI onboarding: %v", errAll)
				SetOAuthSessionError(state, fmt.Sprintf("Failed to complete Gemini CLI onboarding: %v", errAll))
				return
			}
			if errVerify := ensureGeminiProjectsEnabled(ctx, gemClient, projects); errVerify != nil {
				log.Errorf("Failed to verify Cloud AI API status: %v", errVerify)
				SetOAuthSessionError(state, fmt.Sprintf("Failed to verify Cloud AI API status: %v", errVerify))
				return
			}
			ts.ProjectID = strings.Join(projects, ",")
			ts.Checked = true
		} else if strings.EqualFold(requestedProjectID, "GOOGLE_ONE") {
			ts.Auto = false
			if errSetup := performGeminiCLISetup(ctx, gemClient, &ts, ""); errSetup != nil {
				log.Errorf("Google One auto-discovery failed: %v", errSetup)
				SetOAuthSessionError(state, fmt.Sprintf("Google One auto-discovery failed: %v", errSetup))
				return
			}
			if strings.TrimSpace(ts.ProjectID) == "" {
				log.Error("Google One auto-discovery returned empty project ID")
				SetOAuthSessionError(state, "Google One auto-discovery returned empty project ID")
				return
			}
			isChecked, errCheck := checkCloudAPIIsEnabled(ctx, gemClient, ts.ProjectID)
			if errCheck != nil {
				log.Errorf("Failed to verify Cloud AI API status: %v", errCheck)
				SetOAuthSessionError(state, fmt.Sprintf("Failed to verify Cloud AI API status: %v", errCheck))
				return
			}
			ts.Checked = isChecked
			if !isChecked {
				log.Error("Cloud AI API is not enabled for the auto-discovered project")
				SetOAuthSessionError(state, fmt.Sprintf("Cloud AI API not enabled for project %s", ts.ProjectID))
				return
			}
		} else {
			if errEnsure := ensureGeminiProjectAndOnboard(ctx, gemClient, &ts, requestedProjectID); errEnsure != nil {
				log.Errorf("Failed to complete Gemini CLI onboarding: %v", errEnsure)
				SetOAuthSessionError(state, fmt.Sprintf("Failed to complete Gemini CLI onboarding: %v", errEnsure))
				return
			}

			if strings.TrimSpace(ts.ProjectID) == "" {
				log.Error("Onboarding did not return a project ID")
				SetOAuthSessionError(state, "Failed to resolve project ID")
				return
			}

			isChecked, errCheck := checkCloudAPIIsEnabled(ctx, gemClient, ts.ProjectID)
			if errCheck != nil {
				log.Errorf("Failed to verify Cloud AI API status: %v", errCheck)
				SetOAuthSessionError(state, fmt.Sprintf("Failed to verify Cloud AI API status: %v", errCheck))
				return
			}
			ts.Checked = isChecked
			if !isChecked {
				log.Error("Cloud AI API is not enabled for the selected project")
				SetOAuthSessionError(state, fmt.Sprintf("Cloud AI API not enabled for project %s", ts.ProjectID))
				return
			}
		}

		recordMetadata := map[string]any{
			"email":      ts.Email,
			"project_id": ts.ProjectID,
			"auto":       ts.Auto,
			"checked":    ts.Checked,
		}

		fileName := geminiAuth.CredentialFileName(ts.Email, ts.ProjectID, true)
		record := &coreauth.Auth{
			ID:       fileName,
			Provider: "gemini",
			FileName: fileName,
			Storage:  &ts,
			Metadata: recordMetadata,
		}
		h.applyOAuthAccountSetupToRecord(record, setup)
		savedPath, errSave := h.saveTokenRecord(ctx, record)
		if errSave != nil {
			log.Errorf("Failed to save token to file: %v", errSave)
			SetOAuthSessionError(state, "Failed to save token to file")
			return
		}

		CompleteOAuthSessionWithRecord(state, savedPath, record)
		CompleteOAuthSessionsByProvider("gemini")
		fmt.Printf("You can now use Gemini CLI services through this CLI; token saved to %s\n", savedPath)
	}()

	c.JSON(http.StatusOK, oauthStartResponse(authURL, state))
}

func (h *Handler) RequestGitLabToken(c *gin.Context) {
	ctx := context.Background()
	ctx = PopulateAuthContext(ctx, c)

	fmt.Println("Initializing GitLab Duo authentication...")

	baseURL := gitLabBaseURLFromRequest(c)
	clientID := strings.TrimSpace(c.Query("client_id"))
	clientSecret := strings.TrimSpace(c.Query("client_secret"))
	if clientID == "" {
		clientID = strings.TrimSpace(os.Getenv("GITLAB_OAUTH_CLIENT_ID"))
	}
	if clientSecret == "" {
		clientSecret = strings.TrimSpace(os.Getenv("GITLAB_OAUTH_CLIENT_SECRET"))
	}
	if clientID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "gitlab client_id is required"})
		return
	}

	pkceCodes, err := gitlabauth.GeneratePKCECodes()
	if err != nil {
		log.Errorf("Failed to generate GitLab PKCE codes: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate PKCE codes"})
		return
	}

	state, err := misc.GenerateRandomState()
	if err != nil {
		log.Errorf("Failed to generate GitLab state parameter: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate state parameter"})
		return
	}

	redirectURI := gitlabauth.RedirectURL(gitlabauth.DefaultCallbackPort)
	authClient := gitlabauth.NewAuthClient(h.cfg)
	authURL, err := authClient.GenerateAuthURL(baseURL, clientID, redirectURI, state, pkceCodes)
	if err != nil {
		log.Errorf("Failed to generate GitLab authorization URL: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate authorization url"})
		return
	}

	RegisterOAuthSession(state, "gitlab")

	isWebUI := isWebUIRequest(c)
	var forwarder *callbackForwarder
	if isWebUI {
		targetURL, errTarget := h.managementCallbackURL("/gitlab/callback")
		if errTarget != nil {
			log.WithError(errTarget).Error("failed to compute gitlab callback target")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "callback server unavailable"})
			return
		}
		var errStart error
		if forwarder, errStart = startCallbackForwarder(gitlabauth.DefaultCallbackPort, "gitlab", targetURL); errStart != nil {
			log.WithError(errStart).Error("failed to start gitlab callback forwarder")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to start callback server"})
			return
		}
	}

	go func() {
		if isWebUI {
			defer stopCallbackForwarderInstance(gitlabauth.DefaultCallbackPort, forwarder)
		}

		waitFile := filepath.Join(h.cfg.AuthDir, fmt.Sprintf(".oauth-gitlab-%s.oauth", state))
		deadline := time.Now().Add(oauthCallbackWaitTimeout)
		var code string
		for {
			if !IsOAuthSessionPending(state, "gitlab") {
				return
			}
			if time.Now().After(deadline) {
				log.Error("gitlab oauth flow timed out")
				SetOAuthSessionError(state, "Timeout waiting for OAuth callback")
				return
			}
			if data, errRead := os.ReadFile(waitFile); errRead == nil {
				var payload map[string]string
				_ = json.Unmarshal(data, &payload)
				_ = os.Remove(waitFile)
				if errStr := strings.TrimSpace(payload["error"]); errStr != "" {
					SetOAuthSessionError(state, errStr)
					return
				}
				if payloadState := strings.TrimSpace(payload["state"]); payloadState != state {
					SetOAuthSessionError(state, "State code error")
					return
				}
				code = strings.TrimSpace(payload["code"])
				if code == "" {
					SetOAuthSessionError(state, "Authorization code missing")
					return
				}
				break
			}
			time.Sleep(500 * time.Millisecond)
		}

		tokenResp, errExchange := authClient.ExchangeCodeForTokens(ctx, baseURL, clientID, clientSecret, redirectURI, code, pkceCodes.CodeVerifier)
		if errExchange != nil {
			log.Errorf("Failed to exchange GitLab authorization code: %v", errExchange)
			SetOAuthSessionError(state, "Failed to exchange authorization code for tokens")
			return
		}

		user, errUser := authClient.GetCurrentUser(ctx, baseURL, tokenResp.AccessToken)
		if errUser != nil {
			log.Errorf("Failed to fetch GitLab user profile: %v", errUser)
			SetOAuthSessionError(state, "Failed to fetch account profile")
			return
		}

		direct, errDirect := authClient.FetchDirectAccess(ctx, baseURL, tokenResp.AccessToken)
		if errDirect != nil {
			log.Errorf("Failed to fetch GitLab direct access metadata: %v", errDirect)
			SetOAuthSessionError(state, "Failed to fetch GitLab Duo access")
			return
		}

		identifier := gitLabAccountIdentifier(user)
		fileName := fmt.Sprintf("gitlab-%s.json", sanitizeGitLabFileName(identifier))
		metadata := buildGitLabAuthMetadata(baseURL, gitLabLoginModeOAuth, tokenResp, direct)
		metadata["auth_kind"] = "oauth"
		metadata["oauth_client_id"] = clientID
		if clientSecret != "" {
			metadata["oauth_client_secret"] = clientSecret
		}
		metadata["username"] = strings.TrimSpace(user.Username)
		if email := primaryGitLabEmail(user); email != "" {
			metadata["email"] = email
		}
		metadata["name"] = strings.TrimSpace(user.Name)

		record := &coreauth.Auth{
			ID:       fileName,
			Provider: "gitlab",
			FileName: fileName,
			Label:    identifier,
			Metadata: metadata,
		}
		savedPath, errSave := h.saveTokenRecord(ctx, record)
		if errSave != nil {
			log.Errorf("Failed to save GitLab auth record: %v", errSave)
			SetOAuthSessionError(state, "Failed to save authentication tokens")
			return
		}

		fmt.Printf("GitLab Duo authentication successful. Token saved to %s\n", savedPath)
		CompleteOAuthSessionWithRecord(state, savedPath, record)
		CompleteOAuthSessionsByProvider("gitlab")
	}()

	c.JSON(http.StatusOK, oauthStartResponse(authURL, state))
}

func (h *Handler) RequestGitLabPATToken(c *gin.Context) {
	ctx := context.Background()
	ctx = PopulateAuthContext(ctx, c)

	var payload struct {
		BaseURL             string `json:"base_url"`
		PersonalAccessToken string `json:"personal_access_token"`
		Token               string `json:"token"`
	}
	if err := c.ShouldBindJSON(&payload); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "invalid body"})
		return
	}

	baseURL := gitlabauth.NormalizeBaseURL(strings.TrimSpace(payload.BaseURL))
	if baseURL == "" {
		baseURL = gitLabBaseURLFromRequest(nil)
	}
	pat := strings.TrimSpace(payload.PersonalAccessToken)
	if pat == "" {
		pat = strings.TrimSpace(payload.Token)
	}
	if pat == "" {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "personal_access_token is required"})
		return
	}

	authClient := gitlabauth.NewAuthClient(h.cfg)

	user, err := authClient.GetCurrentUser(ctx, baseURL, pat)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": err.Error()})
		return
	}
	patSelf, err := authClient.GetPersonalAccessTokenSelf(ctx, baseURL, pat)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": err.Error()})
		return
	}
	direct, err := authClient.FetchDirectAccess(ctx, baseURL, pat)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": err.Error()})
		return
	}

	identifier := gitLabAccountIdentifier(user)
	fileName := fmt.Sprintf("gitlab-%s-pat.json", sanitizeGitLabFileName(identifier))
	metadata := buildGitLabAuthMetadata(baseURL, gitLabLoginModePAT, nil, direct)
	metadata["auth_kind"] = "personal_access_token"
	metadata["personal_access_token"] = pat
	metadata["token_preview"] = maskGitLabToken(pat)
	metadata["username"] = strings.TrimSpace(user.Username)
	if email := primaryGitLabEmail(user); email != "" {
		metadata["email"] = email
	}
	metadata["name"] = strings.TrimSpace(user.Name)
	if patSelf != nil {
		if name := strings.TrimSpace(patSelf.Name); name != "" {
			metadata["pat_name"] = name
		}
		if len(patSelf.Scopes) > 0 {
			metadata["pat_scopes"] = append([]string(nil), patSelf.Scopes...)
		}
	}

	record := &coreauth.Auth{
		ID:       fileName,
		Provider: "gitlab",
		FileName: fileName,
		Label:    identifier + " (PAT)",
		Metadata: metadata,
	}

	savedPath, err := h.saveTokenRecord(ctx, record)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "error": "failed to save authentication tokens"})
		return
	}

	response := gin.H{
		"status":      "ok",
		"saved_path":  savedPath,
		"username":    strings.TrimSpace(user.Username),
		"email":       primaryGitLabEmail(user),
		"token_label": identifier,
	}
	if direct != nil && direct.ModelDetails != nil {
		if provider := strings.TrimSpace(direct.ModelDetails.ModelProvider); provider != "" {
			response["model_provider"] = provider
		}
		if model := strings.TrimSpace(direct.ModelDetails.ModelName); model != "" {
			response["model_name"] = model
		}
	}

	fmt.Printf("GitLab Duo PAT authentication successful. Token saved to %s\n", savedPath)
	c.JSON(http.StatusOK, response)
}

func (h *Handler) RequestGitHubToken(c *gin.Context) {
	ctx := context.Background()

	fmt.Println("Initializing GitHub Copilot authentication...")

	state := fmt.Sprintf("gh-%d", time.Now().UnixNano())

	// Initialize Copilot auth service
	deviceClient := copilot.NewDeviceFlowClient(h.cfg)

	// Initiate device flow
	deviceCode, err := deviceClient.RequestDeviceCode(ctx)
	if err != nil {
		log.Errorf("Failed to initiate device flow: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to initiate device flow"})
		return
	}

	authURL := deviceCode.VerificationURI
	userCode := deviceCode.UserCode

	RegisterOAuthSession(state, "github-copilot")

	go func() {
		fmt.Printf("Please visit %s and enter code: %s\n", authURL, userCode)

		tokenData, errPoll := deviceClient.PollForToken(ctx, deviceCode)
		if errPoll != nil {
			SetOAuthSessionError(state, "Authentication failed")
			fmt.Printf("Authentication failed: %v\n", errPoll)
			return
		}

		userInfo, errUser := deviceClient.FetchUserInfo(ctx, tokenData.AccessToken)
		if errUser != nil {
			log.Warnf("Failed to fetch user info: %v", errUser)
		}

		username := userInfo.Login
		if username == "" {
			username = "github-user"
		}

		tokenStorage := &copilot.CopilotTokenStorage{
			AccessToken: tokenData.AccessToken,
			TokenType:   tokenData.TokenType,
			Scope:       tokenData.Scope,
			Username:    username,
			Email:       userInfo.Email,
			Name:        userInfo.Name,
			Type:        "github-copilot",
		}

		fileName := fmt.Sprintf("github-copilot-%s.json", username)
		label := userInfo.Email
		if label == "" {
			label = username
		}
		metadata, errMeta := copilotTokenMetadata(tokenStorage)
		if errMeta != nil {
			log.Errorf("Failed to build token metadata: %v", errMeta)
			SetOAuthSessionError(state, "Failed to build token metadata")
			return
		}

		record := &coreauth.Auth{
			ID:       fileName,
			Provider: "github-copilot",
			Label:    label,
			FileName: fileName,
			Storage:  tokenStorage,
			Metadata: metadata,
		}

		savedPath, errSave := h.saveTokenRecord(ctx, record)
		if errSave != nil {
			log.Errorf("Failed to save authentication tokens: %v", errSave)
			SetOAuthSessionError(state, "Failed to save authentication tokens")
			return
		}

		fmt.Printf("Authentication successful! Token saved to %s\n", savedPath)
		fmt.Println("You can now use GitHub Copilot services through this CLI")
		CompleteOAuthSessionWithRecord(state, savedPath, record)
		CompleteOAuthSessionsByProvider("github-copilot")
	}()

	c.JSON(200, gin.H{
		"status":           "ok",
		"url":              authURL,
		"state":            state,
		"user_code":        userCode,
		"verification_uri": authURL,
	})
}

func copilotTokenMetadata(storage *copilot.CopilotTokenStorage) (map[string]any, error) {
	if storage == nil {
		return nil, fmt.Errorf("token storage is nil")
	}
	payload, errMarshal := json.Marshal(storage)
	if errMarshal != nil {
		return nil, fmt.Errorf("marshal token storage: %w", errMarshal)
	}
	metadata := make(map[string]any)
	if errUnmarshal := json.Unmarshal(payload, &metadata); errUnmarshal != nil {
		return nil, fmt.Errorf("unmarshal token storage: %w", errUnmarshal)
	}
	return metadata, nil
}

type projectSelectionRequiredError struct{}

func (e *projectSelectionRequiredError) Error() string {
	return "gemini cli: project selection required"
}

func ensureGeminiProjectAndOnboard(ctx context.Context, httpClient *http.Client, storage *geminiAuth.GeminiTokenStorage, requestedProject string) error {
	if storage == nil {
		return fmt.Errorf("gemini storage is nil")
	}

	trimmedRequest := strings.TrimSpace(requestedProject)
	if trimmedRequest == "" {
		projects, errProjects := fetchGCPProjects(ctx, httpClient)
		if errProjects != nil {
			return fmt.Errorf("fetch project list: %w", errProjects)
		}
		if len(projects) == 0 {
			return fmt.Errorf("no Google Cloud projects available for this account")
		}
		trimmedRequest = strings.TrimSpace(projects[0].ProjectID)
		if trimmedRequest == "" {
			return fmt.Errorf("resolved project id is empty")
		}
		storage.Auto = true
	} else {
		storage.Auto = false
	}

	if err := performGeminiCLISetup(ctx, httpClient, storage, trimmedRequest); err != nil {
		return err
	}

	if strings.TrimSpace(storage.ProjectID) == "" {
		storage.ProjectID = trimmedRequest
	}

	return nil
}

func onboardAllGeminiProjects(ctx context.Context, httpClient *http.Client, storage *geminiAuth.GeminiTokenStorage) ([]string, error) {
	projects, errProjects := fetchGCPProjects(ctx, httpClient)
	if errProjects != nil {
		return nil, fmt.Errorf("fetch project list: %w", errProjects)
	}
	if len(projects) == 0 {
		return nil, fmt.Errorf("no Google Cloud projects available for this account")
	}
	activated := make([]string, 0, len(projects))
	seen := make(map[string]struct{}, len(projects))
	for _, project := range projects {
		candidate := strings.TrimSpace(project.ProjectID)
		if candidate == "" {
			continue
		}
		if _, dup := seen[candidate]; dup {
			continue
		}
		if err := performGeminiCLISetup(ctx, httpClient, storage, candidate); err != nil {
			return nil, fmt.Errorf("onboard project %s: %w", candidate, err)
		}
		finalID := strings.TrimSpace(storage.ProjectID)
		if finalID == "" {
			finalID = candidate
		}
		activated = append(activated, finalID)
		seen[candidate] = struct{}{}
	}
	if len(activated) == 0 {
		return nil, fmt.Errorf("no Google Cloud projects available for this account")
	}
	return activated, nil
}

func ensureGeminiProjectsEnabled(ctx context.Context, httpClient *http.Client, projectIDs []string) error {
	for _, pid := range projectIDs {
		trimmed := strings.TrimSpace(pid)
		if trimmed == "" {
			continue
		}
		isChecked, errCheck := checkCloudAPIIsEnabled(ctx, httpClient, trimmed)
		if errCheck != nil {
			return fmt.Errorf("project %s: %w", trimmed, errCheck)
		}
		if !isChecked {
			return fmt.Errorf("project %s: Cloud AI API not enabled", trimmed)
		}
	}
	return nil
}

func performGeminiCLISetup(ctx context.Context, httpClient *http.Client, storage *geminiAuth.GeminiTokenStorage, requestedProject string) error {
	metadata := map[string]string{
		"ideType":    "IDE_UNSPECIFIED",
		"platform":   "PLATFORM_UNSPECIFIED",
		"pluginType": "GEMINI",
	}

	trimmedRequest := strings.TrimSpace(requestedProject)
	explicitProject := trimmedRequest != ""

	loadReqBody := map[string]any{
		"metadata": metadata,
	}
	if explicitProject {
		loadReqBody["cloudaicompanionProject"] = trimmedRequest
	}

	var loadResp map[string]any
	if errLoad := callGeminiCLI(ctx, httpClient, "loadCodeAssist", loadReqBody, &loadResp); errLoad != nil {
		return fmt.Errorf("load code assist: %w", errLoad)
	}

	tierID := "legacy-tier"
	if tiers, okTiers := loadResp["allowedTiers"].([]any); okTiers {
		for _, rawTier := range tiers {
			tier, okTier := rawTier.(map[string]any)
			if !okTier {
				continue
			}
			if isDefault, okDefault := tier["isDefault"].(bool); okDefault && isDefault {
				if id, okID := tier["id"].(string); okID && strings.TrimSpace(id) != "" {
					tierID = strings.TrimSpace(id)
					break
				}
			}
		}
	}

	projectID := trimmedRequest
	if projectID == "" {
		if id, okProject := loadResp["cloudaicompanionProject"].(string); okProject {
			projectID = strings.TrimSpace(id)
		}
		if projectID == "" {
			if projectMap, okProject := loadResp["cloudaicompanionProject"].(map[string]any); okProject {
				if id, okID := projectMap["id"].(string); okID {
					projectID = strings.TrimSpace(id)
				}
			}
		}
	}
	if projectID == "" {
		// Auto-discovery: try onboardUser without specifying a project
		// to let Google auto-provision one (matches Gemini CLI headless behavior
		// and Antigravity's FetchProjectID pattern).
		autoOnboardReq := map[string]any{
			"tierId":   tierID,
			"metadata": metadata,
		}

		autoCtx, autoCancel := context.WithTimeout(ctx, 30*time.Second)
		defer autoCancel()
		for attempt := 1; ; attempt++ {
			var onboardResp map[string]any
			if errOnboard := callGeminiCLI(autoCtx, httpClient, "onboardUser", autoOnboardReq, &onboardResp); errOnboard != nil {
				return fmt.Errorf("auto-discovery onboardUser: %w", errOnboard)
			}

			if done, okDone := onboardResp["done"].(bool); okDone && done {
				if resp, okResp := onboardResp["response"].(map[string]any); okResp {
					switch v := resp["cloudaicompanionProject"].(type) {
					case string:
						projectID = strings.TrimSpace(v)
					case map[string]any:
						if id, okID := v["id"].(string); okID {
							projectID = strings.TrimSpace(id)
						}
					}
				}
				break
			}

			log.Debugf("Auto-discovery: onboarding in progress, attempt %d...", attempt)
			select {
			case <-autoCtx.Done():
				return &projectSelectionRequiredError{}
			case <-time.After(2 * time.Second):
			}
		}

		if projectID == "" {
			return &projectSelectionRequiredError{}
		}
		log.Infof("Auto-discovered project ID via onboarding: %s", projectID)
	}

	onboardReqBody := map[string]any{
		"tierId":                  tierID,
		"metadata":                metadata,
		"cloudaicompanionProject": projectID,
	}

	storage.ProjectID = projectID

	for {
		var onboardResp map[string]any
		if errOnboard := callGeminiCLI(ctx, httpClient, "onboardUser", onboardReqBody, &onboardResp); errOnboard != nil {
			return fmt.Errorf("onboard user: %w", errOnboard)
		}

		if done, okDone := onboardResp["done"].(bool); okDone && done {
			responseProjectID := ""
			if resp, okResp := onboardResp["response"].(map[string]any); okResp {
				switch projectValue := resp["cloudaicompanionProject"].(type) {
				case map[string]any:
					if id, okID := projectValue["id"].(string); okID {
						responseProjectID = strings.TrimSpace(id)
					}
				case string:
					responseProjectID = strings.TrimSpace(projectValue)
				}
			}

			finalProjectID := projectID
			if responseProjectID != "" {
				if explicitProject && !strings.EqualFold(responseProjectID, projectID) {
					log.Infof("Gemini onboarding: requested project %s maps to backend project %s", projectID, responseProjectID)
					log.Infof("Using backend project ID: %s", responseProjectID)
				}
				finalProjectID = responseProjectID
			}

			storage.ProjectID = strings.TrimSpace(finalProjectID)
			if storage.ProjectID == "" {
				storage.ProjectID = strings.TrimSpace(projectID)
			}
			if storage.ProjectID == "" {
				return fmt.Errorf("onboard user completed without project id")
			}
			log.Infof("Onboarding complete. Using Project ID: %s", storage.ProjectID)
			return nil
		}

		log.Println("Onboarding in progress, waiting 5 seconds...")
		time.Sleep(5 * time.Second)
	}
}

func callGeminiCLI(ctx context.Context, httpClient *http.Client, endpoint string, body any, result any) error {
	endPointURL := fmt.Sprintf("%s/%s:%s", geminiCLIEndpoint, geminiCLIVersion, endpoint)
	if strings.HasPrefix(endpoint, "operations/") {
		endPointURL = fmt.Sprintf("%s/%s", geminiCLIEndpoint, endpoint)
	}

	var reader io.Reader
	if body != nil {
		rawBody, errMarshal := json.Marshal(body)
		if errMarshal != nil {
			return fmt.Errorf("marshal request body: %w", errMarshal)
		}
		reader = bytes.NewReader(rawBody)
	}

	req, errRequest := http.NewRequestWithContext(ctx, http.MethodPost, endPointURL, reader)
	if errRequest != nil {
		return fmt.Errorf("create request: %w", errRequest)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", misc.GeminiCLIUserAgent(""))

	resp, errDo := httpClient.Do(req)
	if errDo != nil {
		return fmt.Errorf("execute request: %w", errDo)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
	}()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("api request failed with status %d: %s", resp.StatusCode, strings.TrimSpace(string(bodyBytes)))
	}

	if result == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}

	if errDecode := json.NewDecoder(resp.Body).Decode(result); errDecode != nil {
		return fmt.Errorf("decode response body: %w", errDecode)
	}

	return nil
}

func fetchGCPProjects(ctx context.Context, httpClient *http.Client) ([]gcpProject, error) {
	req, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, "https://cloudresourcemanager.googleapis.com/v1/projects", nil)
	if errRequest != nil {
		return nil, fmt.Errorf("could not create project list request: %w", errRequest)
	}

	resp, errDo := httpClient.Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("failed to execute project list request: %w", errDo)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
	}()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("project list request failed with status %d: %s", resp.StatusCode, strings.TrimSpace(string(bodyBytes)))
	}

	var projects gcpProjectList
	if errDecode := json.NewDecoder(resp.Body).Decode(&projects); errDecode != nil {
		return nil, fmt.Errorf("failed to unmarshal project list: %w", errDecode)
	}

	return projects.Projects, nil
}

func checkCloudAPIIsEnabled(ctx context.Context, httpClient *http.Client, projectID string) (bool, error) {
	serviceUsageURL := "https://serviceusage.googleapis.com"
	requiredServices := []string{
		"cloudaicompanion.googleapis.com",
	}
	for _, service := range requiredServices {
		checkURL := fmt.Sprintf("%s/v1/projects/%s/services/%s", serviceUsageURL, projectID, service)
		req, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, checkURL, nil)
		if errRequest != nil {
			return false, fmt.Errorf("failed to create request: %w", errRequest)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", misc.GeminiCLIUserAgent(""))
		resp, errDo := httpClient.Do(req)
		if errDo != nil {
			return false, fmt.Errorf("failed to execute request: %w", errDo)
		}

		if resp.StatusCode == http.StatusOK {
			bodyBytes, _ := io.ReadAll(resp.Body)
			if gjson.GetBytes(bodyBytes, "state").String() == "ENABLED" {
				_ = resp.Body.Close()
				continue
			}
		}
		_ = resp.Body.Close()

		enableURL := fmt.Sprintf("%s/v1/projects/%s/services/%s:enable", serviceUsageURL, projectID, service)
		req, errRequest = http.NewRequestWithContext(ctx, http.MethodPost, enableURL, strings.NewReader("{}"))
		if errRequest != nil {
			return false, fmt.Errorf("failed to create request: %w", errRequest)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", misc.GeminiCLIUserAgent(""))
		resp, errDo = httpClient.Do(req)
		if errDo != nil {
			return false, fmt.Errorf("failed to execute request: %w", errDo)
		}

		bodyBytes, _ := io.ReadAll(resp.Body)
		errMessage := string(bodyBytes)
		errMessageResult := gjson.GetBytes(bodyBytes, "error.message")
		if errMessageResult.Exists() {
			errMessage = errMessageResult.String()
		}
		if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated {
			_ = resp.Body.Close()
			continue
		} else if resp.StatusCode == http.StatusBadRequest {
			_ = resp.Body.Close()
			if strings.Contains(strings.ToLower(errMessage), "already enabled") {
				continue
			}
		}
		_ = resp.Body.Close()
		return false, fmt.Errorf("project activation required: %s", errMessage)
	}
	return true, nil
}

func oauthExchangeFailureStatus(provider string, err error) string {
	provider = strings.TrimSpace(provider)
	if provider == "" {
		provider = "OAuth"
	}
	if err == nil {
		return provider + " OAuth token exchange failed"
	}
	message := strings.Join(strings.Fields(err.Error()), " ")
	if message == "" {
		return provider + " OAuth token exchange failed"
	}
	return provider + " OAuth token exchange failed: " + truncateTestMessagePreview(message, 280)
}

func (h *Handler) CancelOAuthSession(c *gin.Context) {
	state := strings.TrimSpace(c.Query("state"))
	if state == "" {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "state is required"})
		return
	}
	if err := ValidateOAuthState(state); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "invalid state"})
		return
	}

	cancelled := CancelOAuthSessionState(state)
	c.JSON(http.StatusOK, gin.H{
		"status":    "ok",
		"cancelled": cancelled,
	})
}

const kiroCallbackPort = 9876

func (h *Handler) RequestKiroToken(c *gin.Context) {
	ctx := context.Background()

	// Get the login method from query parameter (default: aws for device code flow)
	method := strings.ToLower(strings.TrimSpace(c.Query("method")))
	if method == "" {
		method = "aws"
	}

	fmt.Println("Initializing Kiro authentication...")

	state := fmt.Sprintf("kiro-%d", time.Now().UnixNano())

	switch method {
	case "aws", "builder-id":
		RegisterOAuthSession(state, "kiro")

		// AWS Builder ID uses device code flow (no callback needed)
		go func() {
			ssoClient := kiroauth.NewSSOOIDCClient(h.cfg)

			// Step 1: Register client
			fmt.Println("Registering client...")
			regResp, errRegister := ssoClient.RegisterClient(ctx)
			if errRegister != nil {
				log.Errorf("Failed to register client: %v", errRegister)
				SetOAuthSessionError(state, "Failed to register client")
				return
			}

			// Step 2: Start device authorization
			fmt.Println("Starting device authorization...")
			authResp, errAuth := ssoClient.StartDeviceAuthorization(ctx, regResp.ClientID, regResp.ClientSecret)
			if errAuth != nil {
				log.Errorf("Failed to start device auth: %v", errAuth)
				SetOAuthSessionError(state, "Failed to start device authorization")
				return
			}

			// Store the verification URL for the frontend to display.
			// Using "|" as separator because URLs contain ":".
			SetOAuthSessionError(state, "device_code|"+authResp.VerificationURIComplete+"|"+authResp.UserCode)

			// Step 3: Poll for token
			fmt.Println("Waiting for authorization...")
			interval := 5 * time.Second
			if authResp.Interval > 0 {
				interval = time.Duration(authResp.Interval) * time.Second
			}
			deadline := time.Now().Add(time.Duration(authResp.ExpiresIn) * time.Second)

			for time.Now().Before(deadline) {
				select {
				case <-ctx.Done():
					SetOAuthSessionError(state, "Authorization cancelled")
					return
				case <-time.After(interval):
					tokenResp, errToken := ssoClient.CreateToken(ctx, regResp.ClientID, regResp.ClientSecret, authResp.DeviceCode)
					if errToken != nil {
						errStr := errToken.Error()
						if strings.Contains(errStr, "authorization_pending") {
							continue
						}
						if strings.Contains(errStr, "slow_down") {
							interval += 5 * time.Second
							continue
						}
						log.Errorf("Token creation failed: %v", errToken)
						SetOAuthSessionError(state, "Token creation failed")
						return
					}

					// Success! Save the token
					expiresAt := time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second)
					email := kiroauth.ExtractEmailFromJWT(tokenResp.AccessToken)

					idPart := kiroauth.SanitizeEmailForFilename(email)
					if idPart == "" {
						idPart = fmt.Sprintf("%d", time.Now().UnixNano()%100000)
					}

					now := time.Now()
					fileName := fmt.Sprintf("kiro-aws-%s.json", idPart)

					record := &coreauth.Auth{
						ID:       fileName,
						Provider: "kiro",
						FileName: fileName,
						Metadata: map[string]any{
							"type":          "kiro",
							"access_token":  tokenResp.AccessToken,
							"refresh_token": tokenResp.RefreshToken,
							"expires_at":    expiresAt.Format(time.RFC3339),
							"auth_method":   "builder-id",
							"provider":      "AWS",
							"client_id":     regResp.ClientID,
							"client_secret": regResp.ClientSecret,
							"email":         email,
							"last_refresh":  now.Format(time.RFC3339),
						},
					}

					savedPath, errSave := h.saveTokenRecord(ctx, record)
					if errSave != nil {
						log.Errorf("Failed to save authentication tokens: %v", errSave)
						SetOAuthSessionError(state, "Failed to save authentication tokens")
						return
					}

					fmt.Printf("Authentication successful! Token saved to %s\n", savedPath)
					if email != "" {
						fmt.Printf("Authenticated as: %s\n", email)
					}
					CompleteOAuthSessionWithRecord(state, savedPath, record)
					return
				}
			}

			SetOAuthSessionError(state, "Authorization timed out")
		}()

		// Return immediately with the state for polling
		c.JSON(http.StatusOK, gin.H{"status": "ok", "state": state, "method": "device_code"})

	case "google", "github":
		RegisterOAuthSession(state, "kiro")

		// Social auth uses protocol handler - for WEB UI we use a callback forwarder
		provider := "Google"
		if method == "github" {
			provider = "Github"
		}

		isWebUI := isWebUIRequest(c)
		var forwarder *callbackForwarder
		if isWebUI {
			targetURL, errTarget := h.managementCallbackURL("/kiro/callback")
			if errTarget != nil {
				log.WithError(errTarget).Error("failed to compute kiro callback target")
				c.JSON(http.StatusInternalServerError, gin.H{"error": "callback server unavailable"})
				return
			}
			var errStart error
			if forwarder, errStart = startCallbackForwarder(kiroCallbackPort, "kiro", targetURL); errStart != nil {
				log.WithError(errStart).Error("failed to start kiro callback forwarder")
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to start callback server"})
				return
			}
		}

		go func() {
			if isWebUI {
				defer stopCallbackForwarderInstance(kiroCallbackPort, forwarder)
			}

			socialClient := kiroauth.NewSocialAuthClient(h.cfg)

			// Generate PKCE codes
			codeVerifier, codeChallenge, errPKCE := generateKiroPKCE()
			if errPKCE != nil {
				log.Errorf("Failed to generate PKCE: %v", errPKCE)
				SetOAuthSessionError(state, "Failed to generate PKCE")
				return
			}

			// Build login URL
			authURL := fmt.Sprintf("%s/login?idp=%s&redirect_uri=%s&code_challenge=%s&code_challenge_method=S256&state=%s&prompt=select_account",
				"https://prod.us-east-1.auth.desktop.kiro.dev",
				provider,
				url.QueryEscape(kiroauth.KiroRedirectURI),
				codeChallenge,
				state,
			)

			// Store auth URL for frontend.
			// Using "|" as separator because URLs contain ":".
			SetOAuthSessionError(state, "auth_url|"+authURL)

			// Wait for callback file
			waitFile := filepath.Join(h.cfg.AuthDir, fmt.Sprintf(".oauth-kiro-%s.oauth", state))
			deadline := time.Now().Add(oauthCallbackWaitTimeout)

			for {
				if time.Now().After(deadline) {
					log.Error("oauth flow timed out")
					SetOAuthSessionError(state, "OAuth flow timed out")
					return
				}
				if data, errRead := os.ReadFile(waitFile); errRead == nil {
					var m map[string]string
					_ = json.Unmarshal(data, &m)
					_ = os.Remove(waitFile)
					if errStr := m["error"]; errStr != "" {
						log.Errorf("Authentication failed: %s", errStr)
						SetOAuthSessionError(state, "Authentication failed")
						return
					}
					if m["state"] != state {
						log.Errorf("State mismatch")
						SetOAuthSessionError(state, "State mismatch")
						return
					}
					code := m["code"]
					if code == "" {
						log.Error("No authorization code received")
						SetOAuthSessionError(state, "No authorization code received")
						return
					}

					// Exchange code for tokens
					tokenReq := &kiroauth.CreateTokenRequest{
						Code:         code,
						CodeVerifier: codeVerifier,
						RedirectURI:  kiroauth.KiroRedirectURI,
					}

					tokenResp, errToken := socialClient.CreateToken(ctx, tokenReq)
					if errToken != nil {
						log.Errorf("Failed to exchange code for tokens: %v", errToken)
						SetOAuthSessionError(state, "Failed to exchange code for tokens")
						return
					}

					// Save the token
					expiresIn := tokenResp.ExpiresIn
					if expiresIn <= 0 {
						expiresIn = 3600
					}
					expiresAt := time.Now().Add(time.Duration(expiresIn) * time.Second)
					email := kiroauth.ExtractEmailFromJWT(tokenResp.AccessToken)

					idPart := kiroauth.SanitizeEmailForFilename(email)
					if idPart == "" {
						idPart = fmt.Sprintf("%d", time.Now().UnixNano()%100000)
					}

					now := time.Now()
					fileName := fmt.Sprintf("kiro-%s-%s.json", strings.ToLower(provider), idPart)

					record := &coreauth.Auth{
						ID:       fileName,
						Provider: "kiro",
						FileName: fileName,
						Metadata: map[string]any{
							"type":          "kiro",
							"access_token":  tokenResp.AccessToken,
							"refresh_token": tokenResp.RefreshToken,
							"profile_arn":   tokenResp.ProfileArn,
							"expires_at":    expiresAt.Format(time.RFC3339),
							"auth_method":   "social",
							"provider":      provider,
							"email":         email,
							"last_refresh":  now.Format(time.RFC3339),
						},
					}

					savedPath, errSave := h.saveTokenRecord(ctx, record)
					if errSave != nil {
						log.Errorf("Failed to save authentication tokens: %v", errSave)
						SetOAuthSessionError(state, "Failed to save authentication tokens")
						return
					}

					fmt.Printf("Authentication successful! Token saved to %s\n", savedPath)
					if email != "" {
						fmt.Printf("Authenticated as: %s\n", email)
					}
					CompleteOAuthSessionWithRecord(state, savedPath, record)
					return
				}
				time.Sleep(500 * time.Millisecond)
			}
		}()

		c.JSON(http.StatusOK, gin.H{"status": "ok", "state": state, "method": "social"})

	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid method, use 'aws', 'google', or 'github'"})
	}
}

// generateKiroPKCE generates PKCE code verifier and challenge for Kiro OAuth.
func generateKiroPKCE() (verifier, challenge string, err error) {
	b := make([]byte, 32)
	if _, errRead := io.ReadFull(rand.Reader, b); errRead != nil {
		return "", "", fmt.Errorf("failed to generate random bytes: %w", errRead)
	}
	verifier = base64.RawURLEncoding.EncodeToString(b)

	h := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(h[:])

	return verifier, challenge, nil
}

func (h *Handler) RequestKiloToken(c *gin.Context) {
	ctx := context.Background()

	fmt.Println("Initializing Kilo authentication...")

	state := fmt.Sprintf("kil-%d", time.Now().UnixNano())
	kilocodeAuth := kilo.NewKiloAuth()

	resp, err := kilocodeAuth.InitiateDeviceFlow(ctx)
	if err != nil {
		log.Errorf("Failed to initiate device flow: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to initiate device flow"})
		return
	}

	RegisterOAuthSession(state, "kilo")

	go func() {
		fmt.Printf("Please visit %s and enter code: %s\n", resp.VerificationURL, resp.Code)

		status, err := kilocodeAuth.PollForToken(ctx, resp.Code)
		if err != nil {
			SetOAuthSessionError(state, "Authentication failed")
			fmt.Printf("Authentication failed: %v\n", err)
			return
		}

		profile, err := kilocodeAuth.GetProfile(ctx, status.Token)
		if err != nil {
			log.Warnf("Failed to fetch profile: %v", err)
			profile = &kilo.Profile{Email: status.UserEmail}
		}

		var orgID string
		if len(profile.Orgs) > 0 {
			orgID = profile.Orgs[0].ID
		}

		defaults, err := kilocodeAuth.GetDefaults(ctx, status.Token, orgID)
		if err != nil {
			defaults = &kilo.Defaults{}
		}

		ts := &kilo.KiloTokenStorage{
			Token:          status.Token,
			OrganizationID: orgID,
			Model:          defaults.Model,
			Email:          status.UserEmail,
			Type:           "kilo",
		}

		fileName := kilo.CredentialFileName(status.UserEmail)
		record := &coreauth.Auth{
			ID:       fileName,
			Provider: "kilo",
			FileName: fileName,
			Storage:  ts,
			Metadata: map[string]any{
				"email":           status.UserEmail,
				"organization_id": orgID,
				"model":           defaults.Model,
			},
		}

		savedPath, errSave := h.saveTokenRecord(ctx, record)
		if errSave != nil {
			log.Errorf("Failed to save authentication tokens: %v", errSave)
			SetOAuthSessionError(state, "Failed to save authentication tokens")
			return
		}

		fmt.Printf("Authentication successful! Token saved to %s\n", savedPath)
		CompleteOAuthSessionWithRecord(state, savedPath, record)
		CompleteOAuthSessionsByProvider("kilo")
	}()

	c.JSON(200, gin.H{
		"status":           "ok",
		"url":              resp.VerificationURL,
		"state":            state,
		"user_code":        resp.Code,
		"verification_uri": resp.VerificationURL,
	})
}
