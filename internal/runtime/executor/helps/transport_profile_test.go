package helps

import (
	"context"
	"net/http"
	"strings"
	"testing"

	tls "github.com/refraction-networking/utls"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// assertClaudeCLIClientHelloRoundTripper verifies that the given round tripper
// is the uTLS replicated claude-cli ClientHello transport: a fallback round
// tripper whose protected-host path uses a utlsRoundTripper pinned to
// HelloCustom (newClaudeCLIClientHelloSpec) with ALPN http/1.1.
func assertClaudeCLIClientHelloRoundTripper(t *testing.T, rt http.RoundTripper) {
	t.Helper()
	fallback, ok := rt.(*fallbackRoundTripper)
	if !ok {
		t.Fatalf("claude default round tripper = %T, want *fallbackRoundTripper (uTLS claude-cli ClientHello)", rt)
	}
	utlsRT, ok := fallback.utls.(*utlsRoundTripper)
	if !ok {
		t.Fatalf("claude default protected transport = %T, want *utlsRoundTripper", fallback.utls)
	}
	if utlsRT.clientHello.Str() != tls.HelloCustom.Str() {
		t.Fatalf("claude default ClientHello = %s, want HelloCustom", utlsRT.clientHello.Str())
	}
	spec, err := utlsRT.clientHelloSpec(utlsRT.clientHello)
	if err != nil {
		t.Fatalf("claude default clientHelloSpec: %v", err)
	}
	alpn := alpnProtocols(spec)
	if len(alpn) != 1 || alpn[0] != "http/1.1" {
		t.Fatalf("claude default ALPN = %v, want [http/1.1]", alpn)
	}
}

func TestIsRuntimeTransportProfileEnforced_ClaudePreset(t *testing.T) {
	auth := &cliproxyauth.Auth{
		ID:       "claude-a",
		Provider: "claude",
		Metadata: map[string]any{
			"account_settings": map[string]any{
				"transport_profile": map[string]any{
					"preset": "claude_chrome_like_mac_v2",
				},
			},
		},
	}

	if !IsRuntimeTransportProfileEnforced(auth) {
		t.Fatal("expected claude transport_profile preset to be runtime-enforced")
	}
}

func TestIsRuntimeTransportProfileEnforced_CodexPreset(t *testing.T) {
	auth := &cliproxyauth.Auth{
		ID:       "codex-a",
		Provider: "codex",
		Metadata: map[string]any{
			"account_settings": map[string]any{
				"transport_profile": map[string]any{
					"preset": "provider-default",
					"alpn":   []string{"h2"},
				},
			},
		},
	}

	if !IsRuntimeTransportProfileEnforced(auth) {
		t.Fatal("expected codex transport_profile preset to be runtime-enforced")
	}
}

func TestIsRuntimeTLSProfileEnforced_ClaudePreset(t *testing.T) {
	auth := &cliproxyauth.Auth{
		ID:       "claude-tls",
		Provider: "claude",
		Metadata: map[string]any{
			"account_settings": map[string]any{
				"tls_profile": map[string]any{
					"preset": "claude_utls_chrome_133",
				},
			},
		},
	}

	if !IsRuntimeTLSProfileEnforced(auth) {
		t.Fatal("expected claude tls_profile preset to be runtime-enforced")
	}
	if IsRuntimeTransportProfileEnforced(auth) {
		t.Fatal("expected tls-only profile not to be reported as transport_profile enforcement")
	}
	client := NewProxyAwareHTTPClient(context.Background(), nil, auth, 0)
	if client == nil || client.Transport == nil {
		t.Fatal("expected tls-profile client to be created")
	}
	if _, ok := client.Transport.(*fallbackRoundTripper); !ok {
		t.Fatalf("transport type = %T, want *fallbackRoundTripper", client.Transport)
	}
}

func TestRuntimeTransportProfile_ClaudeChrome133AliasesCanonicalize(t *testing.T) {
	for _, tc := range []struct {
		name       string
		profileID  string
		tlsProfile string
	}{
		{name: "canonical", profileID: "claude_utls_chrome_133", tlsProfile: "claude_utls_chrome_133"},
		{name: "old project alias", profileID: "claude_chrome_like_mac_v3", tlsProfile: "claude_chrome_like_mac_v3"},
		{name: "short utls alias", profileID: "chrome_133", tlsProfile: "chrome_133"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			auth := &cliproxyauth.Auth{
				ID:       "claude-alias-" + tc.name,
				Provider: "claude",
				Metadata: map[string]any{
					"account_settings": map[string]any{
						"transport_profile": map[string]any{
							"preset": tc.profileID,
						},
						"tls_profile": map[string]any{
							"preset": tc.tlsProfile,
						},
					},
				},
			}

			profile := ResolveRuntimeTransportProfile(auth)
			if profile == nil {
				t.Fatal("expected claude profile to resolve")
			}
			if profile.ProfileID != "claude_utls_chrome_133" || profile.TLSProfileID != "claude_utls_chrome_133" {
				t.Fatalf("profile IDs = (%q, %q), want canonical claude_utls_chrome_133", profile.ProfileID, profile.TLSProfileID)
			}
			if !profile.SupportsRuntime() {
				t.Fatalf("expected canonicalized profile to be runtime-supported: %#v", profile)
			}
		})
	}
}

