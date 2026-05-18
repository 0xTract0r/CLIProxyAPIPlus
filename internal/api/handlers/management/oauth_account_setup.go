package management

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	runtimehelps "github.com/router-for-me/CLIProxyAPI/v6/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/proxyutil"
	"golang.org/x/oauth2"
)

type oauthAccountSetup struct {
	Note     string
	ProxyURL string
}

type oauthSessionResult struct {
	Provider  string
	SavedPath string
	AuthName  string
	Note      string
	ProxyURL  string
}

type oauthIdentityHeaderRoundTripper struct {
	base    http.RoundTripper
	headers map[string]string
}

func (rt oauthIdentityHeaderRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if len(rt.headers) == 0 {
		return rt.baseRoundTripper().RoundTrip(req)
	}
	clone := req.Clone(req.Context())
	clone.Header = req.Header.Clone()
	for name, value := range rt.headers {
		name = strings.TrimSpace(name)
		value = strings.TrimSpace(value)
		if name == "" || value == "" || clone.Header.Get(name) != "" {
			continue
		}
		clone.Header.Set(name, value)
	}
	return rt.baseRoundTripper().RoundTrip(clone)
}

func (rt oauthIdentityHeaderRoundTripper) baseRoundTripper() http.RoundTripper {
	if rt.base != nil {
		return rt.base
	}
	return http.DefaultTransport
}

func parseOAuthAccountSetupFromRequest(c *gin.Context) (*oauthAccountSetup, error) {
	if c == nil {
		return nil, nil
	}
	note := firstNonEmptyQuery(c, "note", "account_note", "remark")
	proxyURL := firstNonEmptyQuery(c, "proxy_url", "proxyUrl", "proxy")
	if strings.TrimSpace(note) == "" && strings.TrimSpace(proxyURL) == "" {
		return nil, nil
	}
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL != "" {
		if _, err := proxyutil.Parse(proxyURL); err != nil {
			return nil, fmt.Errorf("invalid proxy_url: %w", err)
		}
	}
	return &oauthAccountSetup{
		Note:     strings.TrimSpace(note),
		ProxyURL: proxyURL,
	}, nil
}

func firstNonEmptyQuery(c *gin.Context, names ...string) string {
	for _, name := range names {
		if value := strings.TrimSpace(c.Query(name)); value != "" {
			return value
		}
	}
	return ""
}

