package helps

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/buildinfo"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const (
	ProviderTLSProbeEvidenceType                    = "core-mediated-provider-tls-probe"
	ProviderTLSProbeEchoEvidenceType                = "core-mediated-fingerprint-echo-probe"
	ProviderTLSProbeClaimScope                      = "core-outbound-tls-handshake-not-authenticated-provider-request"
	ProviderTLSProbeEchoClaimScope                  = "controlled-fingerprint-echo-not-provider-edge"
	ProviderTLSProbeRuntimeProfileSourceExplicit    = "explicit_account_profile"
	ProviderTLSProbeRuntimeProfileSourceCoreDefault = "core_default_runtime"
	ProviderTLSProbeRuntimeProfileSourceCoreManaged = "core_managed_account_runtime"
	ProviderTLSProbeTargetKindProviderHost          = "provider_host"
	ProviderTLSProbeTargetKindFingerprintEcho       = "controlled_fingerprint_echo"
)

var errProviderTLSProbeRuntimeTransportRequired = errors.New("runtime_transport_required")

// RuntimeHelloObserver is implemented by round trippers that can report the
// runtime (actually-used) ClientHello state, so the provider TLS probe can
// distinguish "TLS profile configured/enforced by the builder" from "the
// fingerprint actually used at runtime". A round tripper that silently
// downgraded from HelloCustom to the Chrome-like fallback reports Downgraded.
type RuntimeHelloObserver interface {
	RuntimeHelloState() RuntimeHelloState
}

// RuntimeHelloState captures the runtime ClientHello observability for a utls
// round tripper. ConfiguredHello is the expected ClientHello identifier;
// LastHandshakeHello is the identifier used by the most recent successful
// handshake; FallbackCount is how many times a silent downgrade occurred;
// Downgraded is true when FallbackCount > 0 or LastHandshakeHello differs from
// ConfiguredHello. RetryCount is how many extra configured-ClientHello handshake
// attempts were made (transient-failure retries that reused the same
// fingerprint). HardFailCount is how many times the configured ClientHello
// exhausted retries and failed WITHOUT downgrading (strict mode, e.g. the claude
// HelloCustom no-downgrade profile). For the claude strict profile FallbackCount
// is always 0 and a failure shows up in HardFailCount instead.
type RuntimeHelloState struct {
	ConfiguredHello    string `json:"configured_hello,omitempty"`
	LastHandshakeHello string `json:"last_handshake_hello,omitempty"`
	FallbackCount      int64  `json:"fallback_count"`
	RetryCount         int64  `json:"retry_count"`
	HardFailCount      int64  `json:"hard_fail_count"`
	Downgraded         bool   `json:"downgraded"`
}

type ProviderTLSProbeOptions struct {
	Timestamp     time.Time
	CorrelationID string
	TargetHost    string
	Path          string
	Method        string
	RoundTripper  http.RoundTripper
}

type ProviderTLSProbeResult struct {
	EvidenceType                 string                           `json:"evidence_type"`
	ClaimScope                   string                           `json:"claim_scope"`
	CorrelationID                string                           `json:"correlation_id"`
	Provider                     string                           `json:"provider"`
	AuthIDHash                   string                           `json:"auth_id_hash,omitempty"`
	AccountHash                  string                           `json:"account_hash,omitempty"`
	TargetKind                   string                           `json:"target_kind"`
	TargetHost                   string                           `json:"target_host"`
	OutboundURL                  string                           `json:"outbound_url"`
	Method                       string                           `json:"method"`
	RequestTimestampWindow       ProviderTLSProbeTimestampWindow  `json:"request_timestamp_window"`
	CoreBuildHeaders             ProviderTLSProbeCoreBuildHeaders `json:"core_build_headers"`
	AccountRuntimeEvidenceSHA256 string                           `json:"account_runtime_evidence_sha256"`
	AccountRuntimeSummary        ProviderTLSProbeRuntimeSummary   `json:"account_runtime_summary"`
	HTTPStatus                   int                              `json:"http_status,omitempty"`
	HTTPStatusText               string                           `json:"http_status_text,omitempty"`
	Error                        string                           `json:"error,omitempty"`
	AuthorizationSent            bool                             `json:"authorization_sent"`
	ProviderObserved             bool                             `json:"provider_observed"`
	SecretValuesStored           bool                             `json:"secret_values_stored"`
	RuntimeProfileEnforced       bool                             `json:"runtime_profile_enforced"`
	RuntimeProfileSource         string                           `json:"runtime_profile_source,omitempty"`
	// RuntimeHello* fields report the runtime (actually-used) ClientHello as
	// observed after the request, independent of TLSEnforced (which only
	// reflects builder-time assembly). They let an operator notice a configured
	// HelloCustom that silently downgraded to the Chrome-like fallback at
	// runtime (e.g. TLSEnforced=true but RuntimeHelloDowngraded=true). Empty
	// RuntimeHelloLast means the runtime hello could not be observed (no
	// successful handshake or the transport is not a RuntimeHelloObserver).
	RuntimeHelloConfigured    string                           `json:"runtime_hello_configured,omitempty"`
	RuntimeHelloLast          string                           `json:"runtime_hello_last,omitempty"`
	RuntimeHelloFallbackCount int64                            `json:"runtime_hello_fallback_count"`
	RuntimeHelloRetryCount    int64                            `json:"runtime_hello_retry_count"`
	RuntimeHelloHardFailCount int64                            `json:"runtime_hello_hard_fail_count"`
	RuntimeHelloDowngraded    bool                             `json:"runtime_hello_downgraded"`
	Limitations               []string                         `json:"limitations"`
	Transport                 ProviderTLSProbeTransportSummary `json:"transport"`
	EchoFingerprint           *ProviderTLSProbeEchoFingerprint `json:"echo_fingerprint,omitempty"`
	ProviderEdgeParityScore   ProviderTLSProbeParityScore      `json:"provider_edge_parity_score"`
}

