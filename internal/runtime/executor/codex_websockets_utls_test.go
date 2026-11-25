package executor

import (
	"context"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// A credential-specific proxy must still use the shared standard TLS transport.
func TestSharedWebsocketDialerDoesNotApplyUTLS(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{SDKConfig: config.SDKConfig{ProxyURL: "socks5h://global.fixture.invalid:1080"}}
	auth := &cliproxyauth.Auth{ProxyURL: "socks5h://credential.fixture.invalid:1080"}
	dialer := newProxyAwareWebsocketDialer(context.Background(), cfg, auth)
	if dialer.NetDialTLSContext != nil {
		t.Fatal("shared websocket dialer must use standard TLS")
	}
}