func (s *oauthAccountSetup) runtimeAuth(provider string) *coreauth.Auth {
	if s == nil {
		return nil
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "anthropic" {
		provider = "claude"
	}
	metadata := map[string]any{"type": provider}
	attributes := map[string]string{}
	if s.ProxyURL != "" {
		metadata["proxy_url"] = s.ProxyURL
	}
	if s.Note != "" {
		metadata["note"] = s.Note
		attributes["note"] = s.Note
	}
	return &coreauth.Auth{
		Provider:   provider,
		ProxyURL:   s.ProxyURL,
		Metadata:   metadata,
		Attributes: attributes,
	}
}

func (h *Handler) configForOAuthSetup(setup *oauthAccountSetup) *config.Config {
	if h == nil {
		if setup != nil && strings.TrimSpace(setup.ProxyURL) != "" {
			return &config.Config{SDKConfig: config.SDKConfig{ProxyURL: setup.ProxyURL}}
		}
		return nil
	}
	if setup == nil || strings.TrimSpace(setup.ProxyURL) == "" {
		return h.cfg
	}
	if h.cfg == nil {
		return &config.Config{SDKConfig: config.SDKConfig{ProxyURL: setup.ProxyURL}}
	}
	cfgCopy := *h.cfg
	cfgCopy.SDKConfig.ProxyURL = setup.ProxyURL
	return &cfgCopy
}

func (h *Handler) oauthHTTPClientForSetup(setup *oauthAccountSetup) *http.Client {
	return utilSetProxyForOAuth(h.configForOAuthSetup(setup), &http.Client{})
}

func (h *Handler) prepareOAuthSetupRuntimeAuth(provider string, setup *oauthAccountSetup) *coreauth.Auth {
	if setup == nil {
		return nil
	}
	auth := setup.runtimeAuth(provider)
	h.applyOAuthAccountSetupToRecord(auth, setup)
	return auth
}

func copyOAuthSetupSeed(dst *coreauth.Auth, src *coreauth.Auth) {
	if dst == nil || src == nil || src.Metadata == nil {
		return
	}
	seed := strings.TrimSpace(metadataString(src.Metadata, "managed_header_seed"))
	if seed == "" {
		return
	}
	if dst.Metadata == nil {
		dst.Metadata = make(map[string]any)
	}
	if strings.TrimSpace(metadataString(dst.Metadata, "managed_header_seed")) == "" {
		dst.Metadata["managed_header_seed"] = seed
	}
}

func (h *Handler) oauthIdentityHTTPClient(ctx context.Context, host string, auth *coreauth.Auth, timeout time.Duration) *http.Client {
	if auth == nil {
		client := &http.Client{}
		if timeout > 0 {
			client.Timeout = timeout
		}
		return utilSetProxyForOAuth(h.configForOAuthSetup(nil), client)
	}
	transportCtx := ctx
	if transportCtx == nil {
		transportCtx = context.Background()
	}
	host = strings.TrimSpace(host)
	if host != "" {
		transportCtx = runtimehelps.WithRuntimeTransportHost(transportCtx, host)
	}
	client := runtimehelps.NewProxyAwareHTTPClient(transportCtx, h.cfg, auth, timeout)
	headers := coreauth.ExtractCustomHeadersFromMetadata(auth.Metadata)
	if len(headers) == 0 {
		headers = map[string]string{}
		for key, value := range auth.Attributes {
			if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(key)), "header:") {
				continue
			}
			name := strings.TrimSpace(strings.TrimPrefix(key, "header:"))
			if name != "" && strings.TrimSpace(value) != "" {
				headers[name] = strings.TrimSpace(value)
			}
		}
	}
	return &http.Client{
		Transport: oauthIdentityHeaderRoundTripper{
			base:    client.Transport,
			headers: normalizeHeaderMap(headers),
		},
		Timeout: client.Timeout,
	}
}

func utilSetProxyForOAuth(cfg *config.Config, client *http.Client) *http.Client {
	if cfg == nil {
		return client
	}
	return util.SetProxy(&cfg.SDKConfig, client)
}

func (h *Handler) applyOAuthAccountSetupToRecord(record *coreauth.Auth, setup *oauthAccountSetup) {
	if record == nil {
		return
	}
	if record.Metadata == nil {
		record.Metadata = make(map[string]any)
	}
	if record.Attributes == nil {
		record.Attributes = make(map[string]string)
	}
	if setup != nil {
		if setup.ProxyURL != "" {
			record.ProxyURL = setup.ProxyURL
			record.Metadata["proxy_url"] = setup.ProxyURL
		}
		if setup.Note != "" {
			record.Metadata["note"] = setup.Note
			record.Attributes["note"] = setup.Note
		}
	}
	ensureManagedHeaderSeed(record)

	stored := readAccountSettingsMetadata(record, h.cfg)
	stored.SchemaVersion = accountSettingsSchemaVersion
	stored.ManagedHeaderSeedHash = accountManagedHeaderSeedHash(record)
	projection := managedHeaderProjectionForAuth(record, h.cfg)
	if len(projection.SummaryHeaders) > 0 || len(projection.VersionedCapabilities) > 0 || len(projection.StableIdentity) > 0 || len(projection.RuntimeFingerprint) > 0 {
		stored.ManagedHeaderState = mergeManagedHeaderState(stored.ManagedHeaderState, projection, providerKey(record))
	}
	profile := runtimehelps.ResolveRuntimeTransportProfile(record)
	stored.RuntimeIdentityState = mergeRuntimeIdentityState(stored.RuntimeIdentityState, record, h.cfg, profile)
	record.Metadata["account_settings"] = stored

	managedHeaders := managedHeadersForAuth(record, h.cfg)
	extraHeaders := normalizeHeaderMap(stored.ExtraHeaders)
	runtimeHeaders := mergeAccountHeaders(managedHeaders, extraHeaders)
	if len(runtimeHeaders) > 0 {
		overwriteAuthMetadataHeaders(record, runtimeHeaders)
	}
}