type ProviderTLSProbeTimestampWindow struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

type ProviderTLSProbeCoreBuildHeaders struct {
	Version   string `json:"X-CPA-VERSION"`
	Commit    string `json:"X-CPA-COMMIT"`
	BuildDate string `json:"X-CPA-BUILD-DATE"`
}

type ProviderTLSProbeRuntimeSummary struct {
	EvidenceType         string `json:"evidence_type"`
	ClaimScope           string `json:"claim_scope"`
	Provider             string `json:"provider"`
	RefreshEnabled       bool   `json:"refresh_enabled"`
	ManagedHeaderPolicy  string `json:"managed_header_policy,omitempty"`
	ManagedHeaderSource  string `json:"managed_header_source,omitempty"`
	ManagedHeaderVersion string `json:"managed_header_version,omitempty"`
	TransportProfileID   string `json:"transport_profile_id"`
	TransportEnforced    bool   `json:"transport_enforced"`
	TLSProfileID         string `json:"tls_profile_id"`
	TLSEnforced          bool   `json:"tls_enforced"`
	HTTPVersion          string `json:"http_version"`
	AuthorizationSent    bool   `json:"authorization_sent"`
	SecretValuesStored   bool   `json:"secret_values_stored"`
	ProviderObserved     bool   `json:"provider_observed"`
}

type ProviderTLSProbeTransportSummary struct {
	RuntimeProfileConfigured bool     `json:"runtime_profile_configured"`
	RuntimeProfileEnforced   bool     `json:"runtime_profile_enforced"`
	RuntimeProfileSource     string   `json:"runtime_profile_source,omitempty"`
	ProxyConfigured          bool     `json:"proxy_configured"`
	TransportProfileID       string   `json:"transport_profile_id,omitempty"`
	TLSProfileID             string   `json:"tls_profile_id,omitempty"`
	ALPN                     []string `json:"alpn,omitempty"`
	ForceHTTP11              bool     `json:"force_http11,omitempty"`
}

type ProviderTLSProbeParityScore struct {
	Score       int                               `json:"score"`
	MaxScore    int                               `json:"max_score"`
	Threshold   int                               `json:"threshold"`
	Passed      bool                              `json:"passed"`
	ClaimScope  string                            `json:"claim_scope"`
	Methodology string                            `json:"methodology"`
	Components  []ProviderTLSProbeParityComponent `json:"components"`
}

type ProviderTLSProbeParityComponent struct {
	Name     string `json:"name"`
	Weight   int    `json:"weight"`
	Earned   int    `json:"earned"`
	Status   string `json:"status"`
	Evidence string `json:"evidence,omitempty"`
}

