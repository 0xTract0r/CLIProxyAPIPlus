package helps

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const (
	AccountRuntimeEvidenceType = "account-runtime"
	AccountRuntimeClaimScope   = "runtime-resolution-not-provider-observed"
	credentialAccountKind      = "api" + "_" + "key"
)

type AccountRuntimeEvidenceOptions struct {
	Timestamp     time.Time
	CorrelationID string
	BaseURLHost   string
	RequestHeader http.Header
}

type AccountRuntimeEvidence struct {
	EvidenceType       string                               `json:"evidence_type"`
	ClaimScope         string                               `json:"claim_scope"`
	Timestamp          string                               `json:"timestamp"`
	CorrelationID      string                               `json:"correlation_id,omitempty"`
	Provider           string                               `json:"provider"`
	AuthIDHash         string                               `json:"auth_id_hash,omitempty"`
	AccountHash        string                               `json:"account_hash,omitempty"`
	BaseURLHost        string                               `json:"base_url_host,omitempty"`
	RefreshEnabled     bool                                 `json:"refresh_enabled"`
	ManagedHeaders     AccountRuntimeManagedHeadersEvidence `json:"managed_headers"`
	TransportProfile   AccountRuntimeProfileEvidence        `json:"transport_profile"`
	TLSProfile         AccountRuntimeProfileEvidence        `json:"tls_profile"`
	HTTPVersion        AccountRuntimeHTTPVersionEvidence    `json:"http_version"`
	ProviderObserved   bool                                 `json:"provider_observed"`
	SecretValuesStored bool                                 `json:"secret_values_stored"`
}

type AccountRuntimeManagedHeadersEvidence struct {
	PolicyVersion string                              `json:"policy_version,omitempty"`
	Source        string                              `json:"source,omitempty"`
	SourceURL     string                              `json:"source_url,omitempty"`
	CheckedAt     string                              `json:"checked_at,omitempty"`
	Version       string                              `json:"version,omitempty"`
	Strategy      string                              `json:"strategy,omitempty"`
	Headers       []AccountRuntimeManagedHeaderDigest `json:"headers,omitempty"`
}

type AccountRuntimeManagedHeaderDigest struct {
	Name        string `json:"name"`
	ValueSHA256 string `json:"value_sha256"`
}

type AccountRuntimeProfileEvidence struct {
	Configured      bool     `json:"configured"`
	Provider        string   `json:"provider,omitempty"`
	Family          string   `json:"family,omitempty"`
	ProfileID       string   `json:"profile_id"`
	RuntimeEnforced bool     `json:"runtime_enforced"`
	Status          string   `json:"status"`
	ALPN            []string `json:"alpn,omitempty"`
	ForceHTTP11     bool     `json:"force_http11,omitempty"`
}

type AccountRuntimeHTTPVersionEvidence struct {
	Version     string   `json:"version"`
	Policy      string   `json:"policy"`
	ALPN        []string `json:"alpn,omitempty"`
	ForceHTTP11 bool     `json:"force_http11,omitempty"`
}

func BuildAccountRuntimeEvidence(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, opts AccountRuntimeEvidenceOptions) AccountRuntimeEvidence {
	now := opts.Timestamp
	if now.IsZero() {
		now = time.Now().UTC()
	}

	provider := providerFromAuth(auth)
	baseURLHost := normalizeRuntimeTransportBaseURLHost(opts.BaseURLHost)
	if baseURLHost == "" {
		baseURLHost = RuntimeTransportHostFromContext(ctx)
	}
	if baseURLHost == "" {
		baseURLHost = runtimeTransportBaseURLHost(auth)
	}
	correlationID := strings.TrimSpace(opts.CorrelationID)
	if correlationID == "" {
		correlationID = correlationIDFromContext(ctx, opts.RequestHeader)
	}

	profile := ResolveRuntimeTransportProfile(auth)
	transportEvidence := accountRuntimeTransportEvidence(provider, profile)
	tlsEvidence := accountRuntimeTLSEvidence(provider, profile)

	return AccountRuntimeEvidence{
		EvidenceType:       AccountRuntimeEvidenceType,
		ClaimScope:         AccountRuntimeClaimScope,
		Timestamp:          now.UTC().Format(time.RFC3339Nano),
		CorrelationID:      correlationID,
		Provider:           provider,
		AuthIDHash:         hashNonEmpty(authIdentitySeed(auth)),
		AccountHash:        hashNonEmpty(accountIdentitySeed(auth)),
		BaseURLHost:        baseURLHost,
		RefreshEnabled:     auth == nil || !auth.RefreshDisabled(),
		ManagedHeaders:     accountRuntimeManagedHeadersEvidence(auth, cfg, opts.RequestHeader),
		TransportProfile:   transportEvidence,
		TLSProfile:         tlsEvidence,
		HTTPVersion:        accountRuntimeHTTPVersionEvidence(profile),
		ProviderObserved:   false,
		SecretValuesStored: false,
	}
}