func TestRuntimeTransportProfile_ClaudeReqwestRustlsAliasesCanonicalize(t *testing.T) {
	for _, tc := range []struct {
		name      string
		profileID string
	}{
		{name: "canonical", profileID: "claude_reqwest_rustls_compatible_v1"},
		{name: "claude code cli alias", profileID: "claude_code_cli_v1"},
		{name: "claw code alias", profileID: "claw_code_reqwest_rustls_v1"},
		{name: "short reqwest alias", profileID: "claude_reqwest_rustls_v1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			auth := &cliproxyauth.Auth{
				ID:       "claude-reqwest-" + tc.name,
				Provider: "claude",
				Metadata: map[string]any{
					"account_settings": map[string]any{
						"transport_profile": map[string]any{
							"preset": tc.profileID,
						},
					},
				},
			}

			profile := ResolveRuntimeTransportProfile(auth)
			if profile == nil {
				t.Fatal("expected claude reqwest-compatible profile to resolve")
			}
			if profile.ProfileID != "claude_reqwest_rustls_compatible_v1" || profile.TLSProfileID != "claude_reqwest_rustls_compatible_v1" {
				t.Fatalf("profile IDs = (%q, %q), want claude_reqwest_rustls_compatible_v1", profile.ProfileID, profile.TLSProfileID)
			}
			if profile.Family != "claude-reqwest-compatible" || profile.TLSFamily != "rustls-compatible" {
				t.Fatalf("families = (%q, %q), want claude-reqwest-compatible/rustls-compatible", profile.Family, profile.TLSFamily)
			}
			if !profile.SupportsRuntime() {
				t.Fatalf("expected reqwest-compatible profile to be runtime-supported: %#v", profile)
			}
			if transport, ok := BuildRuntimeTransportRoundTripper("", auth); !ok || transport == nil {
				t.Fatalf("expected executable Claude reqwest-compatible transport, got %T ok=%v", transport, ok)
			}
			if _, ok := resolveClaudeClientHelloID(profile.ProfileID); ok {
				t.Fatalf("reqwest-compatible profile should not resolve to a browser uTLS ClientHello")
			}
		})
	}
}

func TestRuntimeTransportProfile_ClaudeProviderDefaultDoesNotOptIntoChromeLikeUTLS(t *testing.T) {
	auth := &cliproxyauth.Auth{
		ID:       "claude-provider-default",
		Provider: "claude",
		Metadata: map[string]any{
			"account_settings": map[string]any{
				"transport_profile": map[string]any{
					"preset": "provider-default",
				},
				"tls_profile": map[string]any{
					"preset": "provider-default",
				},
			},
		},
	}

	profile := ResolveRuntimeTransportProfile(auth)
	if profile == nil {
		t.Fatal("expected claude provider-default profile to resolve as configured metadata")
	}
	if !profile.SupportsRuntime() {
		t.Fatalf("provider-default should opt Claude into CLI-native account isolation: %#v", profile)
	}
	if profile.Family != "cli-native" || profile.TLSFamily != "runtime-native" {
		t.Fatalf("provider-default families = (%q, %q), want cli-native/runtime-native", profile.Family, profile.TLSFamily)
	}
	if _, ok := resolveClaudeClientHelloID(profile.ProfileID); ok {
		t.Fatalf("provider-default should not resolve to a Chrome-like Claude uTLS ClientHello")
	}
	if transport, ok := BuildRuntimeTransportRoundTripper("", auth); !ok || transport == nil {
		t.Fatalf("provider-default should build CLI-native isolated transport, got %T ok=%v", transport, ok)
	}
	enforced, status := RuntimeTransportProfileStatus(auth)
	if !enforced {
		t.Fatalf("provider-default should be runtime-enforced for CLI-native isolation: %s", status)
	}
	if !strings.Contains(status, "CLI-native") {
		t.Fatalf("status = %q, want CLI-native isolation", status)
	}
}

