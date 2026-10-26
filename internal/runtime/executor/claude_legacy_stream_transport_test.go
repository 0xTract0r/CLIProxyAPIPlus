package executor

import (
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"net/http"
	"testing"
)

func TestClaudeLegacyStreamTransportKeepsExplicitNativePolicies(t *testing.T) {
	for _, tc := range []struct {
		name                                                        string
		stream, oauth, native, helper, pin, profile, stable, custom bool
		accept, encoding                                            string
	}{
		{name: "legacy_key", stream: true, accept: "text/event-stream", encoding: "identity"},
		{name: "legacy_oauth", stream: true, oauth: true, accept: "text/event-stream", encoding: "identity"},
		{name: "legacy_stabilized", stream: true, stable: true, accept: "text/event-stream", encoding: "identity"},
		{name: "nonstream_unchanged", accept: "application/json", encoding: "gzip, deflate, br, zstd"},
		{name: "native_unchanged", stream: true, native: true, accept: "application/json", encoding: "gzip, deflate, br, zstd"},
		{name: "native_helper_unchanged", stream: true, native: true, helper: true, accept: "application/json", encoding: "gzip, deflate, br, zstd"},
		{name: "pinned_unchanged", stream: true, pin: true, accept: "application/json", encoding: "gzip, deflate, br, zstd"},
		{name: "explicit_profile_unchanged", stream: true, profile: true, accept: "application/json", encoding: "gzip, deflate, br, zstd"},
		{name: "custom_pinned_unchanged", stream: true, pin: true, custom: true, accept: "text/event-stream", encoding: "identity"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetClaudeDeviceProfileCache()
			cfg := &config.Config{ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{PinSoftwareBaseline: tc.pin, StabilizeDeviceProfile: &tc.stable}}
			key := "synthetic-key"
			if tc.oauth {
				key = "sk-ant-oat-synthetic"
			}
			auth := &cliproxyauth.Auth{ID: tc.name, Provider: "claude", ProxyURL: "direct", Attributes: map[string]string{"api_key": key}}
			if tc.profile {
				auth.Attributes["fingerprint_profile"] = "claude-code-cli"
			}
			req := newClaudeHeaderTestRequest(t, http.Header{})
			if tc.custom {
				req.URL.Host = "127.0.0.1:1"
			}
			if err := applyClaudeHeadersWithNativeProfile(req, auth, key, tc.stream, nil, nil, cfg, nil, tc.native, tc.helper); err != nil {
				t.Fatal(err)
			}
			if got := req.Header.Get("Accept"); got != tc.accept {
				t.Fatalf("Accept=%q want %q", got, tc.accept)
			}
			if got := req.Header.Get("Accept-Encoding"); got != tc.encoding {
				t.Fatalf("Accept-Encoding=%q want %q", got, tc.encoding)
			}
		})
	}
}
