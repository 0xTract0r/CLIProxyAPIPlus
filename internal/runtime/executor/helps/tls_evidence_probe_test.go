package helps

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestBuildTLSEvidenceProbeRoundTripperReusesRuntimeProfileResolution(t *testing.T) {
	auth := &cliproxyauth.Auth{ProxyURL: "direct",
		Provider: "claude",
		Metadata: map[string]any{
			"account_settings": map[string]any{
				"transport_profile": map[string]any{
					"provider":   "claude",
					"family":     "utls",
					"profile_id": "claude_chrome_like_mac_v3",
				},
				"tls_profile": map[string]any{
					"provider":   "claude",
					"family":     "utls",
					"profile_id": "chrome_133",
				},
			},
		},
	}

	rt, profile, limitation, err := BuildTLSEvidenceProbeRoundTripper("direct", auth)
	if err != nil {
		t.Fatalf("BuildTLSEvidenceProbeRoundTripper returned error: %v", err)
	}
	if rt == nil {
		t.Fatal("BuildTLSEvidenceProbeRoundTripper returned nil transport")
	}
	if profile == nil || profile.Provider != "claude" || profile.ProfileID != "claude_utls_chrome_133" || profile.TLSProfileID != "claude_utls_chrome_133" {
		t.Fatalf("profile = %#v, want resolved canonical claude_utls_chrome_133 profile", profile)
	}
	if limitation == "" {
		t.Fatal("expected Claude echo-host diagnostic limitation")
	}
}

func TestBuildTLSEvidenceProbeRoundTripperCodex(t *testing.T) {
	auth := &cliproxyauth.Auth{ProxyURL: "direct",
		Provider: "codex",
		Metadata: map[string]any{
			"account_settings": map[string]any{
				"transport_profile": map[string]any{
					"provider":   "codex",
					"family":     "standard",
					"profile_id": "codex_managed_transport_v1",
					"alpn":       []any{"h2", "http/1.1"},
				},
				"tls_profile": map[string]any{
					"provider":   "codex",
					"family":     "go-tls",
					"profile_id": "codex_go_managed_h2_v1",
				},
			},
		},
	}

	rt, profile, limitation, err := BuildTLSEvidenceProbeRoundTripper("direct", auth)
	if err != nil {
		t.Fatalf("BuildTLSEvidenceProbeRoundTripper returned error: %v", err)
	}
	if rt == nil {
		t.Fatal("BuildTLSEvidenceProbeRoundTripper returned nil transport")
	}
	if profile == nil || profile.Provider != "codex" || profile.ProfileID != "codex_managed_transport_v1" {
		t.Fatalf("profile = %#v, want resolved codex managed profile", profile)
	}
	if limitation != "" {
		t.Fatalf("limitation = %q, want empty", limitation)
	}
}

func TestCaptureSyntheticProviderSNIEvidenceCodex(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	auth := &cliproxyauth.Auth{ProxyURL: "direct",
		ID:       "synthetic-codex",
		Provider: "codex",
		Metadata: map[string]any{
			"account_settings": map[string]any{
				"transport_profile": map[string]any{
					"provider":   "codex",
					"family":     "standard",
					"profile_id": "codex_managed_transport_v1",
					"alpn":       []any{"h2", "http/1.1"},
				},
				"tls_profile": map[string]any{
					"provider":   "codex",
					"family":     "go-tls",
					"profile_id": "codex_go_managed_h2_v1",
				},
			},
		},
	}

	evidence, err := CaptureSyntheticProviderSNIEvidence(ctx, auth, "chatgpt.com")
	if err != nil {
		t.Fatalf("CaptureSyntheticProviderSNIEvidence returned error: %v", err)
	}
	assertSyntheticProviderSNIEvidence(t, evidence, "codex", "chatgpt.com")
	if evidence.HTTP2.Available && len(evidence.HTTP2.Settings) == 0 {
		t.Fatal("http2 settings marked available without settings")
	}
	if !evidence.HTTP2.Available && strings.TrimSpace(evidence.HTTP2.Reason) == "" {
		t.Fatal("http2 settings unavailable without reason")
	}
}