func TestRuntimeTransportProfile_CoreManagedAccountIdentityForEmptyCLIProvider(t *testing.T) {
	for _, tc := range []struct {
		name      string
		provider  string
		profileID string
	}{
		// claude default (no tls_profile) now replicates the real claude-cli
		// ClientHello via uTLS HelloCustom, not the prior reqwest/rustls preset.
		{name: "claude", provider: "claude", profileID: "claude_cli_clienthello_v1"},
		{name: "codex", provider: "codex", profileID: "codex_proxy_compatible_v1"},
		{name: "gemini", provider: "gemini", profileID: "gemini_cli_native_v1"},
		{name: "gemini cli", provider: "gemini-cli", profileID: "gemini_cli_native_v1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			auth := &cliproxyauth.Auth{
				ID:       tc.provider + "-account",
				Provider: tc.provider,
			}

			profile := ResolveRuntimeTransportProfile(auth)
			if profile == nil {
				t.Fatal("expected core-managed account runtime identity")
			}
			if !profile.CoreManaged || profile.Source != "core-managed-account-runtime" {
				t.Fatalf("profile source = %#v, want core-managed account runtime", profile)
			}
			if profile.ProfileID != tc.profileID || profile.TLSProfileID != tc.profileID {
				t.Fatalf("profile IDs = (%q, %q), want %q", profile.ProfileID, profile.TLSProfileID, tc.profileID)
			}
			if tc.provider == "codex" && (profile.Family != "codex-proxy-compatible" || profile.TLSFamily != "rustls-compatible") {
				t.Fatalf("codex managed families = (%q, %q), want codex-proxy-compatible/rustls-compatible", profile.Family, profile.TLSFamily)
			}
			if tc.provider == "claude" && (profile.Family != "utls" || profile.TLSFamily != "utls") {
				t.Fatalf("claude managed families = (%q, %q), want utls/utls", profile.Family, profile.TLSFamily)
			}
			if !profile.SupportsRuntime() || !profile.SupportsTransportRuntime() || !profile.SupportsTLSRuntime() {
				t.Fatalf("expected managed profile to support runtime: %#v", profile)
			}
			transport, ok := BuildRuntimeTransportRoundTripper("", auth)
			if !ok || transport == nil {
				t.Fatalf("expected executable runtime transport, got %T ok=%v", transport, ok)
			}
			if tc.provider == "claude" {
				// The claude default must route through the uTLS replicated
				// claude-cli ClientHello (HelloCustom + ALPN http/1.1), not the
				// standard Go transport used by the prior reqwest/rustls preset.
				assertClaudeCLIClientHelloRoundTripper(t, transport)
			}
		})
	}
}

func TestRuntimeTransportProfile_ClaudePerAccountTLSProfileOverridesDefault(t *testing.T) {
	// An explicit per-account tls_profile must win over the new claude-cli
	// HelloCustom default; the default only applies when no profile is set.
	auth := &cliproxyauth.Auth{
		ID:       "claude-explicit-tls",
		Provider: "claude",
		Metadata: map[string]any{
			"account_settings": map[string]any{
				"tls_profile": map[string]any{
					"preset": "claude_utls_chrome_133",
				},
			},
		},
	}

	profile := ResolveRuntimeTransportProfile(auth)
	if profile == nil {
		t.Fatal("expected explicit claude tls_profile to resolve")
	}
	if profile.CoreManaged {
		t.Fatalf("explicit tls_profile must not be core-managed default: %#v", profile)
	}
	if profile.TLSProfileID != "claude_utls_chrome_133" {
		t.Fatalf("TLSProfileID = %q, want claude_utls_chrome_133 (per-account override)", profile.TLSProfileID)
	}
	if profile.ProfileID == "claude_cli_clienthello_v1" || profile.TLSProfileID == "claude_cli_clienthello_v1" {
		t.Fatalf("explicit tls_profile must not be overridden by claude-cli default: %#v", profile)
	}

	transport, ok := BuildRuntimeTransportRoundTripper("", auth)
	if !ok || transport == nil {
		t.Fatalf("expected executable transport for explicit tls_profile, got %T ok=%v", transport, ok)
	}
	fallback, ok := transport.(*fallbackRoundTripper)
	if !ok {
		t.Fatalf("explicit chrome tls_profile transport = %T, want *fallbackRoundTripper", transport)
	}
	utlsRT, ok := fallback.utls.(*utlsRoundTripper)
	if !ok {
		t.Fatalf("explicit chrome tls_profile protected transport = %T, want *utlsRoundTripper", fallback.utls)
	}
	if utlsRT.clientHello.Str() != tls.HelloChrome_133.Str() {
		t.Fatalf("explicit chrome tls_profile ClientHello = %s, want HelloChrome_133", utlsRT.clientHello.Str())
	}
}

