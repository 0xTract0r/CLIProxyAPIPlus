package helps

import (
	"context"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"testing"
	"time"
)

func TestClaude420RuntimeHandshakeBoundDoesNotMutateCacheOrOtherProviders(t *testing.T) {
	auth := &cliproxyauth.Auth{ID: "runtime-handshake-420", Provider: "claude", ProxyURL: "direct"}
	cfg := &config.Config{}
	cfg.TLSHandshakeTimeoutSeconds = 1
	one := NewProxyAwareHTTPClient(context.Background(), cfg, auth, 0)
	bound, ok := one.Transport.(*tlsHandshakeTimeoutRoundTripper)
	if !ok || bound.timeout != time.Second {
		t.Fatalf("configured transport=%T", one.Transport)
	}
	cfg.TLSHandshakeTimeoutSeconds = 0
	zero := NewProxyAwareHTTPClient(context.Background(), cfg, auth, 0)
	if _, ok := zero.Transport.(*tlsHandshakeTimeoutRoundTripper); ok {
		t.Fatal("configured timeout contaminated cached transport")
	}
	if zero.Transport != bound.base {
		t.Fatal("base transport was replaced")
	}
	cfg.TLSHandshakeTimeoutSeconds = 2
	two := NewProxyAwareHTTPClient(context.Background(), cfg, auth, 0)
	next, ok := two.Transport.(*tlsHandshakeTimeoutRoundTripper)
	if !ok || next.timeout != 2*time.Second || bound.timeout != time.Second || next.base != bound.base {
		t.Fatal("hot reload or client isolation failed")
	}
	for _, provider := range []string{"", "kimi", "codex"} {
		other := &cliproxyauth.Auth{ID: "other-420", Provider: provider, ProxyURL: "direct"}
		client := NewProxyAwareHTTPClient(context.Background(), cfg, other, 0)
		if _, ok := client.Transport.(*tlsHandshakeTimeoutRoundTripper); ok {
			t.Fatalf("Claude timeout wrapper leaked to provider %q", provider)
		}
	}
}