func TestCaptureSyntheticProviderSNIEvidenceCodexHTTP11Preset(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	auth := &cliproxyauth.Auth{ProxyURL: "direct",
		ID:       "synthetic-codex-http11",
		Provider: "codex",
		Metadata: map[string]any{
			"account_settings": map[string]any{
				"transport_profile": map[string]any{
					"provider":   "codex",
					"family":     "standard",
					"profile_id": "codex_managed_transport_v1",
					"alpn":       []any{"h2", "http/1.1"},
				},
				"tls_profile": map[string]any{
					"provider":   "codex",
					"family":     "go-tls",
					"profile_id": "codex_go_http11_v1",
				},
			},
		},
	}

	evidence, err := CaptureSyntheticProviderSNIEvidence(ctx, auth, "chatgpt.com")
	if err != nil {
		t.Fatalf("CaptureSyntheticProviderSNIEvidence returned error: %v", err)
	}
	assertSyntheticProviderSNIEvidence(t, evidence, "codex", "chatgpt.com")
	if evidence.RuntimeProfile == nil || !evidence.RuntimeProfile.ForceHTTP11 {
		t.Fatalf("runtime profile = %#v, want ForceHTTP11", evidence.RuntimeProfile)
	}
	if got := evidence.ALPN.Offered; len(got) != 1 || got[0] != "http/1.1" {
		t.Fatalf("offered ALPN = %#v, want [http/1.1]", got)
	}
	if evidence.ALPN.Negotiated != "http/1.1" {
		t.Fatalf("negotiated ALPN = %q, want http/1.1", evidence.ALPN.Negotiated)
	}
	if evidence.HTTP2.Available || !strings.Contains(evidence.HTTP2.Reason, "http2 was not negotiated") {
		t.Fatalf("HTTP2 evidence = %#v, want unavailable because http2 was not negotiated", evidence.HTTP2)
	}
}

func TestCaptureSyntheticProviderSNIEvidenceClaude(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	auth := &cliproxyauth.Auth{ProxyURL: "direct",
		ID:       "synthetic-claude",
		Provider: "claude",
		Metadata: map[string]any{
			"account_settings": map[string]any{
				"transport_profile": map[string]any{
					"provider":   "claude",
					"family":     "utls",
					"profile_id": "claude_utls_chrome_133",
				},
				"tls_profile": map[string]any{
					"provider":   "claude",
					"family":     "utls",
					"profile_id": "claude_utls_chrome_133",
				},
			},
		},
	}

	evidence, err := CaptureSyntheticProviderSNIEvidence(ctx, auth, "api.anthropic.com")
	if err != nil {
		t.Fatalf("CaptureSyntheticProviderSNIEvidence returned error: %v", err)
	}
	assertSyntheticProviderSNIEvidence(t, evidence, "claude", "api.anthropic.com")
}

func assertSyntheticProviderSNIEvidence(t *testing.T, evidence *SyntheticProviderSNIEvidence, provider, providerHost string) {
	t.Helper()
	if evidence == nil {
		t.Fatal("evidence is nil")
	}
	if evidence.EvidenceType != "synthetic-provider-sni" {
		t.Fatalf("evidence type = %q, want synthetic-provider-sni", evidence.EvidenceType)
	}
	if evidence.Provider != provider {
		t.Fatalf("provider = %q, want %q", evidence.Provider, provider)
	}
	if evidence.ProviderHost != providerHost || evidence.ProviderSNI != providerHost || evidence.TLS.ServerName != providerHost {
		t.Fatalf("provider host/SNI mismatch: host=%q provider_sni=%q tls_sni=%q", evidence.ProviderHost, evidence.ProviderSNI, evidence.TLS.ServerName)
	}
	if !strings.Contains(evidence.ProviderHostClaim, "synthetic-provider-sni") || strings.Contains(evidence.ProviderHostClaim, "provider-observed") {
		t.Fatalf("provider_host_claim = %q, want synthetic marker without provider-observed claim", evidence.ProviderHostClaim)
	}
	if !evidence.TLS.ClientHelloCaptured {
		t.Fatal("ClientHello was not captured")
	}
	if evidence.JA3.Hash == "" || evidence.JA3.String == "" {
		t.Fatalf("JA3 evidence incomplete: %#v", evidence.JA3)
	}
	if evidence.JA4.Value == "" {
		t.Fatalf("JA4 evidence incomplete: %#v", evidence.JA4)
	}
	if len(evidence.ALPN.Offered) == 0 {
		t.Fatal("expected offered ALPN protocols")
	}

	data, err := json.Marshal(evidence)
	if err != nil {
		t.Fatalf("marshal evidence: %v", err)
	}
	var schema map[string]any
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatalf("unmarshal evidence schema: %v", err)
	}
	runtimeProfile, ok := schema["runtime_profile"].(map[string]any)
	if !ok {
		t.Fatalf("runtime_profile missing from evidence schema: %s", string(data))
	}
	if _, ok := runtimeProfile["profile_id"]; !ok {
		t.Fatalf("runtime_profile.profile_id missing from evidence schema: %s", string(data))
	}
	lower := strings.ToLower(string(data))
	for _, secretMarker := range []string{"access_token", "refresh_token", "authorization", "bearer "} {
		if strings.Contains(lower, secretMarker) {
			t.Fatalf("evidence JSON leaked secret marker %q: %s", secretMarker, string(data))
		}
	}
}
