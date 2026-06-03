package helps

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/buildinfo"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

type captureProbeRoundTripper struct {
	request *http.Request
}

func (rt *captureProbeRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	rt.request = req.Clone(req.Context())
	rt.request.Header = req.Header.Clone()
	return &http.Response{
		StatusCode: http.StatusUnauthorized,
		Status:     "401 Unauthorized",
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader("unauthorized")),
		Request:    req,
	}, nil
}

type echoProbeRoundTripper struct {
	request *http.Request
	body    string
}

func (rt *echoProbeRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	rt.request = req.Clone(req.Context())
	rt.request.Header = req.Header.Clone()
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(rt.body)),
		Request:    req,
	}, nil
}

func TestRunProviderTLSProbe_RuntimeTransportNoAuthorizationAndAccountRuntimeEvidence(t *testing.T) {
	originalVersion := buildinfo.Version
	originalCommit := buildinfo.Commit
	originalBuildDate := buildinfo.BuildDate
	buildinfo.Version = "test-plus"
	buildinfo.Commit = "test-commit"
	buildinfo.BuildDate = "2026-04-30T12:00:00Z"
	t.Cleanup(func() {
		buildinfo.Version = originalVersion
		buildinfo.Commit = originalCommit
		buildinfo.BuildDate = originalBuildDate
	})

	auth := &cliproxyauth.Auth{
		ID:       "codex-a02",
		FileName: "codex-a02.json",
		Provider: "codex",
		Metadata: map[string]any{
			"auth_method":   "oauth",
			"email":         "codex-a02@example.test",
			"access_token":  "must-not-leak",
			"refresh_token": "must-not-leak",
			"account_settings": map[string]any{
				"schema_version":  1,
				"refresh_enabled": false,
				"transport_profile": map[string]any{
					"provider": "codex",
					"preset":   "provider-default",
				},
				"tls_profile": map[string]any{
					"provider":     "codex",
					"preset":       "codex_go_http11_v1",
					"force_http11": true,
					"http_version": "http/1.1",
				},
			},
		},
	}
	cfg := &config.Config{
		CodexHeaderDefaults: config.CodexHeaderDefaults{
			UserAgent: "codex_cli_rs/0.125.0 (Mac OS 26.3.1; arm64) iTerm.app/3.6.9 (codex_cli_rs; 0.125.0)",
		},
	}
	rt := &captureProbeRoundTripper{}

	result, err := RunProviderTLSProbe(context.Background(), cfg, auth, ProviderTLSProbeOptions{
		Timestamp:     time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC),
		CorrelationID: "corr-codex",
		RoundTripper:  rt,
	})
	if err != nil {
		t.Fatalf("RunProviderTLSProbe returned error: %v", err)
	}
	if rt.request == nil {
		t.Fatal("probe did not issue an outbound request")
	}
	if got := rt.request.URL.String(); got != "https://chatgpt.com/" {
		t.Fatalf("outbound URL = %q, want https://chatgpt.com/", got)
	}
	if got := rt.request.Header.Get("Authorization"); got != "" {
		t.Fatalf("Authorization header = %q, want empty", got)
	}
	if got := rt.request.Header.Get("Proxy-Authorization"); got != "" {
		t.Fatalf("Proxy-Authorization header = %q, want empty", got)
	}
	if result.EvidenceType != ProviderTLSProbeEvidenceType {
		t.Fatalf("EvidenceType = %q, want %q", result.EvidenceType, ProviderTLSProbeEvidenceType)
	}
	if result.CorrelationID != "corr-codex" {
		t.Fatalf("CorrelationID = %q, want corr-codex", result.CorrelationID)
	}
	if result.TargetHost != "chatgpt.com" || result.OutboundURL != "https://chatgpt.com/" {
		t.Fatalf("target/url = %q/%q", result.TargetHost, result.OutboundURL)
	}
	if result.AuthorizationSent {
		t.Fatal("AuthorizationSent = true, want false")
	}
	if result.ProviderObserved {
		t.Fatal("ProviderObserved = true, want false")
	}
	if result.SecretValuesStored {
		t.Fatal("SecretValuesStored = true, want false")
	}
	if !result.RuntimeProfileEnforced {
		t.Fatal("RuntimeProfileEnforced = false, want true")
	}
	if result.RuntimeProfileSource != ProviderTLSProbeRuntimeProfileSourceExplicit {
		t.Fatalf("RuntimeProfileSource = %q, want %q", result.RuntimeProfileSource, ProviderTLSProbeRuntimeProfileSourceExplicit)
	}
	if !result.Transport.RuntimeProfileEnforced {
		t.Fatalf("transport runtime_profile_enforced = false, summary=%#v", result.Transport)
	}
	if result.Transport.RuntimeProfileSource != ProviderTLSProbeRuntimeProfileSourceExplicit {
		t.Fatalf("transport runtime_profile_source = %q, want %q", result.Transport.RuntimeProfileSource, ProviderTLSProbeRuntimeProfileSourceExplicit)
	}
	if result.HTTPStatus != http.StatusUnauthorized {
		t.Fatalf("HTTPStatus = %d, want 401", result.HTTPStatus)
	}
	if result.CoreBuildHeaders.Version != "test-plus" || result.CoreBuildHeaders.Commit != "test-commit" {
		t.Fatalf("CoreBuildHeaders = %#v", result.CoreBuildHeaders)
	}
	if !strings.HasPrefix(result.AccountRuntimeEvidenceSHA256, "sha256:") {
		t.Fatalf("AccountRuntimeEvidenceSHA256 = %q", result.AccountRuntimeEvidenceSHA256)
	}
	if result.AccountRuntimeSummary.ManagedHeaderPolicy != "codex-managed/v2" {
		t.Fatalf("managed policy = %q, want codex-managed/v2", result.AccountRuntimeSummary.ManagedHeaderPolicy)
	}
	if result.AccountRuntimeSummary.TransportProfileID != "provider-default" || !result.AccountRuntimeSummary.TransportEnforced {
		t.Fatalf("transport summary = %#v", result.AccountRuntimeSummary)
	}
	if result.AccountRuntimeSummary.TLSProfileID != "codex_go_http11_v1" || !result.AccountRuntimeSummary.TLSEnforced {
		t.Fatalf("tls summary = %#v", result.AccountRuntimeSummary)
	}
	if result.AccountRuntimeSummary.HTTPVersion != "http/1.1" {
		t.Fatalf("http version = %q, want http/1.1", result.AccountRuntimeSummary.HTTPVersion)
	}

	payload, errMarshal := json.Marshal(result)
	if errMarshal != nil {
		t.Fatalf("marshal result: %v", errMarshal)
	}
	lower := strings.ToLower(string(payload))
	for _, forbidden := range []string{
		"must-not-leak",
		"access_token",
		"refresh_token",
		"authorization\":\"bearer",
		"codex-a02@example.test",
		"codex-a02.json",
		"codex_cli_rs/0.125.0",
	} {
		if strings.Contains(lower, strings.ToLower(forbidden)) {
			t.Fatalf("probe result leaked %q: %s", forbidden, string(payload))
		}
	}
}