type ProviderTLSProbeEchoFingerprint struct {
	SourceHost            string `json:"source_host"`
	HTTPVersion           string `json:"http_version,omitempty"`
	UserAgent             string `json:"user_agent,omitempty"`
	TLSVersionNegotiated  string `json:"tls_version_negotiated,omitempty"`
	JA3                   string `json:"ja3,omitempty"`
	JA3Hash               string `json:"ja3_hash,omitempty"`
	JA4                   string `json:"ja4,omitempty"`
	JA4R                  string `json:"ja4_r,omitempty"`
	Peetprint             string `json:"peetprint,omitempty"`
	PeetprintHash         string `json:"peetprint_hash,omitempty"`
	AkamaiFingerprint     string `json:"akamai_fingerprint,omitempty"`
	AkamaiFingerprintHash string `json:"akamai_fingerprint_hash,omitempty"`
}

func RunProviderTLSProbe(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, opts ProviderTLSProbeOptions) (ProviderTLSProbeResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	provider := providerFromAuth(auth)
	targetHost, errTarget := ProviderTLSProbeSafeTargetHost(provider, opts.TargetHost)
	if errTarget != nil {
		return ProviderTLSProbeResult{}, errTarget
	}
	targetKind := providerTLSProbeTargetKind(provider, targetHost)
	method := normalizeProviderTLSProbeMethod(opts.Method)
	pathInput := opts.Path
	if strings.TrimSpace(pathInput) == "" && targetKind == ProviderTLSProbeTargetKindFingerprintEcho {
		pathInput = "/api/all"
	}
	path, errPath := normalizeProviderTLSProbePath(pathInput)
	if errPath != nil {
		return ProviderTLSProbeResult{}, errPath
	}
	correlationID := strings.TrimSpace(opts.CorrelationID)
	if correlationID == "" {
		correlationID = "provider-tls-probe-" + uuid.NewString()
	}
	start := opts.Timestamp
	if start.IsZero() {
		start = time.Now().UTC()
	}

	outboundURL := "https://" + targetHost + path
	runtimeEvidence := BuildAccountRuntimeEvidence(WithRuntimeTransportHost(ctx, targetHost), cfg, auth, AccountRuntimeEvidenceOptions{
		Timestamp:     start,
		CorrelationID: correlationID,
		BaseURLHost:   targetHost,
	})
	runtimeEvidenceHash := accountRuntimeEvidenceHash(runtimeEvidence)

	result := ProviderTLSProbeResult{
		EvidenceType:                 providerTLSProbeEvidenceTypeForTarget(targetKind),
		ClaimScope:                   providerTLSProbeClaimScopeForTarget(targetKind),
		CorrelationID:                correlationID,
		Provider:                     provider,
		AuthIDHash:                   runtimeEvidence.AuthIDHash,
		AccountHash:                  runtimeEvidence.AccountHash,
		TargetKind:                   targetKind,
		TargetHost:                   targetHost,
		OutboundURL:                  outboundURL,
		Method:                       method,
		CoreBuildHeaders:             ProviderTLSProbeCoreBuildHeaders{Version: buildinfo.Version, Commit: buildinfo.Commit, BuildDate: buildinfo.BuildDate},
		AccountRuntimeEvidenceSHA256: runtimeEvidenceHash,
		AccountRuntimeSummary:        providerTLSProbeRuntimeSummary(runtimeEvidence),
		AuthorizationSent:            false,
		ProviderObserved:             false,
		SecretValuesStored:           false,
		Transport:                    providerTLSProbeTransportSummary(auth, cfg),
	}

	roundTripper, runtimeProfileSource, errTransport := providerTLSProbeRuntimeRoundTripper(WithRuntimeTransportHost(ctx, targetHost), cfg, auth, opts.RoundTripper)
	if errTransport != nil {
		result.Error = errTransport.Error()
		result.ProviderEdgeParityScore = providerTLSProbeParityScore(result)
		return result, errTransport
	}
	result.RuntimeProfileEnforced = true
	result.RuntimeProfileSource = runtimeProfileSource
	result.Transport.RuntimeProfileEnforced = true
	result.Transport.RuntimeProfileSource = runtimeProfileSource
	result.Limitations = []string{
		"diagnostic probe only: no Authorization header is sent and no provider credential is used",
		"successful TLS/HTTP status only proves the core runtime-profile transport initiated provider-host outbound traffic",
		"this is not a full authenticated account request and cannot validate provider Authorization behavior",
		"normal pcap cannot reveal encrypted HTTP/2 SETTINGS",
	}
	if targetKind == ProviderTLSProbeTargetKindFingerprintEcho {
		result.Limitations = []string{
			"controlled fingerprint echo only: no Authorization header is sent and no provider credential is used",
			"echo fields prove what the controlled echo endpoint observed for this diagnostic request",
			"this is not the real provider edge and must not be claimed as provider-side TLS parity",
			"normal pcap cannot reveal encrypted HTTP/2 SETTINGS; HTTP/2 fields here come only from the echo response",
		}
	}

	req, errReq := http.NewRequestWithContext(ctx, method, outboundURL, nil)
	if errReq != nil {
		return ProviderTLSProbeResult{}, fmt.Errorf("build provider tls probe request: %w", errReq)
	}
	req.Header.Set("Accept", "*/*")
	req.Header.Set("X-CLIProxyAPI-Diagnostic-Correlation-ID", correlationID)
	req.Header.Set("X-CLIProxyAPI-Diagnostic-Probe", "provider-tls")
	req.Header.Del("Authorization")
	req.Header.Del("Proxy-Authorization")

	client := &http.Client{Transport: roundTripper}
	resp, errDo := client.Do(req)
	end := time.Now().UTC()
	result.RequestTimestampWindow = ProviderTLSProbeTimestampWindow{
		Start: start.UTC().Format(time.RFC3339Nano),
		End:   end.Format(time.RFC3339Nano),
	}
	applyRuntimeHelloState(&result, roundTripper)
	if errDo != nil {
		result.Error = errDo.Error()
		result.ProviderEdgeParityScore = providerTLSProbeParityScore(result)
		logProviderTLSProbeResult(result)
		return result, nil
	}
	if resp != nil {
		result.HTTPStatus = resp.StatusCode
		result.HTTPStatusText = resp.Status
		if resp.Body != nil {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			_ = resp.Body.Close()
			if targetKind == ProviderTLSProbeTargetKindFingerprintEcho {
				result.EchoFingerprint = parseProviderTLSProbeEchoFingerprint(targetHost, body)
			}
		}
	}
	result.ProviderEdgeParityScore = providerTLSProbeParityScore(result)
	logProviderTLSProbeResult(result)
	return result, nil
}

