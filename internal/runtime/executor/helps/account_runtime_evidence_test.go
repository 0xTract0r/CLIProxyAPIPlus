package helps

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestBuildAccountRuntimeEvidence_CodexA02StyleManagedHeadersAndFallbackTLS(t *testing.T) {
	resetCodexClientProfileCache()
	timestamp := time.Date(2026, 4, 30, 10, 0, 0, 0, time.UTC)
	auth := &cliproxyauth.Auth{ProxyURL: "direct",
		ID:       "codex-a02",
		FileName: "codex-a02.json",
		Provider: "codex",
		Metadata: map[string]any{
			"auth_method": "oauth",
			"email":       "codex-a02@example.test",
			"account_settings": map[string]any{
				"schema_version":  1,
				"refresh_enabled": false,
				"transport_profile": map[string]any{
					"provider": "codex",
					"preset":   "provider-default",
				},
			},
		},
	}
	cfg := &config.Config{
		CodexHeaderDefaults: config.CodexHeaderDefaults{
			UserAgent: "codex_cli_rs/0.125.0 (Mac OS 26.3.1; arm64) iTerm.app/3.6.9 (codex_cli_rs; 0.125.0)",
		},
	}

	evidence := BuildAccountRuntimeEvidence(context.Background(), cfg, auth, AccountRuntimeEvidenceOptions{
		Timestamp:     timestamp,
		CorrelationID: "req-a02",
		BaseURLHost:   "chatgpt.com",
	})

	if evidence.EvidenceType != AccountRuntimeEvidenceType {
		t.Fatalf("EvidenceType = %q, want %q", evidence.EvidenceType, AccountRuntimeEvidenceType)
	}
	if evidence.ClaimScope != AccountRuntimeClaimScope {
		t.Fatalf("ClaimScope = %q, want %q", evidence.ClaimScope, AccountRuntimeClaimScope)
	}
	if evidence.ProviderObserved {
		t.Fatal("account-runtime evidence must not claim provider observation")
	}
	if evidence.Provider != "codex" {
		t.Fatalf("Provider = %q, want codex", evidence.Provider)
	}
	if evidence.RefreshEnabled {
		t.Fatal("RefreshEnabled = true, want false")
	}
	if evidence.ManagedHeaders.PolicyVersion != "codex-managed/v2" {
		t.Fatalf("managed policy = %q, want codex-managed/v2", evidence.ManagedHeaders.PolicyVersion)
	}
	if evidence.ManagedHeaders.Version != "0.125.0" {
		t.Fatalf("managed version = %q, want 0.125.0", evidence.ManagedHeaders.Version)
	}
	if evidence.ManagedHeaders.Strategy != "core-managed/structured-account-settings" {
		t.Fatalf("managed strategy = %q", evidence.ManagedHeaders.Strategy)
	}
	assertManagedHeaderDigestNames(t, evidence.ManagedHeaders.Headers, []string{
		"Accept-Encoding",
		"Accept-Language",
		"Originator",
		"User-Agent",
		"Version",
		"sec-ch-ua",
		"sec-ch-ua-mobile",
		"sec-ch-ua-platform",
		"sec-fetch-dest",
		"sec-fetch-mode",
		"sec-fetch-site",
	})
	if evidence.TransportProfile.ProfileID != "provider-default" || !evidence.TransportProfile.RuntimeEnforced {
		t.Fatalf("transport evidence = %#v, want provider-default runtime-enforced", evidence.TransportProfile)
	}
	if evidence.TLSProfile.Configured || evidence.TLSProfile.ProfileID != "current-fallback" {
		t.Fatalf("tls evidence = %#v, want unconfigured current fallback", evidence.TLSProfile)
	}
	if evidence.HTTPVersion.Version != "go-default-auto" || evidence.HTTPVersion.Policy != "current-fallback" {
		t.Fatalf("http version = %#v, want current fallback", evidence.HTTPVersion)
	}

	payload, err := json.Marshal(evidence)
	if err != nil {
		t.Fatalf("failed to marshal evidence: %v", err)
	}
	for _, forbidden := range []string{
		"codex_cli_rs/0.125.0",
		"codex_cli_rs",
		"0.125.0 (Mac OS",
		"codex-a02@example.test",
		"codex-a02.json",
	} {
		if strings.Contains(string(payload), forbidden) {
			t.Fatalf("evidence leaked raw value %q in %s", forbidden, string(payload))
		}
	}
}

