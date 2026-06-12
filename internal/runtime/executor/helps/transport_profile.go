package helps

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// RuntimeTransportProfile describes the minimal runtime transport contract
// derived from account_settings.transport_profile.
type RuntimeTransportProfile struct {
	Provider            string   `json:"provider"`
	Family              string   `json:"family"`
	ProfileID           string   `json:"profile_id"`
	TransportConfigured bool     `json:"transport_configured"`
	TLSFamily           string   `json:"tls_family"`
	TLSProfileID        string   `json:"tls_profile_id"`
	TLSConfigured       bool     `json:"tls_configured"`
	Source              string   `json:"source,omitempty"`
	CoreManaged         bool     `json:"core_managed"`
	ALPN                []string `json:"alpn,omitempty"`
	ForceHTTP11         bool     `json:"force_http11"`
	TransportStatus     string   `json:"transport_status"`
	TLSStatus           string   `json:"tls_status"`
	ProviderMismatch    bool     `json:"provider_mismatch"`
}

type runtimeTransportHostContextKey struct{}

func WithRuntimeTransportHost(ctx context.Context, rawHost string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	host := normalizeRuntimeTransportBaseURLHost(rawHost)
	if host == "" {
		return ctx
	}
	return context.WithValue(ctx, runtimeTransportHostContextKey{}, host)
}

func WithRuntimeTransportHostFromRequest(ctx context.Context, req *http.Request) context.Context {
	if req == nil || req.URL == nil {
		return ctx
	}
	if host := req.URL.Hostname(); host != "" {
		return WithRuntimeTransportHost(ctx, host)
	}
	return WithRuntimeTransportHost(ctx, req.URL.Host)
}

func RuntimeTransportHostFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if host, ok := ctx.Value(runtimeTransportHostContextKey{}).(string); ok {
		return normalizeRuntimeTransportBaseURLHost(host)
	}
	return ""
}