func ProviderTLSProbeSafeTargetHost(provider string, requestedHost string) (string, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	requestedHost = strings.ToLower(strings.TrimSpace(requestedHost))
	if strings.Contains(requestedHost, "://") || strings.ContainsAny(requestedHost, "/@") {
		return "", fmt.Errorf("target_host must be a hostname only")
	}
	defaultHost := ""
	switch provider {
	case "claude":
		defaultHost = "api.anthropic.com"
	case "codex":
		defaultHost = "chatgpt.com"
	case "gemini", "gemini-cli":
		defaultHost = "cloudcode-pa.googleapis.com"
	default:
		return "", fmt.Errorf("provider %q does not support provider TLS probe", provider)
	}
	if requestedHost == "" {
		return defaultHost, nil
	}
	if isProviderTLSProbeEchoTargetHost(requestedHost) {
		return requestedHost, nil
	}
	if requestedHost != defaultHost {
		return "", fmt.Errorf("target_host %q is not allowed for provider %q; expected %q", requestedHost, provider, defaultHost)
	}
	return requestedHost, nil
}

func providerTLSProbeTargetKind(provider string, targetHost string) string {
	if isProviderTLSProbeEchoTargetHost(targetHost) {
		return ProviderTLSProbeTargetKindFingerprintEcho
	}
	return ProviderTLSProbeTargetKindProviderHost
}

func providerTLSProbeEvidenceTypeForTarget(targetKind string) string {
	if targetKind == ProviderTLSProbeTargetKindFingerprintEcho {
		return ProviderTLSProbeEchoEvidenceType
	}
	return ProviderTLSProbeEvidenceType
}

func providerTLSProbeClaimScopeForTarget(targetKind string) string {
	if targetKind == ProviderTLSProbeTargetKindFingerprintEcho {
		return ProviderTLSProbeEchoClaimScope
	}
	return ProviderTLSProbeClaimScope
}

func isProviderTLSProbeEchoTargetHost(host string) bool {
	switch strings.ToLower(strings.TrimSpace(host)) {
	case "tls.peet.ws":
		return true
	default:
		return false
	}
}

func normalizeProviderTLSProbeMethod(method string) string {
	switch strings.ToUpper(strings.TrimSpace(method)) {
	case http.MethodGet:
		return http.MethodGet
	default:
		return http.MethodHead
	}
}

func normalizeProviderTLSProbePath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "/", nil
	}
	if strings.Contains(path, "://") || strings.Contains(path, "\r") || strings.Contains(path, "\n") {
		return "", fmt.Errorf("path must be a relative absolute path")
	}
	if !strings.HasPrefix(path, "/") {
		return "", fmt.Errorf("path must start with /")
	}
	return path, nil
}