func TestRuntimeTransportProfile_CodexProxyCompatiblePresetUsesCommunityFamilies(t *testing.T) {
	auth := &cliproxyauth.Auth{
		ID:       "codex-proxy-compatible",
		Provider: "codex",
		Metadata: map[string]any{
			"account_settings": map[string]any{
				"transport_profile": map[string]any{
					"preset": "codex_proxy_compatible_v1",
				},
				"tls_profile": map[string]any{
					"preset": "codex_proxy_compatible_v1",
				},
			},
		},
	}

	profile := ResolveRuntimeTransportProfile(auth)
	if profile == nil {
		t.Fatal("expected codex-proxy-compatible preset to resolve")
	}
	if profile.ProfileID != "codex_proxy_compatible_v1" || profile.TLSProfileID != "codex_proxy_compatible_v1" {
		t.Fatalf("profile IDs = (%q, %q), want codex_proxy_compatible_v1", profile.ProfileID, profile.TLSProfileID)
	}
	if profile.Family != "codex-proxy-compatible" || profile.TLSFamily != "rustls-compatible" {
		t.Fatalf("families = (%q, %q), want codex-proxy-compatible/rustls-compatible", profile.Family, profile.TLSFamily)
	}
	if !profile.SupportsRuntime() || !profile.SupportsTransportRuntime() || !profile.SupportsTLSRuntime() {
		t.Fatalf("expected community profile to support runtime: %#v", profile)
	}
	if transport, ok := BuildRuntimeTransportRoundTripper("", auth); !ok || transport == nil {
		t.Fatalf("expected executable Codex-Proxy-compatible transport, got %T ok=%v", transport, ok)
	}
	enforced, status := RuntimeTransportProfileStatus(auth)
	if !enforced {
		t.Fatalf("expected runtime-enforced profile: %s", status)
	}
	if !strings.Contains(status, "codex_proxy_compatible_v1") {
		t.Fatalf("status = %q, want explicit community profile ID", status)
	}
}

func TestRuntimeTransportProfile_CoreManagedCacheKeyIsAccountIsolated(t *testing.T) {
	authA := &cliproxyauth.Auth{
		ID:       "claude-file-a",
		Provider: "claude",
		Metadata: map[string]any{
			"auth_method": "oauth",
			"email":       "claude-a@example.com",
		},
	}
	authB := authA.Clone()
	authB.ID = "claude-file-b"
	authB.Metadata["email"] = "claude-b@example.com"

	keyA := RuntimeTransportProfileCacheKey("http://shared-proxy:8080", authA)
	keyB := RuntimeTransportProfileCacheKey("http://shared-proxy:8080", authB)

	if keyA == "" || keyB == "" {
		t.Fatalf("expected managed cache keys, got %q and %q", keyA, keyB)
	}
	if keyA == keyB {
		t.Fatalf("expected core-managed cache keys to differ by auth/account, got %q", keyA)
	}
	for _, want := range []string{"auth=claude-file-a", "account=oauth:claude-a@example.com", "base=api.anthropic.com", "proxy=http://shared-proxy:8080", "claude_cli_clienthello_v1", "core-managed-account-runtime"} {
		if !strings.Contains(keyA, want) {
			t.Fatalf("cache key %q does not contain %q", keyA, want)
		}
	}
}