func ResolveRuntimeTransportProfile(auth *cliproxyauth.Auth) *RuntimeTransportProfile {
	provider := strings.ToLower(strings.TrimSpace(""))
	if auth != nil {
		provider = strings.ToLower(strings.TrimSpace(auth.Provider))
	}

	var settings map[string]any
	if auth != nil && len(auth.Metadata) > 0 {
		settings = normalizeObject(auth.Metadata["account_settings"])
	}
	if len(settings) == 0 {
		return coreManagedRuntimeTransportProfile(provider)
	}
	profileMap := normalizeObject(settings["transport_profile"])
	tlsMap := normalizeObject(settings["tls_profile"])
	if len(profileMap) == 0 && len(tlsMap) == 0 {
		return coreManagedRuntimeTransportProfile(provider)
	}

	profileID := firstNonEmptyString(profileMap["profile_id"], profileMap["preset"])
	tlsProfileID := firstNonEmptyString(tlsMap["profile_id"], tlsMap["preset"], tlsMap["client_hello"])
	configuredProvider := strings.ToLower(strings.TrimSpace(firstNonEmptyString(profileMap["provider"], tlsMap["provider"])))
	authProvider := strings.ToLower(strings.TrimSpace(auth.Provider))
	provider = authProvider
	if provider == "" {
		provider = configuredProvider
	}
	providerMismatch := configuredProvider != "" && authProvider != "" && configuredProvider != authProvider
	family := strings.ToLower(strings.TrimSpace(firstNonEmptyString(profileMap["family"])))
	tlsFamily := strings.ToLower(strings.TrimSpace(firstNonEmptyString(tlsMap["family"])))
	canonicalProfileID := canonicalRuntimeProfileID(provider, profileID)
	canonicalTLSProfileID := canonicalRuntimeProfileID(provider, tlsProfileID)
	if tlsProfileID == "" && profileID != "" && provider == "claude" {
		tlsProfileID = profileID
		canonicalTLSProfileID = canonicalProfileID
	} else if tlsProfileID == "" && profileID != "" && provider == "codex" && isCodexProxyCommunityProfile(profileID) {
		tlsProfileID = profileID
		canonicalTLSProfileID = canonicalProfileID
	}
	if family == "" && provider == "claude" && strings.EqualFold(strings.TrimSpace(profileID), "provider-default") {
		family = "cli-native"
	} else if family == "" && provider == "claude" && isClaudeReqwestCommunityProfile(canonicalProfileID) {
		family = "claude-reqwest-compatible"
	} else if family == "" && provider == "claude" && profileID != "" {
		family = "utls"
	} else if family == "" && provider == "codex" && isCodexProxyCommunityProfile(profileID) {
		family = "codex-proxy-compatible"
	} else if family == "" && provider == "codex" && profileID != "" {
		family = "standard"
	} else if family == "" && (provider == "gemini" || provider == "gemini-cli") && profileID != "" {
		family = "cli-native"
	}
	if tlsFamily == "" && provider == "claude" && strings.EqualFold(strings.TrimSpace(tlsProfileID), "provider-default") {
		tlsFamily = "runtime-native"
	} else if tlsFamily == "" && provider == "claude" && isClaudeReqwestCommunityProfile(canonicalTLSProfileID) {
		tlsFamily = "rustls-compatible"
	} else if tlsFamily == "" && provider == "claude" && tlsProfileID != "" {
		tlsFamily = "utls"
	} else if tlsFamily == "" && provider == "codex" && isCodexProxyCommunityProfile(tlsProfileID) {
		tlsFamily = "rustls-compatible"
	} else if tlsFamily == "" && provider == "codex" && tlsProfileID != "" {
		tlsFamily = "go-tls"
	} else if tlsFamily == "" && (provider == "gemini" || provider == "gemini-cli") && tlsProfileID != "" {
		tlsFamily = "runtime-native"
	}

	alpn := normalizeStringSlice(profileMap["alpn"])
	if len(alpn) == 0 {
		alpn = normalizeStringSlice(tlsMap["alpn"])
	}

	forceHTTP11 := normalizeBool(tlsMap["force_http11"]) ||
		strings.EqualFold(strings.TrimSpace(firstNonEmptyString(tlsMap["http_version"])), "1.1") ||
		strings.EqualFold(strings.TrimSpace(firstNonEmptyString(tlsMap["http_version"])), "http/1.1") ||
		codexTLSProfileForcesHTTP11(provider, tlsProfileID)
	if forceHTTP11 && provider == "codex" {
		alpn = []string{"http/1.1"}
	}

	profile := &RuntimeTransportProfile{
		Provider:            provider,
		Family:              family,
		ProfileID:           canonicalProfileID,
		TransportConfigured: len(profileMap) > 0,
		TLSFamily:           tlsFamily,
		TLSProfileID:        canonicalTLSProfileID,
		TLSConfigured:       len(tlsMap) > 0,
		Source:              "explicit_account_profile",
		ALPN:                alpn,
		ForceHTTP11:         forceHTTP11,
		ProviderMismatch:    providerMismatch,
	}
	profile.TransportStatus = profile.runtimeTransportStatus()
	profile.TLSStatus = profile.runtimeTLSStatus()
	return profile
}