func providerTLSProbeRuntimeRoundTripper(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, override http.RoundTripper) (http.RoundTripper, string, error) {
	proxyURL := ""
	if auth != nil {
		proxyURL = strings.TrimSpace(auth.ProxyURL)
	}
	if proxyURL == "" && cfg != nil {
		proxyURL = strings.TrimSpace(cfg.ProxyURL)
	}
	if rt, ok := BuildRuntimeTransportRoundTripper(proxyURL, auth); ok && rt != nil {
		source := ProviderTLSProbeRuntimeProfileSourceExplicit
		if profile := ResolveRuntimeTransportProfile(auth); profile != nil && profile.CoreManaged {
			source = ProviderTLSProbeRuntimeProfileSourceCoreManaged
		}
		if isPlainDefaultRoundTripper(rt) || isPlainDefaultRoundTripper(override) {
			return nil, "", fmt.Errorf("%w: runtime transport resolved to http.DefaultTransport", errProviderTLSProbeRuntimeTransportRequired)
		}
		if override != nil {
			return override, source, nil
		}
		return rt, source, nil
	}
	enforced, status := RuntimeTransportProfileStatus(auth)
	if enforced {
		status = "runtime transport builder returned no executable transport"
	}
	if profile := ResolveRuntimeTransportProfile(auth); profile == nil {
		if client := newProxyAwareHTTPClient(ctx, cfg, auth, 0); client != nil {
			if isPlainDefaultRoundTripper(override) {
				return nil, "", fmt.Errorf("%w: runtime transport override resolved to http.DefaultTransport", errProviderTLSProbeRuntimeTransportRequired)
			}
			if override != nil {
				return override, ProviderTLSProbeRuntimeProfileSourceCoreDefault, nil
			}
			if isPlainDefaultRoundTripper(client.Transport) {
				return nil, "", fmt.Errorf("%w: core default runtime transport explicitly resolved to http.DefaultTransport", errProviderTLSProbeRuntimeTransportRequired)
			}
			return client.Transport, ProviderTLSProbeRuntimeProfileSourceCoreDefault, nil
		}
		status = "core default runtime transport did not provide an executable transport"
	}
	if status == "" {
		status = "runtime transport profile is not configured or unsupported"
	}
	return nil, "", fmt.Errorf("%w: %s", errProviderTLSProbeRuntimeTransportRequired, status)
}

// applyRuntimeHelloState reads back the runtime ClientHello state from the
// round tripper (if it implements RuntimeHelloObserver) and folds it into the
// read-only RuntimeHello* result fields. It is a no-op when the transport does
// not expose runtime hello observability.
func applyRuntimeHelloState(result *ProviderTLSProbeResult, roundTripper http.RoundTripper) {
	observer, ok := roundTripper.(RuntimeHelloObserver)
	if !ok {
		return
	}
	state := observer.RuntimeHelloState()
	result.RuntimeHelloConfigured = state.ConfiguredHello
	result.RuntimeHelloLast = state.LastHandshakeHello
	result.RuntimeHelloFallbackCount = state.FallbackCount
	result.RuntimeHelloRetryCount = state.RetryCount
	result.RuntimeHelloHardFailCount = state.HardFailCount
	result.RuntimeHelloDowngraded = state.Downgraded
}

func providerTLSProbeRuntimeSummary(evidence AccountRuntimeEvidence) ProviderTLSProbeRuntimeSummary {
	return ProviderTLSProbeRuntimeSummary{
		EvidenceType:         evidence.EvidenceType,
		ClaimScope:           evidence.ClaimScope,
		Provider:             evidence.Provider,
		RefreshEnabled:       evidence.RefreshEnabled,
		ManagedHeaderPolicy:  evidence.ManagedHeaders.PolicyVersion,
		ManagedHeaderSource:  evidence.ManagedHeaders.Source,
		ManagedHeaderVersion: evidence.ManagedHeaders.Version,
		TransportProfileID:   evidence.TransportProfile.ProfileID,
		TransportEnforced:    evidence.TransportProfile.RuntimeEnforced,
		TLSProfileID:         evidence.TLSProfile.ProfileID,
		TLSEnforced:          evidence.TLSProfile.RuntimeEnforced,
		HTTPVersion:          evidence.HTTPVersion.Version,
		AuthorizationSent:    false,
		SecretValuesStored:   evidence.SecretValuesStored,
		ProviderObserved:     evidence.ProviderObserved,
	}
}