func TestResolveClaudeClientHelloID_DoesNotTreatProviderDefaultAsChrome(t *testing.T) {
	for _, profileID := range []string{"", "provider-default"} {
		if _, ok := resolveClaudeClientHelloID(profileID); ok {
			t.Fatalf("%q should not resolve to a Chrome-like Claude uTLS ClientHello", profileID)
		}
	}
	if _, ok := resolveClaudeClientHelloID("claude_utls_chrome_133"); !ok {
		t.Fatal("canonical claude_utls_chrome_133 should resolve")
	}
}

func TestIsRuntimeTLSProfileEnforced_CodexHTTP11Preset(t *testing.T) {
	auth := &cliproxyauth.Auth{
		ID:       "codex-tls",
		Provider: "codex",
		Metadata: map[string]any{
			"account_settings": map[string]any{
				"tls_profile": map[string]any{
					"preset": "codex_go_http11_v1",
				},
			},
		},
	}

	if !IsRuntimeTLSProfileEnforced(auth) {
		t.Fatal("expected codex tls_profile preset to be runtime-enforced")
	}
	profile := ResolveRuntimeTransportProfile(auth)
	if profile == nil {
		t.Fatal("expected codex tls_profile preset to resolve")
	}
	if !profile.ForceHTTP11 {
		t.Fatal("expected codex_go_http11_v1 preset to imply ForceHTTP11")
	}
	if got := profile.ALPN; len(got) != 1 || got[0] != "http/1.1" {
		t.Fatalf("profile ALPN = %#v, want [http/1.1]", got)
	}
	client := NewProxyAwareHTTPClient(context.Background(), nil, auth, 0)
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", client.Transport)
	}
	if transport.ForceAttemptHTTP2 {
		t.Fatal("expected codex_go_http11_v1 to disable ForceAttemptHTTP2")
	}
	if got := transport.TLSClientConfig.NextProtos; len(got) != 1 || got[0] != "http/1.1" {
		t.Fatalf("NextProtos = %#v, want [http/1.1]", got)
	}
}

func TestRuntimeTLSProfile_CodexH2PresetDoesNotForceHTTP11(t *testing.T) {
	auth := &cliproxyauth.Auth{
		ID:       "codex-h2-tls",
		Provider: "codex",
		Metadata: map[string]any{
			"account_settings": map[string]any{
				"transport_profile": map[string]any{
					"preset": "codex_managed_transport_v1",
					"alpn":   []any{"h2", "http/1.1"},
				},
				"tls_profile": map[string]any{
					"preset": "codex_go_managed_h2_v1",
				},
			},
		},
	}

	profile := ResolveRuntimeTransportProfile(auth)
	if profile == nil {
		t.Fatal("expected codex h2 tls_profile preset to resolve")
	}
	if profile.ForceHTTP11 {
		t.Fatal("expected codex h2 tls_profile preset not to imply ForceHTTP11")
	}
	client := NewProxyAwareHTTPClient(context.Background(), nil, auth, 0)
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", client.Transport)
	}
	if !transport.ForceAttemptHTTP2 {
		t.Fatal("expected codex h2 tls_profile preset to keep ForceAttemptHTTP2 enabled")
	}
	if got := transport.TLSClientConfig.NextProtos; len(got) != 2 || got[0] != "h2" || got[1] != "http/1.1" {
		t.Fatalf("NextProtos = %#v, want [h2 http/1.1]", got)
	}
}

func TestRuntimeTLSProfile_ClaudePresetDoesNotForceHTTP11(t *testing.T) {
	auth := &cliproxyauth.Auth{
		ID:       "claude-tls-http11-guard",
		Provider: "claude",
		Metadata: map[string]any{
			"account_settings": map[string]any{
				"tls_profile": map[string]any{
					"preset": "chrome_133",
				},
			},
		},
	}

	profile := ResolveRuntimeTransportProfile(auth)
	if profile == nil {
		t.Fatal("expected claude tls_profile preset to resolve")
	}
	if profile.ForceHTTP11 {
		t.Fatal("expected claude tls_profile preset not to imply ForceHTTP11")
	}
	if len(profile.ALPN) != 0 {
		t.Fatalf("expected claude tls_profile preset not to rewrite ALPN, got %#v", profile.ALPN)
	}
}