func coreManagedRuntimeTransportProfile(provider string) *RuntimeTransportProfile {
	provider = normalizeCoreManagedRuntimeProvider(provider)
	if provider == "" {
		return nil
	}
	profileID := provider + "_cli_native_v1"
	switch provider {
	case "claude":
		// Default claude->anthropic outbound replicates the real claude-cli
		// (Node/OpenSSL) ClientHello via uTLS HelloCustom + ALPN http/1.1
		// (resolveClaudeClientHelloID -> HelloCustom). This is the no-tls_profile
		// default; a per-account tls_profile still overrides it (handled in
		// ResolveRuntimeTransportProfile before this function is reached).
		profileID = claudeCLIClientHelloProfileID
	case "codex":
		profileID = "codex_proxy_compatible_v1"
	case "gemini-cli":
		profileID = "gemini_cli_native_v1"
	}
	family := "cli-native"
	tlsFamily := "runtime-native"
	if provider == "claude" {
		family = "utls"
		tlsFamily = "utls"
	} else if provider == "codex" {
		family = "codex-proxy-compatible"
		tlsFamily = "rustls-compatible"
	}
	profile := &RuntimeTransportProfile{
		Provider:            provider,
		Family:              family,
		ProfileID:           profileID,
		TransportConfigured: true,
		TLSFamily:           tlsFamily,
		TLSProfileID:        profileID,
		TLSConfigured:       true,
		Source:              "core-managed-account-runtime",
		CoreManaged:         true,
	}
	profile.TransportStatus = profile.runtimeTransportStatus()
	profile.TLSStatus = profile.runtimeTLSStatus()
	return profile
}

func normalizeCoreManagedRuntimeProvider(provider string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "claude", "codex", "gemini", "gemini-cli":
		return strings.ToLower(strings.TrimSpace(provider))
	default:
		return ""
	}
}

func canonicalRuntimeProfileID(provider string, profileID string) string {
	id := strings.ToLower(strings.TrimSpace(profileID))
	if strings.EqualFold(strings.TrimSpace(provider), "claude") {
		return canonicalClaudeRuntimeProfileID(id)
	}
	return id
}

func canonicalClaudeRuntimeProfileID(profileID string) string {
	switch strings.ToLower(strings.TrimSpace(profileID)) {
	case "claude_chrome_like_mac_v3", "chrome_133":
		return "claude_utls_chrome_133"
	case "claude_code_cli_v1", "claw_code_reqwest_rustls_v1", "claude_reqwest_rustls_v1":
		return "claude_reqwest_rustls_compatible_v1"
	default:
		return strings.ToLower(strings.TrimSpace(profileID))
	}
}

func isClaudeReqwestCommunityProfile(profileID string) bool {
	switch strings.ToLower(strings.TrimSpace(profileID)) {
	case "claude_reqwest_rustls_compatible_v1":
		return true
	default:
		return false
	}
}

func codexTLSProfileForcesHTTP11(provider string, tlsProfileID string) bool {
	return strings.EqualFold(strings.TrimSpace(provider), "codex") &&
		strings.EqualFold(strings.TrimSpace(tlsProfileID), "codex_go_http11_v1")
}

func isCodexProxyCommunityProfile(profileID string) bool {
	switch strings.ToLower(strings.TrimSpace(profileID)) {
	case "codex_proxy_compatible_v1", "codex_rustls_native_v1":
		return true
	default:
		return false
	}
}

func IsRuntimeTransportProfileEnforced(auth *cliproxyauth.Auth) bool {
	profile := ResolveRuntimeTransportProfile(auth)
	return profile != nil && profile.SupportsTransportRuntime()
}

func IsRuntimeTLSProfileEnforced(auth *cliproxyauth.Auth) bool {
	profile := ResolveRuntimeTransportProfile(auth)
	return profile != nil && profile.SupportsTLSRuntime()
}

func IsRuntimeProfileEnforced(auth *cliproxyauth.Auth) bool {
	profile := ResolveRuntimeTransportProfile(auth)
	return profile != nil && profile.SupportsRuntime()
}

func RuntimeTransportProfileStatus(auth *cliproxyauth.Auth) (bool, string) {
	profile := ResolveRuntimeTransportProfile(auth)
	if profile == nil {
		return false, "runtime transport profile is not configured"
	}
	messages := make([]string, 0, 2)
	if strings.TrimSpace(profile.TransportStatus) != "" {
		messages = append(messages, profile.TransportStatus)
	}
	if strings.TrimSpace(profile.TLSStatus) != "" {
		messages = append(messages, profile.TLSStatus)
	}
	return profile.SupportsRuntime(), strings.Join(messages, "; ")
}