func BuildAccountRuntimeEvidenceJSON(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, opts AccountRuntimeEvidenceOptions) ([]byte, error) {
	evidence := BuildAccountRuntimeEvidence(ctx, cfg, auth, opts)
	return json.MarshalIndent(evidence, "", "  ")
}

func LogAccountRuntimeEvidence(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, opts AccountRuntimeEvidenceOptions) {
	payload, errMarshal := BuildAccountRuntimeEvidenceJSON(ctx, cfg, auth, opts)
	if errMarshal != nil {
		log.WithError(errMarshal).Debug("failed to build account runtime evidence")
		return
	}
	LogWithRequestID(ctx).WithField("account_runtime_evidence", string(payload)).Debug("account runtime evidence resolved")
}

func accountRuntimeManagedHeadersEvidence(auth *cliproxyauth.Auth, cfg *config.Config, requestHeaders http.Header) AccountRuntimeManagedHeadersEvidence {
	provider := providerFromAuth(auth)
	strategy := "core-managed/default"
	if cliproxyauth.HasStructuredAccountSettingsMetadata(auth) {
		strategy = "core-managed/structured-account-settings"
	}

	switch provider {
	case "codex":
		profile := ResolveCodexClientProfile(auth, requestHeaders, cfg)
		return AccountRuntimeManagedHeadersEvidence{
			PolicyVersion: "codex-managed/v2",
			Source:        profile.Source.Source,
			SourceURL:     profile.Source.SourceURL,
			CheckedAt:     profile.Source.CheckedAt,
			Version:       profile.Version,
			Strategy:      strategy,
			Headers:       digestManagedHeaders(CodexManagedHeaders(profile)),
		}
	case "claude":
		profile := ResolveClaudeDeviceProfile(auth, "", requestHeaders, cfg)
		headers := map[string]string{
			"User-Agent":                  profile.UserAgent,
			"X-App":                       "cli",
			"X-Stainless-Package-Version": profile.PackageVersion,
			"X-Stainless-Runtime-Version": profile.RuntimeVersion,
			"X-Stainless-Timeout":         claudeTimeout(cfg),
		}
		return AccountRuntimeManagedHeadersEvidence{
			PolicyVersion: "claude-managed/v2",
			Source:        profile.Source.Source,
			SourceURL:     profile.Source.SourceURL,
			CheckedAt:     profile.Source.CheckedAt,
			Version:       claudeManagedHeaderVersion(profile),
			Strategy:      strategy,
			Headers:       digestManagedHeaders(headers),
		}
	default:
		return AccountRuntimeManagedHeadersEvidence{
			PolicyVersion: "managed/v2",
			Strategy:      strategy,
		}
	}
}

func accountRuntimeTransportEvidence(provider string, profile *RuntimeTransportProfile) AccountRuntimeProfileEvidence {
	if profile == nil || !profile.TransportConfigured {
		return AccountRuntimeProfileEvidence{
			Configured:      false,
			Provider:        provider,
			Family:          "go-http-transport",
			ProfileID:       "current-fallback",
			RuntimeEnforced: false,
			Status:          "transport_profile is not configured; using current core fallback transport",
		}
	}
	profileID := profile.ProfileID
	if profileID == "" {
		profileID = "provider-default"
	}
	return AccountRuntimeProfileEvidence{
		Configured:      true,
		Provider:        profile.Provider,
		Family:          profile.Family,
		ProfileID:       profileID,
		RuntimeEnforced: profile.SupportsTransportRuntime(),
		Status:          profile.TransportStatus,
		ALPN:            cloneSortedStrings(profile.ALPN),
		ForceHTTP11:     profile.ForceHTTP11,
	}
}

func accountRuntimeTLSEvidence(provider string, profile *RuntimeTransportProfile) AccountRuntimeProfileEvidence {
	if profile == nil || !profile.TLSConfigured {
		return AccountRuntimeProfileEvidence{
			Configured:      false,
			Provider:        provider,
			Family:          defaultTLSFallbackFamily(provider),
			ProfileID:       "current-fallback",
			RuntimeEnforced: false,
			Status:          "tls_profile is not configured; using current core fallback TLS behavior",
		}
	}
	profileID := profile.TLSProfileID
	if profileID == "" {
		profileID = "provider-default"
	}
	return AccountRuntimeProfileEvidence{
		Configured:      true,
		Provider:        profile.Provider,
		Family:          profile.TLSFamily,
		ProfileID:       profileID,
		RuntimeEnforced: profile.SupportsTLSRuntime(),
		Status:          profile.TLSStatus,
		ALPN:            cloneSortedStrings(profile.ALPN),
		ForceHTTP11:     profile.ForceHTTP11,
	}
}