func TestRuntimeTransportProfileCacheKey_IncludesAuthAndProfile(t *testing.T) {
	authA := &cliproxyauth.Auth{
		ID:       "claude-a",
		Provider: "claude",
		Metadata: map[string]any{
			"account_settings": map[string]any{
				"transport_profile": map[string]any{
					"preset": "claude_chrome_like_mac_v1",
				},
			},
		},
	}
	authB := authA.Clone()
	authB.ID = "claude-b"

	keyA := RuntimeTransportProfileCacheKey("http://shared-proxy:8080", authA)
	keyB := RuntimeTransportProfileCacheKey("http://shared-proxy:8080", authB)

	if keyA == "" {
		t.Fatal("expected non-empty transport cache key for supported profile")
	}
	if keyA == keyB {
		t.Fatalf("expected cache key to differ by auth ID, got %q", keyA)
	}
}

func TestRuntimeTransportProfileCacheKey_IncludesAccount(t *testing.T) {
	authA := &cliproxyauth.Auth{
		ID:       "shared-auth-id",
		Provider: "claude",
		Metadata: map[string]any{
			"auth_method": "oauth",
			"email":       "account-a@example.com",
			"account_settings": map[string]any{
				"transport_profile": map[string]any{
					"preset": "provider-default",
				},
				"tls_profile": map[string]any{
					"preset": "claude_chrome_like_mac_v1",
				},
			},
		},
	}
	authB := authA.Clone()
	authB.Metadata["email"] = "account-b@example.com"

	keyA := RuntimeTransportProfileCacheKey("http://shared-proxy:8080", authA)
	keyB := RuntimeTransportProfileCacheKey("http://shared-proxy:8080", authB)

	if keyA == "" || keyB == "" {
		t.Fatalf("expected non-empty transport cache keys, got %q and %q", keyA, keyB)
	}
	if keyA == keyB {
		t.Fatalf("expected cache keys to differ by account, got %q", keyA)
	}
	for _, want := range []string{"account=oauth:account-a@example.com", "base=api.anthropic.com", "proxy=http://shared-proxy:8080", "claude_chrome_like_mac_v1"} {
		if !strings.Contains(keyA, want) {
			t.Fatalf("cache key %q does not contain %q", keyA, want)
		}
	}
}

func TestRuntimeTransportProfileCacheKey_IncludesBaseURLHost(t *testing.T) {
	authA := &cliproxyauth.Auth{
		ID:       "claude-same-auth",
		Provider: "claude",
		Attributes: map[string]string{
			"base_url": "https://api.anthropic.com/v1",
		},
		Metadata: map[string]any{
			"account_settings": map[string]any{
				"transport_profile": map[string]any{"preset": "claude_utls_chrome_133"},
			},
		},
	}
	authB := authA.Clone()
	authB.Attributes["base_url"] = "https://gateway.example.com/anthropic"

	keyA := RuntimeTransportProfileCacheKey("http://shared-proxy:8080", authA)
	keyB := RuntimeTransportProfileCacheKey("http://shared-proxy:8080", authB)

	if keyA == "" || keyB == "" {
		t.Fatalf("expected non-empty transport cache keys, got %q and %q", keyA, keyB)
	}
	if keyA == keyB {
		t.Fatalf("expected cache keys to differ by base URL host, got %q", keyA)
	}
	if !strings.Contains(keyA, "base=api.anthropic.com") || !strings.Contains(keyB, "base=gateway.example.com") {
		t.Fatalf("cache keys should include normalized base hosts, got %q and %q", keyA, keyB)
	}
}

func TestRuntimeTransportProfileCacheKey_IncludesTLSProfile(t *testing.T) {
	authA := &cliproxyauth.Auth{
		ID:       "claude-same-auth",
		Provider: "claude",
		Metadata: map[string]any{
			"account_settings": map[string]any{
				"transport_profile": map[string]any{"preset": "provider-default"},
				"tls_profile":       map[string]any{"preset": "claude_chrome_like_mac_v1"},
			},
		},
	}
	authB := authA.Clone()
	authB.Metadata["account_settings"] = map[string]any{
		"transport_profile": map[string]any{"preset": "provider-default"},
		"tls_profile":       map[string]any{"preset": "claude_chrome_like_mac_v2"},
	}

	keyA := RuntimeTransportProfileCacheKey("http://shared-proxy:8080", authA)
	keyB := RuntimeTransportProfileCacheKey("http://shared-proxy:8080", authB)

	if keyA == "" || keyB == "" {
		t.Fatalf("expected non-empty transport cache keys, got %q and %q", keyA, keyB)
	}
	if keyA == keyB {
		t.Fatalf("expected cache keys to differ by TLS profile, got %q", keyA)
	}
}