func TestRunProviderTLSProbe_NullProfilesUseCoreDefaultRuntime(t *testing.T) {
	auth := &cliproxyauth.Auth{
		ID:       "codex-a02",
		FileName: "codex-a02.json",
		Provider: "codex",
		Metadata: map[string]any{
			"auth_method":   "oauth",
			"email":         "codex-a02@example.test",
			"access_token":  "must-not-leak",
			"refresh_token": "must-not-leak",
			"account_settings": map[string]any{
				"schema_version":    1,
				"refresh_enabled":   false,
				"transport_profile": nil,
				"tls_profile":       nil,
			},
		},
	}
	cfg := &config.Config{
		SDKConfig: config.SDKConfig{ProxyURL: "http://proxy.example.test:8080"},
		CodexHeaderDefaults: config.CodexHeaderDefaults{
			UserAgent: "codex_cli_rs/0.125.0 (Mac OS 26.3.1; arm64) iTerm.app/3.6.9 (codex_cli_rs; 0.125.0)",
		},
	}
	rt := &captureProbeRoundTripper{}

	defaultRT, source, errDefault := providerTLSProbeRuntimeRoundTripper(context.Background(), cfg, auth, nil)
	if errDefault != nil {
		t.Fatalf("core default runtime transport returned error: %v", errDefault)
	}
	if source != ProviderTLSProbeRuntimeProfileSourceCoreManaged {
		t.Fatalf("source = %q, want %q", source, ProviderTLSProbeRuntimeProfileSourceCoreManaged)
	}
	if defaultRT == nil {
		t.Fatal("core default runtime transport is nil")
	}
	if defaultRT == http.DefaultTransport {
		t.Fatal("core default runtime transport used http.DefaultTransport")
	}

	result, err := RunProviderTLSProbe(context.Background(), cfg, auth, ProviderTLSProbeOptions{
		Timestamp:     time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC),
		CorrelationID: "corr-codex-a02",
		RoundTripper:  rt,
	})
	if err != nil {
		t.Fatalf("RunProviderTLSProbe returned error: %v", err)
	}
	if rt.request == nil {
		t.Fatal("probe did not issue an outbound request")
	}
	if got := rt.request.Header.Get("Authorization"); got != "" {
		t.Fatalf("Authorization header = %q, want empty", got)
	}
	if got := rt.request.Header.Get("Proxy-Authorization"); got != "" {
		t.Fatalf("Proxy-Authorization header = %q, want empty", got)
	}
	if strings.Contains(strings.ToLower(rt.request.Header.Get("Authorization")), "bearer") {
		t.Fatalf("Authorization header contains bearer: %q", rt.request.Header.Get("Authorization"))
	}
	if !result.RuntimeProfileEnforced {
		t.Fatal("RuntimeProfileEnforced = false, want true")
	}
	if result.RuntimeProfileSource != ProviderTLSProbeRuntimeProfileSourceCoreManaged {
		t.Fatalf("RuntimeProfileSource = %q, want %q", result.RuntimeProfileSource, ProviderTLSProbeRuntimeProfileSourceCoreManaged)
	}
	if !result.Transport.RuntimeProfileConfigured {
		t.Fatalf("transport runtime_profile_configured = false, summary=%#v", result.Transport)
	}
	if !result.Transport.RuntimeProfileEnforced {
		t.Fatalf("transport runtime_profile_enforced = false, summary=%#v", result.Transport)
	}
	if result.Transport.RuntimeProfileSource != ProviderTLSProbeRuntimeProfileSourceCoreManaged {
		t.Fatalf("transport runtime_profile_source = %q, want %q", result.Transport.RuntimeProfileSource, ProviderTLSProbeRuntimeProfileSourceCoreManaged)
	}
	if !result.AccountRuntimeSummary.TransportEnforced || !result.AccountRuntimeSummary.TLSEnforced {
		t.Fatalf("account managed profile summary was not enforced: %#v", result.AccountRuntimeSummary)
	}

	payload, errMarshal := json.Marshal(result)
	if errMarshal != nil {
		t.Fatalf("marshal result: %v", errMarshal)
	}
	lower := strings.ToLower(string(payload))
	for _, forbidden := range []string{
		"must-not-leak",
		"access_token",
		"refresh_token",
		"authorization\":\"bearer",
		"codex-a02@example.test",
		"codex-a02.json",
		"codex_cli_rs/0.125.0",
		"http.defaulttransport",
	} {
		if strings.Contains(lower, strings.ToLower(forbidden)) {
			t.Fatalf("probe result leaked %q: %s", forbidden, string(payload))
		}
	}
}