func RuntimeTransportProfileCacheKey(proxyURL string, auth *cliproxyauth.Auth) string {
	return RuntimeTransportProfileCacheKeyForHost(proxyURL, runtimeTransportBaseURLHost(auth), auth)
}

func RuntimeTransportProfileCacheKeyForHost(proxyURL string, baseURLHost string, auth *cliproxyauth.Auth) string {
	profile := ResolveRuntimeTransportProfile(auth)
	if profile == nil || !profile.SupportsRuntime() || auth == nil {
		return ""
	}

	authID := strings.TrimSpace(auth.ID)
	if authID == "" {
		authID = strings.TrimSpace(auth.FileName)
	}
	if authID == "" {
		authID = strings.TrimSpace(auth.Label)
	}
	if authID == "" {
		authID = "anonymous"
	}

	return fmt.Sprintf(
		"transport:%s|auth=%s|account=%s|base=%s|proxy=%s|profile=%s",
		profile.Provider,
		authID,
		runtimeTransportAccountKey(auth),
		normalizeRuntimeTransportBaseURLHost(baseURLHost),
		strings.TrimSpace(proxyURL),
		profile.cacheToken(),
	)
}

func RuntimeTransportProfileToken(auth *cliproxyauth.Auth) string {
	profile := ResolveRuntimeTransportProfile(auth)
	if profile == nil || !profile.SupportsRuntime() {
		return ""
	}
	return profile.cacheToken()
}

func BuildRuntimeTransportRoundTripper(proxyURL string, auth *cliproxyauth.Auth) (http.RoundTripper, bool) {
	profile := ResolveRuntimeTransportProfile(auth)
	if profile == nil || !profile.SupportsRuntime() {
		return nil, false
	}

	switch profile.Provider {
	case "claude":
		if profile.isCLINativeProfile() {
			return standardTransportForProxy(proxyURL), true
		}
		if isClaudeReqwestCommunityProfile(profile.ProfileID) || isClaudeReqwestCommunityProfile(profile.TLSProfileID) {
			return NewClaudeReqwestCompatibleRoundTripperForProfile(proxyURL, profile.ProfileID, profile.ALPN), true
		}
		clientHelloProfile := profile.TLSProfileID
		if clientHelloProfile == "" {
			clientHelloProfile = profile.ProfileID
		}
		return NewUtlsRoundTripperForProfile(proxyURL, clientHelloProfile), true
	case "codex":
		if profile.isCLINativeProfile() {
			return standardTransportForProxy(proxyURL), true
		}
		return NewCodexTransportRoundTripperForProfile(proxyURL, profile.ProfileID, profile.ALPN, profile.ForceHTTP11), true
	case "gemini", "gemini-cli":
		if profile.isCLINativeProfile() {
			return standardTransportForProxy(proxyURL), true
		}
		return nil, false
	default:
		return nil, false
	}
}

func (p *RuntimeTransportProfile) SupportsRuntime() bool {
	return p != nil && (p.SupportsTransportRuntime() || p.SupportsTLSRuntime())
}

func (p *RuntimeTransportProfile) SupportsTransportRuntime() bool {
	if p == nil {
		return false
	}
	if p.ProviderMismatch {
		return false
	}
	if !p.TransportConfigured {
		return false
	}
	switch p.Provider {
	case "claude":
		if p.isCLINativeProfile() {
			return true
		}
		if p.Family != "" && p.Family != "utls" && p.Family != "claude-reqwest-compatible" {
			return false
		}
		switch p.ProfileID {
		case "provider-default",
			"claude_cli_native_v1",
			"claude_cli_clienthello_v1",
			"claude_reqwest_rustls_compatible_v1":
			return true
		case
			"claude_chrome_like_mac_v1",
			"claude_chrome_like_mac_v2",
			"claude_utls_chrome_133",
			"chrome_120",
			"chrome_131":
			return true
		default:
			return false
		}
	case "codex":
		if p.isCLINativeProfile() {
			return true
		}
		if p.Family != "" && p.Family != "standard" && p.Family != "codex-proxy-compatible" {
			return false
		}
		switch p.ProfileID {
		case "provider-default",
			"codex_proxy_compatible_v1",
			"codex_rustls_native_v1",
			"codex_isolated_transport_v1",
			"codex_managed_transport_v1":
			return true
		default:
			return false
		}
	case "gemini", "gemini-cli":
		return p.isCLINativeProfile()
	default:
		return false
	}
}