func accountRuntimeHTTPVersionEvidence(profile *RuntimeTransportProfile) AccountRuntimeHTTPVersionEvidence {
	if profile != nil && profile.ForceHTTP11 {
		return AccountRuntimeHTTPVersionEvidence{
			Version:     "http/1.1",
			Policy:      "force_http11",
			ALPN:        []string{"http/1.1"},
			ForceHTTP11: true,
		}
	}
	if profile != nil && len(profile.ALPN) > 0 {
		alpn := cloneSortedStrings(profile.ALPN)
		switch {
		case containsStringFold(alpn, "h2") && containsStringFold(alpn, "http/1.1"):
			return AccountRuntimeHTTPVersionEvidence{Version: "h2-or-http/1.1", Policy: "alpn", ALPN: alpn}
		case containsStringFold(alpn, "h2"):
			return AccountRuntimeHTTPVersionEvidence{Version: "h2", Policy: "alpn", ALPN: alpn}
		case containsStringFold(alpn, "http/1.1"):
			return AccountRuntimeHTTPVersionEvidence{Version: "http/1.1", Policy: "alpn", ALPN: alpn}
		default:
			return AccountRuntimeHTTPVersionEvidence{Version: "profile-alpn", Policy: "alpn", ALPN: alpn}
		}
	}
	return AccountRuntimeHTTPVersionEvidence{
		Version: "go-default-auto",
		Policy:  "current-fallback",
	}
}

func digestManagedHeaders(headers map[string]string) []AccountRuntimeManagedHeaderDigest {
	headers = normalizeHeaderMap(headers)
	if len(headers) == 0 {
		return nil
	}
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	sort.Strings(names)
	digests := make([]AccountRuntimeManagedHeaderDigest, 0, len(names))
	for _, name := range names {
		digests = append(digests, AccountRuntimeManagedHeaderDigest{
			Name:        name,
			ValueSHA256: sha256Hex(headers[name]),
		})
	}
	return digests
}

func providerFromAuth(auth *cliproxyauth.Auth) string {
	if auth == nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(auth.Provider))
}

func authIdentitySeed(auth *cliproxyauth.Auth) string {
	if auth == nil {
		return ""
	}
	for _, value := range []string{auth.ID, auth.FileName, auth.Label} {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func accountIdentitySeed(auth *cliproxyauth.Auth) string {
	if auth == nil {
		return ""
	}
	if accountType, accountValue := auth.AccountInfo(); strings.TrimSpace(accountValue) != "" && !strings.EqualFold(strings.TrimSpace(accountType), credentialAccountKind) {
		return strings.TrimSpace(accountType) + ":" + strings.TrimSpace(accountValue)
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

func hashNonEmpty(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	return sha256Hex(value)
}

func sha256Hex(value string) string {
	sum := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func claudeTimeout(cfg *config.Config) string {
	if cfg != nil {
		if timeout := strings.TrimSpace(cfg.ClaudeHeaderDefaults.Timeout); timeout != "" {
			return timeout
		}
	}
	return "600"
}

func claudeManagedHeaderVersion(profile ClaudeDeviceProfile) string {
	if version, ok := parseClaudeCLIVersion(profile.UserAgent); ok {
		return strings.Join([]string{
			intToString(version.major),
			intToString(version.minor),
			intToString(version.patch),
		}, ".")
	}
	if strings.TrimSpace(profile.PackageVersion) != "" {
		return strings.TrimSpace(profile.PackageVersion)
	}
	return ""
}

func intToString(value int) string {
	return strconv.Itoa(value)
}

func defaultTLSFallbackFamily(provider string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "claude":
		return "go-tls-or-utls-default"
	case "codex":
		return "go-tls-default"
	default:
		return "go-tls-default"
	}
}

func cloneSortedStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	out := append([]string(nil), values...)
	sort.Strings(out)
	return out
}

func correlationIDFromContext(ctx context.Context, headers http.Header) string {
	if requestID := strings.TrimSpace(logging.GetRequestID(ctx)); requestID != "" {
		return requestID
	}
	if ctx != nil {
		if ginCtx, ok := ctx.Value("gin").(*gin.Context); ok && ginCtx != nil {
			if requestID := strings.TrimSpace(logging.GetGinRequestID(ginCtx)); requestID != "" {
				return requestID
			}
			if ginCtx.Request != nil {
				if requestID := firstHeaderValue(ginCtx.Request.Header, "X-Request-Id", "X-Request-ID", "X-Client-Request-Id"); requestID != "" {
					return requestID
				}
			}
		}
	}
	return firstHeaderValue(headers, "X-Request-Id", "X-Request-ID", "X-Client-Request-Id")
}

func firstHeaderValue(headers http.Header, names ...string) string {
	for _, name := range names {
		if value := strings.TrimSpace(headers.Get(name)); value != "" {
			return value
		}
	}
	return ""
}
