package cliproxy

import (
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestDefaultRoundTripperProvider_IsolatesByAuthIdentity(t *testing.T) {
	provider := newDefaultRoundTripperProvider()

	authA := &coreauth.Auth{
		ID:       "codex-a",
		Provider: "codex",
		ProxyURL: "http://shared-proxy:8080",
	}
	authB := &coreauth.Auth{
		ID:       "codex-b",
		Provider: "codex",
		ProxyURL: "http://shared-proxy:8080",
	}

	rtA1 := provider.RoundTripperFor(authA)
	rtA2 := provider.RoundTripperFor(authA)
	rtB := provider.RoundTripperFor(authB)

	if rtA1 == nil || rtA2 == nil || rtB == nil {
		t.Fatal("expected transports to be created")
	}
	if rtA1 != rtA2 {
		t.Fatal("expected same auth identity to reuse cached transport")
	}
	if rtA1 == rtB {
		t.Fatal("expected different auth IDs to get isolated transports")
	}
}

func TestDefaultRoundTripperProvider_IsolatesByTransportProfile(t *testing.T) {
	provider := newDefaultRoundTripperProvider()

	authV1 := &coreauth.Auth{
		ID:       "codex-profile",
		Provider: "codex",
		ProxyURL: "http://shared-proxy:8080",
		Metadata: map[string]any{
			"account_settings": map[string]any{
				"transport_profile": map[string]any{
					"preset": "provider-default",
				},
			},
		},
	}
	authV2 := authV1.Clone()
	authV2.Metadata = map[string]any{
		"account_settings": map[string]any{
			"transport_profile": map[string]any{
				"preset": "codex_isolated_transport_v1",
			},
		},
	}

	rtV1 := provider.RoundTripperFor(authV1)
	rtV2 := provider.RoundTripperFor(authV2)

	if rtV1 == nil || rtV2 == nil {
		t.Fatal("expected transports to be created")
	}
	if rtV1 == rtV2 {
		t.Fatal("expected different transport_profile tokens to split cached transports")
	}
}