func TestRunProviderTLSProbeRequiresRuntimeTransportAndDoesNotFallback(t *testing.T) {
	auth := &cliproxyauth.Auth{
		ID:       "codex-no-runtime",
		FileName: "codex-no-runtime.json",
		Provider: "codex",
		Metadata: map[string]any{
			"account_settings": map[string]any{
				"schema_version": 1,
				"transport_profile": map[string]any{
					"provider": "codex",
					"preset":   "unsupported-direct-fallback",
				},
			},
		},
	}
	rt := &captureProbeRoundTripper{}

	cfg := &config.Config{SDKConfig: config.SDKConfig{ProxyURL: "http://proxy.example.test:8080"}}
	result, err := RunProviderTLSProbe(context.Background(), cfg, auth, ProviderTLSProbeOptions{
		Timestamp:     time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC),
		CorrelationID: "corr-no-runtime",
		RoundTripper:  rt,
	})
	if err == nil {
		t.Fatal("RunProviderTLSProbe returned nil error, want runtime_transport_required")
	}
	if !strings.Contains(err.Error(), "runtime_transport_required") {
		t.Fatalf("error = %q, want runtime_transport_required", err.Error())
	}
	if rt.request != nil {
		t.Fatalf("probe unexpectedly issued request to %s", rt.request.URL.String())
	}
	if result.HTTPStatus != 0 {
		t.Fatalf("HTTPStatus = %d, want 0", result.HTTPStatus)
	}
	if result.RuntimeProfileEnforced {
		t.Fatal("RuntimeProfileEnforced = true, want false")
	}
	if result.Transport.RuntimeProfileEnforced {
		t.Fatalf("transport runtime_profile_enforced = true, summary=%#v", result.Transport)
	}
	if !strings.Contains(result.Error, "runtime_transport_required") {
		t.Fatalf("result error = %q, want runtime_transport_required", result.Error)
	}
}