func TestRuntimeTransportProfileRejectsProviderMismatch(t *testing.T) {
	auth := &cliproxyauth.Auth{
		ID:       "claude-not-codex",
		Provider: "claude",
		Metadata: map[string]any{
			"account_settings": map[string]any{
				"tls_profile": map[string]any{
					"provider":     "codex",
					"preset":       "codex_go_http11_v1",
					"force_http11": true,
				},
			},
		},
	}

	profile := ResolveRuntimeTransportProfile(auth)
	if profile == nil {
		t.Fatal("expected profile to resolve for mismatch diagnostics")
	}
	if !profile.ProviderMismatch {
		t.Fatal("expected provider mismatch to be recorded")
	}
	if profile.Provider != "claude" {
		t.Fatalf("provider = %q, want true auth provider claude", profile.Provider)
	}
	if IsRuntimeTLSProfileEnforced(auth) || IsRuntimeTransportProfileEnforced(auth) {
		t.Fatal("expected mismatched codex profile on claude auth to be rejected")
	}
	enforced, status := RuntimeTransportProfileStatus(auth)
	if enforced {
		t.Fatalf("expected mismatch profile not to be enforced: %s", status)
	}
	if !strings.Contains(status, "different provider") || !strings.Contains(status, "falling back") {
		t.Fatalf("expected explicit provider mismatch fallback status, got %q", status)
	}
	if key := RuntimeTransportProfileCacheKey("http://shared-proxy:8080", auth); key != "" {
		t.Fatalf("expected no cache key for rejected provider mismatch, got %q", key)
	}
}

func TestNewProxyAwareHTTPClient_IsolatesSameProxyDifferentAccount(t *testing.T) {
	authA := &cliproxyauth.Auth{
		Provider: "claude",
		ProxyURL: "http://shared-proxy:8080",
		Metadata: map[string]any{
			"auth_method": "oauth",
			"email":       "account-a@example.com",
			"account_settings": map[string]any{
				"transport_profile": map[string]any{
					"preset": "claude_chrome_like_mac_v3",
				},
			},
		},
	}
	authB := &cliproxyauth.Auth{
		Provider: "claude",
		ProxyURL: "http://shared-proxy:8080",
		Metadata: map[string]any{
			"auth_method": "oauth",
			"email":       "account-b@example.com",
			"account_settings": map[string]any{
				"transport_profile": map[string]any{
					"preset": "claude_chrome_like_mac_v3",
				},
			},
		},
	}

	clientA1 := NewProxyAwareHTTPClient(context.Background(), nil, authA, 0)
	clientA2 := NewProxyAwareHTTPClient(context.Background(), nil, authA, 0)
	clientB := NewProxyAwareHTTPClient(context.Background(), nil, authB, 0)

	if clientA1 == nil || clientA1.Transport == nil || clientB == nil || clientB.Transport == nil {
		t.Fatal("expected transport-profile clients to be created")
	}
	if clientA1.Transport != clientA2.Transport {
		t.Fatal("expected same account/profile to reuse cached transport")
	}
	if clientA1.Transport == clientB.Transport {
		t.Fatal("expected different account identities to use isolated transport cache entries")
	}
}

func TestRuntimeTransportProfileStatus_ProviderSpecificPresets(t *testing.T) {
	claudeAuth := &cliproxyauth.Auth{
		Provider: "claude",
		Metadata: map[string]any{
			"account_settings": map[string]any{
				"transport_profile": map[string]any{
					"preset": "provider-default",
				},
			},
		},
	}
	codexAuth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{
			"account_settings": map[string]any{
				"transport_profile": map[string]any{
					"preset": "provider-default",
				},
			},
		},
	}

	claudeProfile := ResolveRuntimeTransportProfile(claudeAuth)
	codexProfile := ResolveRuntimeTransportProfile(codexAuth)
	if claudeProfile == nil || codexProfile == nil {
		t.Fatal("expected provider-specific profiles to resolve")
	}
	if claudeProfile.Family != "cli-native" {
		t.Fatalf("claude family = %q, want cli-native", claudeProfile.Family)
	}
	if codexProfile.Family != "standard" {
		t.Fatalf("codex family = %q, want standard", codexProfile.Family)
	}
	enforced, claudeStatus := RuntimeTransportProfileStatus(claudeAuth)
	if !enforced {
		t.Fatalf("expected claude provider-default to be enforced as CLI-native account isolation: %s", claudeStatus)
	}
	if !strings.Contains(claudeStatus, "claude transport_profile") || strings.Contains(claudeStatus, "codex transport_profile") {
		t.Fatalf("claude status should be provider-specific, got %q", claudeStatus)
	}
	if !strings.Contains(claudeStatus, "CLI-native") {
		t.Fatalf("claude provider-default status should explain CLI-native isolation, got %q", claudeStatus)
	}
	enforced, codexStatus := RuntimeTransportProfileStatus(codexAuth)
	if !enforced {
		t.Fatalf("expected codex provider-default to be enforced: %s", codexStatus)
	}
	if !strings.Contains(codexStatus, "codex transport_profile") {
		t.Fatalf("codex status should be provider-specific, got %q", codexStatus)
	}
}

