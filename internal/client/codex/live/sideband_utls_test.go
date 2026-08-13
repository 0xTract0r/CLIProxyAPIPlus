package live

import (
	"net/http"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

// TestNewProxyAwareSidebandDialerAppliesCodexRustlsUTLS verifies the sideband relay
// dials its wss upstream (api.openai.com, codex identity) with the codex-rs uTLS
// ClientHello: NetDialTLSContext must be set and Proxy forced to nil. If Proxy
// stayed set, gorilla would wrap the uTLS dialer as the base dialer used to REACH a
// proxy (double-proxy), so the nil-Proxy invariant is load-bearing.
func TestNewProxyAwareSidebandDialerAppliesCodexRustlsUTLS(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		cfg  *config.Config
		auth *auth.Auth
	}{
		{
			name: "no-proxy",
			cfg:  &config.Config{},
			auth: &auth.Auth{},
		},
		{
			name: "account-socks-proxy",
			cfg:  &config.Config{SDKConfig: sdkconfig.SDKConfig{ProxyURL: "socks5h://global.example.com:1080"}},
			auth: &auth.Auth{ProxyURL: "socks5h://account.example.com:1080"},
		},
		{
			name: "http-proxy",
			cfg:  &config.Config{SDKConfig: sdkconfig.SDKConfig{ProxyURL: "http://proxy.example.com:8080"}},
			auth: &auth.Auth{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dialer := newProxyAwareSidebandDialer(tc.cfg, tc.auth)
			if dialer.NetDialTLSContext == nil {
				t.Fatal("sideband dialer must set NetDialTLSContext to the codex-rs uTLS dialer")
			}
			if dialer.Proxy != nil {
				t.Fatal("sideband dialer.Proxy must be nil so the uTLS closure owns the proxy tunnel (no double-proxy)")
			}
		})
	}
}

// TestApplyCodexRustlsSidebandTLSNilDialerNoPanic guards the defensive nil check.
func TestApplyCodexRustlsSidebandTLSNilDialerNoPanic(t *testing.T) {
	t.Parallel()

	applyCodexRustlsSidebandTLS(nil, "socks5h://proxy.example.com:1080")

	dialer := &websocket.Dialer{Proxy: http.ProxyFromEnvironment}
	applyCodexRustlsSidebandTLS(dialer, "")
	if dialer.Proxy != nil {
		t.Fatal("dialer.Proxy must be nil after applying codex-rs uTLS")
	}
	if dialer.NetDialTLSContext == nil {
		t.Fatal("dialer.NetDialTLSContext must be set after applying codex-rs uTLS")
	}
}
