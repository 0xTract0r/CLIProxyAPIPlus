package helps

import (
	"context"
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

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