func (p *RuntimeTransportProfile) SupportsTLSRuntime() bool {
	if p == nil || !p.TLSConfigured {
		return false
	}
	if p.ProviderMismatch {
		return false
	}
	switch p.Provider {
	case "claude":
		if p.isCLINativeProfile() || p.TLSProfileID == "provider-default" {
			return true
		}
		if p.TLSFamily != "" && p.TLSFamily != "utls" && p.TLSFamily != "rustls-compatible" {
			return false
		}
		switch p.TLSProfileID {
		case "claude_cli_clienthello_v1",
			"claude_reqwest_rustls_compatible_v1":
			return true
		case
			"claude_chrome_like_mac_v1",
			"claude_chrome_like_mac_v2",
			"claude_utls_chrome_133",
			"chrome_120",
			"chrome_131":
			return true
		default:
			return false
		}
	case "codex":
		if p.isCLINativeProfile() {
			return true
		}
		if p.TLSFamily != "" && p.TLSFamily != "go-tls" && p.TLSFamily != "standard" && p.TLSFamily != "rustls-compatible" {
			return false
		}
		switch p.TLSProfileID {
		case "", "provider-default",
			"codex_proxy_compatible_v1",
			"codex_rustls_native_v1",
			"codex_go_managed_h2_v1",
			"codex_go_standard_h2_v1",
			"codex_go_http11_v1":
			return true
		default:
			return false
		}
	case "gemini", "gemini-cli":
		return p.isCLINativeProfile()
	default:
		return false
	}
}

func (p *RuntimeTransportProfile) isCLINativeProfile() bool {
	if p == nil {
		return false
	}
	if p.Family == "cli-native" || p.TLSFamily == "runtime-native" {
		return true
	}
	switch p.ProfileID {
	case "claude_cli_native_v1", "codex_cli_native_v1", "gemini_cli_native_v1":
		return true
	}
	switch p.TLSProfileID {
	case "claude_cli_native_v1", "codex_cli_native_v1", "gemini_cli_native_v1":
		return true
	}
	return false
}

func (p *RuntimeTransportProfile) cacheToken() string {
	if p == nil {
		return ""
	}
	alpn := append([]string(nil), p.ALPN...)
	sort.Strings(alpn)
	return fmt.Sprintf("%s|%s|%s|%s|%t|%s|%s", p.Family, p.ProfileID, p.TLSFamily, p.TLSProfileID, p.ForceHTTP11, strings.Join(alpn, ","), p.Source)
}