func oauthSessionResultForRecord(provider, savedPath string, record *coreauth.Auth) oauthSessionResult {
	result := oauthSessionResult{
		Provider:  strings.ToLower(strings.TrimSpace(provider)),
		SavedPath: strings.TrimSpace(savedPath),
	}
	if record == nil {
		return result
	}
	if result.Provider == "" {
		result.Provider = providerKey(record)
	}
	result.AuthName = authDisplayName(record)
	result.Note = authNote(record)
	result.ProxyURL = authProxyURL(record)
	return result
}

func CompleteOAuthSessionWithRecord(state string, savedPath string, record *coreauth.Auth) {
	CompleteOAuthSessionWithResult(state, oauthSessionResultForRecord("", savedPath, record))
}

var (
	accountHeaderNumericPattern = regexp.MustCompile(`\d+`)
	accountHeaderVersionPattern = regexp.MustCompile(`\d+(?:\.\d+)+|\d+`)
	codexSecCHUAChromiumPattern = regexp.MustCompile(`(?i)Chromium"?;v="?(\d+)`)
)

const managedHeaderVariantPolicyNearLatestV1 = "verified-baseline-near-latest/v1"

type accountManagedHeaderVariant struct {
	VersionOffset     int
	VersionVariant    string
	BrandOrderSlot    int
	BrandOrderVariant string
}

func accountManagedHeaderSeed(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Metadata != nil {
		if value := strings.TrimSpace(metadataString(auth.Metadata, "managed_header_seed")); value != "" {
			return strings.Join([]string{providerKey(auth), "managed_header_seed=" + value}, "|")
		}
	}
	return ""
}

func ensureManagedHeaderSeed(auth *coreauth.Auth) {
	if auth == nil {
		return
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	if strings.TrimSpace(metadataString(auth.Metadata, "managed_header_seed")) != "" {
		return
	}
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err == nil {
		auth.Metadata["managed_header_seed"] = hex.EncodeToString(buf)
		return
	}
	sum := sha256.Sum256([]byte(strings.Join([]string{
		providerKey(auth),
		strings.TrimSpace(auth.ID),
		strings.TrimSpace(auth.FileName),
		strings.TrimSpace(auth.Label),
		time.Now().UTC().Format(time.RFC3339Nano),
	}, "|")))
	auth.Metadata["managed_header_seed"] = hex.EncodeToString(sum[:16])
}

func accountManagedHeaderSeedHash(auth *coreauth.Auth) string {
	seed := accountManagedHeaderSeed(auth)
	if seed == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:])
}

func accountManagedHeaderSeedUint(auth *coreauth.Auth) uint64 {
	hash := accountManagedHeaderSeedHash(auth)
	if hash == "" {
		return 0
	}
	raw, err := hex.DecodeString(hash[:16])
	if err != nil || len(raw) < 8 {
		return 0
	}
	return binary.BigEndian.Uint64(raw)
}

func personalizeManagedHeaderProjectionForAuth(auth *coreauth.Auth, projection authFileManagedHeaderProjection) authFileManagedHeaderProjection {
	seed := accountManagedHeaderSeedUint(auth)
	if auth == nil || seed == 0 {
		return projection
	}
	variant := accountManagedHeaderVariantForAuth(auth, seed)
	switch providerKey(auth) {
	case "codex":
		return personalizeCodexManagedHeaderProjection(projection, variant)
	case "claude":
		return personalizeClaudeManagedHeaderProjection(projection, variant)
	default:
		return projection
	}
}