func providerTLSProbeTransportSummary(auth *cliproxyauth.Auth, cfg *config.Config) ProviderTLSProbeTransportSummary {
	profile := ResolveRuntimeTransportProfile(auth)
	if profile == nil {
		return ProviderTLSProbeTransportSummary{ProxyConfigured: providerTLSProbeProxyConfigured(auth, cfg)}
	}
	return ProviderTLSProbeTransportSummary{
		RuntimeProfileConfigured: profile.TransportConfigured || profile.TLSConfigured,
		RuntimeProfileEnforced:   profile.SupportsRuntime(),
		RuntimeProfileSource:     providerTLSProbeRuntimeProfileSource(profile),
		ProxyConfigured:          providerTLSProbeProxyConfigured(auth, cfg),
		TransportProfileID:       profile.ProfileID,
		TLSProfileID:             profile.TLSProfileID,
		ALPN:                     cloneSortedStrings(profile.ALPN),
		ForceHTTP11:              profile.ForceHTTP11,
	}
}

func providerTLSProbeProxyConfigured(auth *cliproxyauth.Auth, cfg *config.Config) bool {
	if auth != nil && strings.TrimSpace(auth.ProxyURL) != "" {
		return true
	}
	return cfg != nil && strings.TrimSpace(cfg.ProxyURL) != ""
}

func providerTLSProbeRuntimeProfileSource(profile *RuntimeTransportProfile) string {
	if profile != nil && profile.CoreManaged {
		return ProviderTLSProbeRuntimeProfileSourceCoreManaged
	}
	return ProviderTLSProbeRuntimeProfileSourceExplicit
}

func providerTLSProbeParityScore(result ProviderTLSProbeResult) ProviderTLSProbeParityScore {
	components := []ProviderTLSProbeParityComponent{
		providerTLSProbeAccountIdentityComponent(result),
		providerTLSProbeManagedHeadersComponent(result),
		providerTLSProbeRuntimeProfileComponent(result),
		providerTLSProbeFingerprintComponent(result),
		providerTLSProbeHTTP2Component(result),
		providerTLSProbePathComponent(result),
		providerTLSProbeSafetyComponent(result),
	}
	score := 0
	maxScore := 0
	for _, component := range components {
		score += component.Earned
		maxScore += component.Weight
	}
	const threshold = 90
	return ProviderTLSProbeParityScore{
		Score:       score,
		MaxScore:    maxScore,
		Threshold:   threshold,
		Passed:      score >= threshold,
		ClaimScope:  "project-defined-provider-edge-parity-approximation-not-provider-attestation",
		Methodology: "Community best-practice CLI TLS/runtime readiness score. A score >=90 means the core-mediated controlled evidence is strong enough for this project's provider-edge approximation target; it is not provider official parity or provider edge attestation.",
		Components:  components,
	}
}

func providerTLSProbeAccountIdentityComponent(result ProviderTLSProbeResult) ProviderTLSProbeParityComponent {
	earned := 0
	evidence := make([]string, 0, 3)
	if strings.TrimSpace(result.AuthIDHash) != "" {
		earned += 5
		evidence = append(evidence, "auth_id_hash")
	}
	if strings.TrimSpace(result.AccountHash) != "" {
		earned += 5
		evidence = append(evidence, "account_hash")
	}
	if strings.TrimSpace(result.AccountRuntimeEvidenceSHA256) != "" {
		earned += 5
		evidence = append(evidence, "account_runtime_evidence_sha256")
	}
	return providerTLSProbeParityComponent("account_runtime_identity", 15, earned, strings.Join(evidence, ","))
}

func providerTLSProbeManagedHeadersComponent(result ProviderTLSProbeResult) ProviderTLSProbeParityComponent {
	earned := 0
	evidence := make([]string, 0, 3)
	if strings.TrimSpace(result.AccountRuntimeSummary.ManagedHeaderPolicy) != "" {
		earned += 5
		evidence = append(evidence, "policy")
	}
	if strings.TrimSpace(result.AccountRuntimeSummary.ManagedHeaderSource) != "" {
		earned += 5
		evidence = append(evidence, "source")
	}
	if strings.TrimSpace(result.AccountRuntimeSummary.ManagedHeaderVersion) != "" {
		earned += 5
		evidence = append(evidence, "version")
	}
	return providerTLSProbeParityComponent("managed_headers_strategy", 15, earned, strings.Join(evidence, ","))
}

