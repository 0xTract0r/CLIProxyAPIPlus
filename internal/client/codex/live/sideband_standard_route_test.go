package live

import (
	"context"
	"crypto/tls"
	"errors"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"testing"
)

func TestOrdinarySidebandUsesSelectedRouteAndStandardTLS(t *testing.T) {
	for _, tc := range []struct {
		name, cred, global string
		disabled, denied   bool
	}{
		{"missing", "", "", false, true}, {"invalid", "invalid://example.invalid", "direct", false, true},
		{"disabled", "direct", "direct", true, true}, {"explicit-direct", "direct", "invalid://local", false, false},
		{"service-direct", "", "direct", false, false}, {"approved-socks5", "socks5://proxy.invalid:1080", "", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			cfg := &config.Config{}
			cfg.ProxyURL = tc.global
			d := newProxyAwareSidebandDialer(ctx, cfg, &auth.Auth{ProxyURL: tc.cred, Disabled: tc.disabled})
			if d.Proxy != nil || d.NetDialTLSContext != nil || d.TLSClientConfig == nil || d.TLSClientConfig.InsecureSkipVerify || d.TLSClientConfig.MinVersion < tls.VersionTLS12 {
				t.Fatal("unexpected TLS or proxy setup")
			}
			_, err := d.NetDialContext(ctx, "tcp", "127.0.0.1:0")
			if err == nil {
				t.Fatal("unexpected successful dial")
			}
			if tc.denied != (err.Error() == "sideband requires an authorized upstream route") {
				t.Fatalf("unexpected route result %v", err)
			}
			if !tc.denied && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation lost: %v", err)
			}
		})
	}
}
