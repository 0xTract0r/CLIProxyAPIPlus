package executor

import (
	"net"
	"net/http"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

// TestApplyCodexRustlsWebsocketTLSForcesUTLSAndNilProxy verifies the codex-only
// wiring: after applying the codex-rs uTLS TLS dialer, the gorilla Dialer must have
// a non-nil NetDialTLSContext and, critically, Proxy forced to nil. If Proxy stayed
// set, gorilla would wrap the uTLS dialer as the base dialer used to REACH a proxy
// (double-proxy), so the nil-Proxy invariant is load-bearing.
func TestApplyCodexRustlsWebsocketTLSForcesUTLSAndNilProxy(t *testing.T) {
	t.Parallel()

	for _, proxyURL := range []string{
		"",
		"socks5h://user:pass@proxy.example.com:1080",
		"http://proxy.example.com:8080",
	} {
		dialer := &websocket.Dialer{
			Proxy:          http.ProxyFromEnvironment,
			NetDialContext: (&net.Dialer{}).DialContext,
		}
		applyCodexRustlsWebsocketTLS(dialer, proxyURL)
		if dialer.Proxy != nil {
			t.Fatalf("[proxy=%q] dialer.Proxy must be nil so the uTLS closure owns the proxy tunnel (no double-proxy)", proxyURL)
		}
		if dialer.NetDialTLSContext == nil {
			t.Fatalf("[proxy=%q] dialer.NetDialTLSContext must be set to the codex-rs uTLS dialer", proxyURL)
		}
	}
}

// TestSharedWebsocketDialerDoesNotApplyUTLS guards the isolation boundary: the
// shared newProxyAwareWebsocketDialer is used by BOTH the codex and xAI executors,
// so it must NOT hardcode the codex-rs uTLS ClientHello. codex applies it
// separately (dialCodexWebsocket -> applyCodexRustlsWebsocketTLS); if the shared
// dialer set NetDialTLSContext, xAI traffic would leak the codex-rs JA3.
func TestSharedWebsocketDialerDoesNotApplyUTLS(t *testing.T) {
	t.Parallel()

	dialer := newProxyAwareWebsocketDialer(
		&config.Config{SDKConfig: sdkconfig.SDKConfig{ProxyURL: "socks5h://global.example.com:1080"}},
		&cliproxyauth.Auth{ProxyURL: "socks5h://account.example.com:1080"},
	)
	if dialer.NetDialTLSContext != nil {
		t.Fatal("shared websocket dialer must not set NetDialTLSContext; codex-rs uTLS must not leak onto xAI traffic")
	}
}