func providerTLSProbeRuntimeProfileComponent(result ProviderTLSProbeResult) ProviderTLSProbeParityComponent {
	earned := 0
	evidence := make([]string, 0, 4)
	if result.RuntimeProfileEnforced && result.Transport.RuntimeProfileEnforced {
		earned += 6
		evidence = append(evidence, "runtime_enforced")
	}
	if strings.TrimSpace(result.Transport.TransportProfileID) != "" {
		earned += 3
		evidence = append(evidence, "transport_profile")
	}
	if strings.TrimSpace(result.Transport.TLSProfileID) != "" {
		earned += 3
		evidence = append(evidence, "tls_profile")
	}
	if strings.TrimSpace(result.RuntimeProfileSource) != "" || strings.TrimSpace(result.Transport.RuntimeProfileSource) != "" {
		earned += 3
		evidence = append(evidence, "source")
	}
	return providerTLSProbeParityComponent("runtime_transport_profile", 15, earned, strings.Join(evidence, ","))
}

func providerTLSProbeFingerprintComponent(result ProviderTLSProbeResult) ProviderTLSProbeParityComponent {
	fingerprint := result.EchoFingerprint
	earned := 0
	evidence := make([]string, 0, 6)
	if fingerprint != nil && strings.TrimSpace(fingerprint.TLSVersionNegotiated) != "" {
		earned += 4
		evidence = append(evidence, "tls_version")
	}
	if fingerprint != nil && strings.TrimSpace(fingerprint.JA3Hash) != "" {
		earned += 5
		evidence = append(evidence, "ja3_hash")
	}
	if fingerprint != nil && strings.TrimSpace(fingerprint.JA4) != "" {
		earned += 5
		evidence = append(evidence, "ja4")
	}
	if fingerprint != nil && strings.TrimSpace(fingerprint.HTTPVersion) != "" {
		earned += 4
		evidence = append(evidence, "http_version")
	}
	if fingerprint != nil && strings.TrimSpace(fingerprint.AkamaiFingerprintHash) != "" {
		earned += 5
		evidence = append(evidence, "akamai_http2_hash")
	}
	if fingerprint != nil && strings.TrimSpace(fingerprint.PeetprintHash) != "" {
		earned += 2
		evidence = append(evidence, "peetprint_hash")
	}
	return providerTLSProbeParityComponent("controlled_tls_http_fingerprint", 25, earned, strings.Join(evidence, ","))
}

func providerTLSProbeHTTP2Component(result ProviderTLSProbeResult) ProviderTLSProbeParityComponent {
	earned := 0
	evidence := make([]string, 0, 3)
	if result.EchoFingerprint != nil && strings.EqualFold(strings.TrimSpace(result.EchoFingerprint.HTTPVersion), "h2") {
		earned += 4
		evidence = append(evidence, "echo_h2")
	}
	if result.EchoFingerprint != nil && strings.TrimSpace(result.EchoFingerprint.AkamaiFingerprintHash) != "" {
		earned += 4
		evidence = append(evidence, "akamai_http2_hash")
	}
	if containsStringFold(result.Transport.ALPN, "h2") || !result.Transport.ForceHTTP11 {
		earned += 2
		evidence = append(evidence, "runtime_h2_capable")
	}
	return providerTLSProbeParityComponent("http2_transport_behavior", 10, earned, strings.Join(evidence, ","))
}

func providerTLSProbePathComponent(result ProviderTLSProbeResult) ProviderTLSProbeParityComponent {
	earned := 0
	evidence := make([]string, 0, 4)
	if result.HTTPStatus > 0 && result.HTTPStatus < 500 {
		earned += 3
		evidence = append(evidence, "http_status")
	}
	if strings.TrimSpace(result.TargetHost) != "" && strings.TrimSpace(result.OutboundURL) != "" {
		earned += 2
		evidence = append(evidence, "target_host")
	}
	if result.Transport.ProxyConfigured {
		earned += 3
		evidence = append(evidence, "proxy_configured")
	}
	if strings.TrimSpace(result.RuntimeProfileSource) != "" {
		earned += 2
		evidence = append(evidence, "runtime_source")
	}
	return providerTLSProbeParityComponent("core_runtime_path", 10, earned, strings.Join(evidence, ","))
}