func TestRuntimeTransportProfileStatus_UnknownProfileFallsBack(t *testing.T) {
	auth := &cliproxyauth.Auth{
		Provider: "claude",
		Metadata: map[string]any{
			"account_settings": map[string]any{
				"transport_profile": map[string]any{
					"preset": "codex_managed_transport_v1",
				},
			},
		},
	}

	enforced, status := RuntimeTransportProfileStatus(auth)
	if enforced {
		t.Fatalf("expected unknown claude profile not to be enforced: %s", status)
	}
	if !strings.Contains(status, "unsupported") || !strings.Contains(status, "falling back") {
		t.Fatalf("expected explicit fallback status for unknown profile, got %q", status)
	}
	if key := RuntimeTransportProfileCacheKey("http://shared-proxy:8080", auth); key != "" {
		t.Fatalf("expected unsupported profile not to allocate runtime cache key, got %q", key)
	}
}

func TestNewProxyAwareHTTPClient_UsesProfileScopedTransportCache(t *testing.T) {
	authA := &cliproxyauth.Auth{
		ID:       "claude-a-cache",
		Provider: "claude",
		ProxyURL: "http://shared-proxy:8080",
		Metadata: map[string]any{
			"account_settings": map[string]any{
				"transport_profile": map[string]any{
					"preset": "claude_chrome_like_mac_v3",
				},
			},
		},
	}
	authB := authA.Clone()
	authB.ID = "claude-b-cache"

	clientA1 := NewProxyAwareHTTPClient(context.Background(), nil, authA, 0)
	clientA2 := NewProxyAwareHTTPClient(context.Background(), nil, authA, 0)
	clientB := NewProxyAwareHTTPClient(context.Background(), nil, authB, 0)

	if clientA1 == nil || clientA1.Transport == nil {
		t.Fatal("expected transport-profile client to be created")
	}
	if _, ok := clientA1.Transport.(*fallbackRoundTripper); !ok {
		t.Fatalf("transport type = %T, want *fallbackRoundTripper", clientA1.Transport)
	}
	if clientA1.Transport != clientA2.Transport {
		t.Fatal("expected same auth/profile to reuse cached transport")
	}
	if clientA1.Transport == clientB.Transport {
		t.Fatal("expected different auth IDs to use isolated transport cache entries")
	}
}

func TestNewProxyAwareHTTPClient_UsesCodexProfileScopedTransportCache(t *testing.T) {
	authA := &cliproxyauth.Auth{
		ID:       "codex-a-cache",
		Provider: "codex",
		ProxyURL: "http://shared-proxy:8080",
		Metadata: map[string]any{
			"account_settings": map[string]any{
				"transport_profile": map[string]any{
					"preset": "provider-default",
					"alpn":   []string{"h2"},
				},
			},
		},
	}
	authB := authA.Clone()
	authB.ID = "codex-b-cache"

	clientA1 := NewProxyAwareHTTPClient(context.Background(), nil, authA, 0)
	clientA2 := NewProxyAwareHTTPClient(context.Background(), nil, authA, 0)
	clientB := NewProxyAwareHTTPClient(context.Background(), nil, authB, 0)

	if clientA1 == nil || clientA1.Transport == nil {
		t.Fatal("expected codex transport-profile client to be created")
	}
	if clientA1.Transport != clientA2.Transport {
		t.Fatal("expected same codex auth/profile to reuse cached transport")
	}
	if clientA1.Transport == clientB.Transport {
		t.Fatal("expected different codex auth IDs to use isolated transport cache entries")
	}
}