func (p *RuntimeTransportProfile) runtimeTransportStatus() string {
	if p == nil || !p.TransportConfigured {
		return ""
	}
	provider := strings.TrimSpace(p.Provider)
	if provider == "" {
		provider = "unknown provider"
	}
	profileID := strings.TrimSpace(p.ProfileID)
	if profileID == "" {
		profileID = "provider-default"
	}
	if p.ProviderMismatch {
		return fmt.Sprintf("%s transport_profile %q declares a different provider; falling back to default transport", provider, profileID)
	}
	if p.SupportsTransportRuntime() {
		if p.CoreManaged {
			if p.Provider == "claude" && p.Family == "claude-reqwest-compatible" {
				return fmt.Sprintf("%s core-managed Claude reqwest/rustls-compatible Go transport identity %q is runtime-enforced", provider, profileID)
			}
			if p.Provider == "codex" && p.Family == "codex-proxy-compatible" {
				return fmt.Sprintf("%s core-managed Codex-Proxy-compatible Go transport identity %q is runtime-enforced", provider, profileID)
			}
			return fmt.Sprintf("%s core-managed account transport identity %q is runtime-enforced", provider, profileID)
		}
		if p.isCLINativeProfile() {
			return fmt.Sprintf("%s transport_profile %q uses CLI-native account isolation", provider, profileID)
		}
		if p.Provider == "claude" && p.Family == "claude-reqwest-compatible" {
			return fmt.Sprintf("%s transport_profile %q uses Claude reqwest/rustls-compatible transport via Go approximation", provider, profileID)
		}
		return fmt.Sprintf("%s transport_profile %q is runtime-enforced", provider, profileID)
	}
	return fmt.Sprintf("%s transport_profile %q is unsupported; falling back to default transport", provider, profileID)
}

func (p *RuntimeTransportProfile) runtimeTLSStatus() string {
	if p == nil || !p.TLSConfigured {
		return ""
	}
	provider := strings.TrimSpace(p.Provider)
	if provider == "" {
		provider = "unknown provider"
	}
	profileID := strings.TrimSpace(p.TLSProfileID)
	if profileID == "" {
		profileID = "provider-default"
	}
	if p.ProviderMismatch {
		return fmt.Sprintf("%s tls_profile %q declares a different provider; falling back to default transport", provider, profileID)
	}
	if p.SupportsTLSRuntime() {
		if p.CoreManaged {
			if p.Provider == "claude" && p.TLSFamily == "rustls-compatible" {
				return fmt.Sprintf("%s core-managed Claude reqwest/rustls-compatible TLS target %q is runtime-enforced via Go approximation", provider, profileID)
			}
			if p.Provider == "codex" && p.TLSFamily == "rustls-compatible" {
				return fmt.Sprintf("%s core-managed Codex-Proxy-compatible TLS target %q is runtime-enforced via Go approximation", provider, profileID)
			}
			return fmt.Sprintf("%s core-managed account TLS identity %q is runtime-enforced", provider, profileID)
		}
		if p.isCLINativeProfile() || profileID == "provider-default" {
			return fmt.Sprintf("%s tls_profile %q uses CLI-native TLS behavior with account isolation", provider, profileID)
		}
		if p.Provider == "claude" && p.TLSFamily == "rustls-compatible" {
			return fmt.Sprintf("%s tls_profile %q targets Claude reqwest/rustls-compatible TLS via Go approximation", provider, profileID)
		}
		return fmt.Sprintf("%s tls_profile %q is runtime-enforced", provider, profileID)
	}
	return fmt.Sprintf("%s tls_profile %q is unsupported; falling back to default transport", provider, profileID)
}

func runtimeTransportAccountKey(auth *cliproxyauth.Auth) string {
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

func runtimeTransportBaseURLHost(auth *cliproxyauth.Auth) string {
	provider := ""
	baseURL := ""
	if auth != nil {
		provider = strings.ToLower(strings.TrimSpace(auth.Provider))
		if auth.Attributes != nil {
			baseURL = strings.TrimSpace(auth.Attributes["base_url"])
		}
	}
	if baseURL == "" {
		switch provider {
		case "claude":
			return "api.anthropic.com"
		case "codex":
			return "chatgpt.com"
		default:
			return ""
		}
	}
	return normalizeRuntimeTransportBaseURLHost(baseURL)
}

func normalizeRuntimeTransportBaseURLHost(baseURL string) string {
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		return ""
	}
	parsed, errParse := url.Parse(baseURL)
	if errParse != nil || parsed.Host == "" {
		parsed, errParse = url.Parse("https://" + strings.TrimLeft(baseURL, "/"))
	}
	if errParse != nil || parsed.Host == "" {
		return strings.ToLower(strings.TrimSpace(baseURL))
	}
	return strings.ToLower(parsed.Hostname())
}