func TestRunProviderTLSProbe_RejectsPlainDefaultTransportOverride(t *testing.T) {
	auth := &cliproxyauth.Auth{
		ID:       "codex-no-default-runtime",
		FileName: "codex-no-default-runtime.json",
		Provider: "codex",
		Metadata: map[string]any{
			"account_settings": map[string]any{
				"schema_version":    1,
				"transport_profile": nil,
				"tls_profile":       nil,
			},
		},
	}

	result, err := RunProviderTLSProbe(context.Background(), nil, auth, ProviderTLSProbeOptions{
		Timestamp:     time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC),
		CorrelationID: "corr-no-default-runtime",
		RoundTripper:  http.DefaultTransport,
	})
	if err == nil {
		t.Fatal("RunProviderTLSProbe returned nil error, want runtime_transport_required")
	}
	if !strings.Contains(err.Error(), "runtime_transport_required") {
		t.Fatalf("error = %q, want runtime_transport_required", err.Error())
	}
	if result.RuntimeProfileEnforced {
		t.Fatal("RuntimeProfileEnforced = true, want false")
	}
	if result.RuntimeProfileSource != "" {
		t.Fatalf("RuntimeProfileSource = %q, want empty", result.RuntimeProfileSource)
	}
	if !strings.Contains(result.Error, "http.DefaultTransport") {
		t.Fatalf("result error = %q, want http.DefaultTransport rejection", result.Error)
	}
}

func TestRunProviderTLSProbe_ControlledFingerprintEchoParsesSafeFields(t *testing.T) {
	auth := &cliproxyauth.Auth{
		ID:       "codex-echo",
		FileName: "codex-echo.json",
		Provider: "codex",
		Metadata: map[string]any{
			"account_settings": map[string]any{
				"schema_version":    1,
				"refresh_enabled":   false,
				"transport_profile": nil,
				"tls_profile":       nil,
			},
		},
	}
	cfg := &config.Config{SDKConfig: config.SDKConfig{ProxyURL: "http://proxy.example.test:8080"}}
	rt := &echoProbeRoundTripper{body: `{
		"http_version":"h2",
		"user_agent":"codex-tui/0.128.0",
		"tls":{
			"ja3":"771,4865-4866,0-11-10,29-23,0",
			"ja3_hash":"ja3hash",
			"ja4":"t13d1516h2_abcd_efgh",
			"ja4_r":"t13d1516h2_raw",
			"peetprint":"772-771|2-1.1|29-23",
			"peetprint_hash":"peethash",
			"tls_version_negotiated":"772",
			"client_random":"must-not-be-stored",
			"session_id":"must-not-be-stored"
		},
		"http2":{
			"akamai_fingerprint":"1:65536;4:131072|0|0|m,p,a,s",
			"akamai_fingerprint_hash":"akamaihash",
			"sent_frames":[{"frame_type":"SETTINGS"}]
		},
		"ip":"203.0.113.10"
	}`}

	result, err := RunProviderTLSProbe(context.Background(), cfg, auth, ProviderTLSProbeOptions{
		Timestamp:     time.Date(2026, 5, 7, 15, 0, 0, 0, time.UTC),
		CorrelationID: "corr-echo",
		TargetHost:    "tls.peet.ws",
		Method:        http.MethodGet,
		RoundTripper:  rt,
	})
	if err != nil {
		t.Fatalf("RunProviderTLSProbe returned error: %v", err)
	}
	if rt.request == nil {
		t.Fatal("probe did not issue an outbound echo request")
	}
	if got := rt.request.URL.String(); got != "https://tls.peet.ws/api/all" {
		t.Fatalf("outbound URL = %q, want https://tls.peet.ws/api/all", got)
	}
	if got := rt.request.Header.Get("Authorization"); got != "" {
		t.Fatalf("Authorization header = %q, want empty", got)
	}
	if result.EvidenceType != ProviderTLSProbeEchoEvidenceType {
		t.Fatalf("EvidenceType = %q, want %q", result.EvidenceType, ProviderTLSProbeEchoEvidenceType)
	}
	if result.ClaimScope != ProviderTLSProbeEchoClaimScope {
		t.Fatalf("ClaimScope = %q, want %q", result.ClaimScope, ProviderTLSProbeEchoClaimScope)
	}
	if result.TargetKind != ProviderTLSProbeTargetKindFingerprintEcho {
		t.Fatalf("TargetKind = %q, want echo", result.TargetKind)
	}
	if result.ProviderObserved {
		t.Fatal("ProviderObserved = true, want false for controlled echo")
	}
	if result.EchoFingerprint == nil {
		t.Fatal("EchoFingerprint is nil")
	}
	if result.EchoFingerprint.JA3Hash != "ja3hash" || result.EchoFingerprint.JA4 != "t13d1516h2_abcd_efgh" {
		t.Fatalf("echo tls fingerprint = %#v", result.EchoFingerprint)
	}
	if result.EchoFingerprint.AkamaiFingerprintHash != "akamaihash" || result.EchoFingerprint.HTTPVersion != "h2" {
		t.Fatalf("echo http2 fingerprint = %#v", result.EchoFingerprint)
	}
	if result.ProviderEdgeParityScore.Score < result.ProviderEdgeParityScore.Threshold {
		t.Fatalf("provider edge approximation score = %#v, want pass >= threshold", result.ProviderEdgeParityScore)
	}
	if !result.ProviderEdgeParityScore.Passed {
		t.Fatalf("provider edge approximation passed = false, score=%#v", result.ProviderEdgeParityScore)
	}
	if !strings.Contains(result.ProviderEdgeParityScore.ClaimScope, "not-provider-attestation") {
		t.Fatalf("score claim scope = %q, want non-attestation boundary", result.ProviderEdgeParityScore.ClaimScope)
	}
	payload, errMarshal := json.Marshal(result)
	if errMarshal != nil {
		t.Fatalf("marshal result: %v", errMarshal)
	}
	lower := strings.ToLower(string(payload))
	for _, forbidden := range []string{"must-not-be-stored", "203.0.113.10", "session_id", "client_random", "sent_frames"} {
		if strings.Contains(lower, strings.ToLower(forbidden)) {
			t.Fatalf("echo result leaked %q: %s", forbidden, string(payload))
		}
	}
}