func accountManagedHeaderVariantForAuth(auth *coreauth.Auth, seed uint64) accountManagedHeaderVariant {
	versionOffset := -int(seed % 3)
	brandSlot := int(seed % 3)

	stored := readAccountSettingsMetadata(auth, nil)
	if current := stored.ManagedHeaderState; current != nil && current.Current != nil {
		if parsedOffset, ok := parseManagedHeaderVersionVariant(current.Current.VersionVariant); ok {
			versionOffset = parsedOffset
		}
		if parsedSlot, ok := parseManagedHeaderBrandOrderVariant(current.Current.BrandOrderVariant); ok {
			brandSlot = parsedSlot
		}
	}
	return accountManagedHeaderVariant{
		VersionOffset:     versionOffset,
		VersionVariant:    managedHeaderVersionVariantName(versionOffset),
		BrandOrderSlot:    brandSlot,
		BrandOrderVariant: managedHeaderBrandOrderVariantName(brandSlot),
	}
}

func parseManagedHeaderVersionVariant(value string) (int, bool) {
	value = strings.TrimSpace(value)
	switch value {
	case "latest":
		return 0, true
	case "latest-1":
		return -1, true
	case "latest-2":
		return -2, true
	default:
		return 0, false
	}
}

func managedHeaderVersionVariantName(offset int) string {
	switch offset {
	case 0:
		return "latest"
	case -1:
		return "latest-1"
	case -2:
		return "latest-2"
	default:
		if offset < 0 {
			return fmt.Sprintf("latest%d", offset)
		}
		return "latest"
	}
}

func parseManagedHeaderBrandOrderVariant(value string) (int, bool) {
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, "slot-") {
		return 0, false
	}
	slot, err := strconv.Atoi(strings.TrimPrefix(value, "slot-"))
	if err != nil || slot < 0 {
		return 0, false
	}
	return slot % 3, true
}

func managedHeaderBrandOrderVariantName(slot int) string {
	if slot < 0 {
		slot = 0
	}
	return fmt.Sprintf("slot-%d", slot%3)
}

func personalizeCodexManagedHeaderProjection(projection authFileManagedHeaderProjection, variant accountManagedHeaderVariant) authFileManagedHeaderProjection {
	headers := cloneStringMap(projection.SummaryHeaders)
	sourceVersion := firstNonEmptyHeaderString(headers["Version"], projection.VersionedCapabilities["Version"])
	if strings.TrimSpace(headers["Version"]) == "" && sourceVersion != "" {
		headers["Version"] = sourceVersion
	}
	variantVersion := applyManagedHeaderVersionOffset(sourceVersion, variant.VersionOffset)
	if variantVersion != "" {
		headers["Version"] = variantVersion
		if ua := replaceFirstVersionInString(headers["User-Agent"], variantVersion); ua != "" {
			headers["User-Agent"] = ua
		}
	}
	chromium := codexChromiumVersionForProjection(projection, headers)
	headers["sec-ch-ua"] = codexSecCHUAVariant(chromium, variant.BrandOrderSlot)
	headers["sec-ch-ua-mobile"] = "?0"
	headers["sec-ch-ua-platform"] = `"macOS"`
	projection.VariantPolicy = managedHeaderVariantPolicyNearLatestV1
	projection.VersionVariant = variant.VersionVariant
	projection.BrandOrderVariant = variant.BrandOrderVariant
	projection.SummaryHeaders = normalizeHeaderMap(headers)
	projection.VersionedCapabilities = normalizeHeaderMap(mergeStringMaps(projection.VersionedCapabilities, map[string]string{
		"User-Agent": projection.SummaryHeaders["User-Agent"],
		"Version":    projection.SummaryHeaders["Version"],
	}))
	projection.StableIdentity = normalizeHeaderMap(mergeStringMaps(projection.StableIdentity, map[string]string{
		"sec-ch-ua":          projection.SummaryHeaders["sec-ch-ua"],
		"sec-ch-ua-mobile":   projection.SummaryHeaders["sec-ch-ua-mobile"],
		"sec-ch-ua-platform": projection.SummaryHeaders["sec-ch-ua-platform"],
	}))
	return projection
}

func codexChromiumVersionForProjection(projection authFileManagedHeaderProjection, headers map[string]string) string {
	for _, value := range []string{
		headers["sec-ch-ua"],
		projection.StableIdentity["sec-ch-ua"],
	} {
		if match := codexSecCHUAChromiumPattern.FindStringSubmatch(value); len(match) == 2 {
			if chromium := strings.TrimSpace(match[1]); chromium != "" {
				return chromium
			}
		}
	}
	return "144"
}