func TestBuildAccountRuntimeEvidence_ClaudeUsesClaudeManagedPolicyAndUTLSProfile(t *testing.T) {
	ResetClaudeDeviceProfileCache()
	auth := &cliproxyauth.Auth{ProxyURL: "direct",
		ID:       "claude-runtime",
		FileName: "claude-runtime.json",
		Provider: "claude",
		Metadata: map[string]any{
			"auth_method": "oauth",
			"email":       "claude-runtime@example.test",
			"account_settings": map[string]any{
				"schema_version": 1,
				"transport_profile": map[string]any{
					"provider": "claude",
					"preset":   "claude_chrome_like_mac_v2",
				},
				"tls_profile": map[string]any{
					"provider": "claude",
					"preset":   "claude_chrome_like_mac_v2",
				},
			},
		},
	}
	cfg := &config.Config{
		ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{
			UserAgent:      "claude-cli/2.1.123 (external, cli)",
			PackageVersion: "0.74.0",
			RuntimeVersion: "v24.5.0",
			Timeout:        "600",
		},
	}

	evidence := BuildAccountRuntimeEvidence(context.Background(), cfg, auth, AccountRuntimeEvidenceOptions{
		CorrelationID: "req-claude",
		BaseURLHost:   "api.anthropic.com",
	})

	if evidence.Provider != "claude" {
		t.Fatalf("Provider = %q, want claude", evidence.Provider)
	}
	if evidence.ManagedHeaders.PolicyVersion != "claude-managed/v2" {
		t.Fatalf("managed policy = %q, want claude-managed/v2", evidence.ManagedHeaders.PolicyVersion)
	}
	if evidence.ManagedHeaders.PolicyVersion == "codex-managed/v2" {
		t.Fatal("claude evidence must not use codex managed policy")
	}
	if evidence.ManagedHeaders.Version != "2.1.123" {
		t.Fatalf("managed version = %q, want 2.1.123", evidence.ManagedHeaders.Version)
	}
	assertManagedHeaderDigestNames(t, evidence.ManagedHeaders.Headers, []string{
		"User-Agent",
		"X-App",
		"X-Stainless-Package-Version",
		"X-Stainless-Runtime-Version",
		"X-Stainless-Timeout",
	})
	if evidence.TransportProfile.Provider != "claude" || evidence.TransportProfile.Family != "utls" || !evidence.TransportProfile.RuntimeEnforced {
		t.Fatalf("transport evidence = %#v, want claude utls runtime enforcement", evidence.TransportProfile)
	}
	if evidence.TLSProfile.Provider != "claude" || evidence.TLSProfile.Family != "utls" || !evidence.TLSProfile.RuntimeEnforced {
		t.Fatalf("tls evidence = %#v, want claude utls runtime enforcement", evidence.TLSProfile)
	}
	for _, digest := range evidence.ManagedHeaders.Headers {
		if digest.Name == "Originator" || digest.Name == "Version" {
			t.Fatalf("claude managed header digests should not include codex header %q", digest.Name)
		}
	}
}

func assertManagedHeaderDigestNames(t *testing.T, digests []AccountRuntimeManagedHeaderDigest, want []string) {
	t.Helper()
	if len(digests) != len(want) {
		t.Fatalf("managed header digest count = %d, want %d: %#v", len(digests), len(want), digests)
	}
	got := make([]string, 0, len(digests))
	for _, digest := range digests {
		got = append(got, digest.Name)
		if !strings.HasPrefix(digest.ValueSHA256, "sha256:") || len(digest.ValueSHA256) != len("sha256:")+64 {
			t.Fatalf("digest for %s = %q, want sha256 hex", digest.Name, digest.ValueSHA256)
		}
	}
	for idx, name := range want {
		if got[idx] != name {
			t.Fatalf("managed header digest names = %#v, want %#v", got, want)
		}
	}
}