func TestRunProviderTLSProbe_ControlledFingerprintEchoMissingFieldsLowersScore(t *testing.T) {
	auth := &cliproxyauth.Auth{
		ID:       "codex-echo-missing",
		FileName: "codex-echo-missing.json",
		Provider: "codex",
		Metadata: map[string]any{
			"account_settings": map[string]any{
				"schema_version":    1,
				"refresh_enabled":   false,
				"transport_profile": nil,
				"tls_profile":       nil,
			},
		},
	}
	cfg := &config.Config{SDKConfig: config.SDKConfig{ProxyURL: "http://proxy.example.test:8080"}}
	rt := &echoProbeRoundTripper{body: `{"http_version":"http/1.1","tls":{},"http2":{}}`}

	result, err := RunProviderTLSProbe(context.Background(), cfg, auth, ProviderTLSProbeOptions{
		Timestamp:     time.Date(2026, 5, 7, 15, 0, 0, 0, time.UTC),
		CorrelationID: "corr-echo-missing",
		TargetHost:    "tls.peet.ws",
		Method:        http.MethodGet,
		RoundTripper:  rt,
	})
	if err != nil {
		t.Fatalf("RunProviderTLSProbe returned error: %v", err)
	}
	if result.ProviderEdgeParityScore.Score >= result.ProviderEdgeParityScore.Threshold {
		t.Fatalf("provider edge approximation score = %#v, want below threshold when TLS/HTTP2 echo fields are missing", result.ProviderEdgeParityScore)
	}
	if result.ProviderEdgeParityScore.Passed {
		t.Fatalf("provider edge approximation passed = true, want false when TLS/HTTP2 echo fields are missing")
	}
}

func TestProviderTLSProbeSafeTargetHostRejectsUnexpectedHost(t *testing.T) {
	if got, err := ProviderTLSProbeSafeTargetHost("claude", ""); err != nil || got != "api.anthropic.com" {
		t.Fatalf("default claude host = %q, err=%v", got, err)
	}
	if got, err := ProviderTLSProbeSafeTargetHost("codex", "tls.peet.ws"); err != nil || got != "tls.peet.ws" {
		t.Fatalf("echo host = %q, err=%v", got, err)
	}
	if got, err := ProviderTLSProbeSafeTargetHost("gemini", ""); err != nil || got != "cloudcode-pa.googleapis.com" {
		t.Fatalf("default gemini host = %q, err=%v", got, err)
	}
	if _, err := ProviderTLSProbeSafeTargetHost("claude", "evil.example.test"); err == nil {
		t.Fatal("expected unexpected host to be rejected")
	}
	if _, err := ProviderTLSProbeSafeTargetHost("codex", "https://chatgpt.com/"); err == nil {
		t.Fatal("expected URL target_host to be rejected")
	}
	if _, err := ProviderTLSProbeSafeTargetHost("unknown", ""); err == nil {
		t.Fatal("expected unsupported provider to be rejected")
	}
}