func providerTLSProbeSafetyComponent(result ProviderTLSProbeResult) ProviderTLSProbeParityComponent {
	earned := 0
	evidence := make([]string, 0, 4)
	if !result.AuthorizationSent {
		earned += 3
		evidence = append(evidence, "no_authorization")
	}
	if !result.SecretValuesStored {
		earned += 3
		evidence = append(evidence, "no_secret_storage")
	}
	if result.TargetKind == ProviderTLSProbeTargetKindFingerprintEcho && !result.ProviderObserved {
		earned += 2
		evidence = append(evidence, "controlled_scope_labeled")
	}
	if strings.TrimSpace(result.ClaimScope) != "" {
		earned += 2
		evidence = append(evidence, "claim_scope")
	}
	return providerTLSProbeParityComponent("safety_and_claim_boundary", 10, earned, strings.Join(evidence, ","))
}

func providerTLSProbeParityComponent(name string, weight int, earned int, evidence string) ProviderTLSProbeParityComponent {
	if earned > weight {
		earned = weight
	}
	status := "missing"
	switch {
	case earned >= weight:
		status = "pass"
	case earned > 0:
		status = "partial"
	}
	return ProviderTLSProbeParityComponent{
		Name:     name,
		Weight:   weight,
		Earned:   earned,
		Status:   status,
		Evidence: evidence,
	}
}

func isPlainDefaultRoundTripper(rt http.RoundTripper) bool {
	return rt == http.DefaultTransport
}

func accountRuntimeEvidenceHash(evidence AccountRuntimeEvidence) string {
	payload, errMarshal := json.Marshal(evidence)
	if errMarshal != nil {
		return ""
	}
	sum := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func parseProviderTLSProbeEchoFingerprint(sourceHost string, body []byte) *ProviderTLSProbeEchoFingerprint {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil
	}
	tlsMap, _ := payload["tls"].(map[string]any)
	http2Map, _ := payload["http2"].(map[string]any)
	fingerprint := &ProviderTLSProbeEchoFingerprint{
		SourceHost:            sourceHost,
		HTTPVersion:           stringFromJSONMap(payload, "http_version"),
		UserAgent:             stringFromJSONMap(payload, "user_agent"),
		TLSVersionNegotiated:  stringFromJSONMap(tlsMap, "tls_version_negotiated"),
		JA3:                   stringFromJSONMap(tlsMap, "ja3"),
		JA3Hash:               stringFromJSONMap(tlsMap, "ja3_hash"),
		JA4:                   stringFromJSONMap(tlsMap, "ja4"),
		JA4R:                  stringFromJSONMap(tlsMap, "ja4_r"),
		Peetprint:             stringFromJSONMap(tlsMap, "peetprint"),
		PeetprintHash:         stringFromJSONMap(tlsMap, "peetprint_hash"),
		AkamaiFingerprint:     stringFromJSONMap(http2Map, "akamai_fingerprint"),
		AkamaiFingerprintHash: stringFromJSONMap(http2Map, "akamai_fingerprint_hash"),
	}
	return fingerprint
}

func stringFromJSONMap(payload map[string]any, key string) string {
	if payload == nil {
		return ""
	}
	value, _ := payload[key].(string)
	return value
}

func logProviderTLSProbeResult(result ProviderTLSProbeResult) {
	log.WithFields(log.Fields{
		"correlation_id":                result.CorrelationID,
		"provider":                      result.Provider,
		"target_host":                   result.TargetHost,
		"outbound_url":                  result.OutboundURL,
		"http_status":                   result.HTTPStatus,
		"error":                         result.Error,
		"authorization_sent":            result.AuthorizationSent,
		"account_runtime_hash":          result.AccountRuntimeEvidenceSHA256,
		"transport_profile_id":          result.AccountRuntimeSummary.TransportProfileID,
		"tls_profile_id":                result.AccountRuntimeSummary.TLSProfileID,
		"runtime_http_version":          result.AccountRuntimeSummary.HTTPVersion,
		"core_build_version":            result.CoreBuildHeaders.Version,
		"core_build_commit":             result.CoreBuildHeaders.Commit,
		"core_build_date":               result.CoreBuildHeaders.BuildDate,
		"provider_observed":             result.ProviderObserved,
		"secret_values_stored":          result.SecretValuesStored,
		"runtime_enforced":              result.RuntimeProfileEnforced,
		"runtime_hello_configured":      result.RuntimeHelloConfigured,
		"runtime_hello_last":            result.RuntimeHelloLast,
		"runtime_hello_fallback_count":  result.RuntimeHelloFallbackCount,
		"runtime_hello_retry_count":     result.RuntimeHelloRetryCount,
		"runtime_hello_hard_fail_count": result.RuntimeHelloHardFailCount,
		"runtime_hello_downgraded":      result.RuntimeHelloDowngraded,
	}).Info("provider TLS diagnostic probe completed")
}