func NewCodexTransportRoundTripperForProfile(proxyURL string, profileID string, alpn []string, forceHTTP11 bool) http.RoundTripper {
	_ = profileID

	base := standardTransportForProxy(proxyURL)
	transport, ok := base.(*http.Transport)
	if !ok || transport == nil {
		return base
	}

	cloned := transport.Clone()
	cloned.ForceAttemptHTTP2 = true
	cloned.MaxIdleConnsPerHost = 4
	cloned.MaxIdleConns = 16
	if forceHTTP11 {
		cloned.ForceAttemptHTTP2 = false
		cloned.TLSNextProto = make(map[string]func(authority string, c *tls.Conn) http.RoundTripper)
		if cloned.TLSClientConfig == nil {
			cloned.TLSClientConfig = &tls.Config{}
		}
		cloned.TLSClientConfig.NextProtos = []string{"http/1.1"}
		return cloned
	}
	if len(alpn) > 0 {
		if cloned.TLSClientConfig == nil {
			cloned.TLSClientConfig = &tls.Config{}
		}
		cloned.TLSClientConfig.NextProtos = append([]string(nil), alpn...)
		cloned.ForceAttemptHTTP2 = containsStringFold(alpn, "h2")
	}
	return cloned
}

func NewClaudeReqwestCompatibleRoundTripperForProfile(proxyURL string, profileID string, alpn []string) http.RoundTripper {
	_ = profileID

	base := standardTransportForProxy(proxyURL)
	transport, ok := base.(*http.Transport)
	if !ok || transport == nil {
		return base
	}

	cloned := transport.Clone()
	cloned.ForceAttemptHTTP2 = true
	cloned.MaxIdleConnsPerHost = 4
	cloned.MaxIdleConns = 16
	if len(alpn) > 0 {
		if cloned.TLSClientConfig == nil {
			cloned.TLSClientConfig = &tls.Config{}
		}
		cloned.TLSClientConfig.NextProtos = append([]string(nil), alpn...)
		cloned.ForceAttemptHTTP2 = containsStringFold(alpn, "h2")
	}
	return cloned
}

func normalizeBool(raw any) bool {
	switch value := raw.(type) {
	case bool:
		return value
	case string:
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "1", "true", "yes", "y", "on":
			return true
		default:
			return false
		}
	case float64:
		return value != 0
	case int:
		return value != 0
	default:
		return false
	}
}

func containsStringFold(values []string, want string) bool {
	for _, value := range values {
		if strings.EqualFold(strings.TrimSpace(value), want) {
			return true
		}
	}
	return false
}

func normalizeObject(raw any) map[string]any {
	if raw == nil {
		return nil
	}
	switch value := raw.(type) {
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
			out[key] = item
		}
		return out
	default:
		data, errMarshal := json.Marshal(raw)
		if errMarshal != nil || len(data) == 0 {
			return nil
		}
		var out map[string]any
		if errUnmarshal := json.Unmarshal(data, &out); errUnmarshal != nil || len(out) == 0 {
			return nil
		}
		return out
	}
}

func firstNonEmptyString(values ...any) string {
	for _, raw := range values {
		if text, ok := raw.(string); ok {
			if trimmed := strings.TrimSpace(text); trimmed != "" {
				return trimmed
			}
		}
	}
	return ""
}

func normalizeStringSlice(raw any) []string {
	switch value := raw.(type) {
	case []string:
		out := make([]string, 0, len(value))
		for _, item := range value {
			if trimmed := strings.TrimSpace(item); trimmed != "" {
				out = append(out, trimmed)
			}
		}
		if len(out) == 0 {
			return nil
		}
		return out
	case []any:
		out := make([]string, 0, len(value))
		for _, item := range value {
			if text, ok := item.(string); ok {
				if trimmed := strings.TrimSpace(text); trimmed != "" {
					out = append(out, trimmed)
				}
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