func codexSecCHUAVariant(chromium string, slot int) string {
	chromium = strings.TrimSpace(chromium)
	if chromium == "" {
		chromium = "144"
	}
	switch slot % 3 {
	case 0:
		return fmt.Sprintf(`"Chromium";v="%s", "Not=A?Brand";v="24", "Codex";v="%s"`, chromium, chromium)
	case 1:
		return fmt.Sprintf(`"Not=A?Brand";v="24", "Chromium";v="%s", "Codex";v="%s"`, chromium, chromium)
	default:
		return fmt.Sprintf(`"Codex";v="%s", "Chromium";v="%s", "Not=A?Brand";v="24"`, chromium, chromium)
	}
}

func personalizeClaudeManagedHeaderProjection(projection authFileManagedHeaderProjection, variant accountManagedHeaderVariant) authFileManagedHeaderProjection {
	headers := cloneStringMap(projection.SummaryHeaders)
	sourceVersion := extractFirstVersion(headers["User-Agent"])
	variantVersion := applyManagedHeaderVersionOffset(sourceVersion, variant.VersionOffset)
	if variantVersion != "" {
		if ua := replaceFirstVersionInString(headers["User-Agent"], variantVersion); ua != "" {
			headers["User-Agent"] = ua
		}
	}
	projection.VariantPolicy = managedHeaderVariantPolicyNearLatestV1
	projection.VersionVariant = variant.VersionVariant
	projection.SummaryHeaders = normalizeHeaderMap(headers)
	projection.VersionedCapabilities = normalizeHeaderMap(mergeStringMaps(projection.VersionedCapabilities, map[string]string{
		"User-Agent":                  projection.SummaryHeaders["User-Agent"],
		"X-Stainless-Package-Version": projection.SummaryHeaders["X-Stainless-Package-Version"],
		"X-Stainless-Runtime-Version": projection.SummaryHeaders["X-Stainless-Runtime-Version"],
	}))
	return projection
}

func applyManagedHeaderVersionOffset(version string, offset int) string {
	version = strings.TrimSpace(version)
	if version == "" || offset == 0 {
		return version
	}
	matches := accountHeaderNumericPattern.FindAllStringIndex(version, -1)
	if len(matches) == 0 {
		return version
	}
	last := matches[len(matches)-1]
	patch, err := strconv.Atoi(version[last[0]:last[1]])
	if err != nil {
		return version
	}
	nextPatch := patch + offset
	if nextPatch < 0 {
		nextPatch = 0
	}
	return version[:last[0]] + strconv.Itoa(nextPatch) + version[last[1]:]
}

func extractFirstVersion(value string) string {
	if idx := accountHeaderVersionPattern.FindStringIndex(value); idx != nil {
		return value[idx[0]:idx[1]]
	}
	return ""
}

func replaceFirstVersionInString(value, version string) string {
	if strings.TrimSpace(value) == "" || strings.TrimSpace(version) == "" {
		return value
	}
	idx := accountHeaderVersionPattern.FindStringIndex(value)
	if idx == nil {
		return value
	}
	return value[:idx[0]] + version + value[idx[1]:]
}

func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return map[string]string{}
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func mergeStringMaps(base map[string]string, overlay map[string]string) map[string]string {
	out := cloneStringMap(base)
	for key, value := range overlay {
		if strings.TrimSpace(value) != "" {
			out[key] = value
		}
	}
	return out
}

func firstNonEmptyHeaderString(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func oauthStartResponse(authURL, state string) gin.H {
	expiresAt := time.Now().Add(oauthCallbackWaitTimeout).UTC().Format(time.RFC3339)
	return gin.H{
		"status":             "ok",
		"url":                authURL,
		"state":              state,
		"expires_in_seconds": int(oauthCallbackWaitTimeout.Seconds()),
		"expires_at":         expiresAt,
	}
}

func oauthContextWithHTTPClient(ctx context.Context, client *http.Client) context.Context {
	if client == nil {
		return ctx
	}
	return context.WithValue(ctx, oauth2.HTTPClient, client)
}
