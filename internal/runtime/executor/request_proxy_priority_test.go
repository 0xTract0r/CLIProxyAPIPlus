package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func TestRequestProxyOverridesCredentialProxyForWebsocketAndAntigravity(t *testing.T) {
	t.Parallel()

	seen := make(chan string, 1)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Method + " " + r.Host
		w.WriteHeader(http.StatusForbidden)
	}))
	defer proxy.Close()
	requestProxy := proxy.URL
	ctx := cliproxyexecutor.WithRequestProxyURL(context.Background(), requestProxy)
	cfg := &config.Config{SDKConfig: sdkconfig.SDKConfig{ProxyURL: "http://global-proxy.example:8082"}}
	auth := &cliproxyauth.Auth{ProxyURL: "http://auth-proxy.example:8080"}

	client := helps.NewProxyAwareHTTPClient(ctx, cfg, auth, 0)
	transport, ok := client.Transport.(*http.Transport)
	if !ok || transport.Proxy == nil {
		t.Fatalf("standard transport has no configured proxy: %T", client.Transport)
	}
	dialer := newProxyAwareWebsocketDialer(ctx, cfg, auth)
	req, errReq := http.NewRequest(http.MethodGet, "https://upstream.example/v1", nil)
	if errReq != nil {
		t.Fatalf("request: %v", errReq)
	}
	httpProxy, err := transport.Proxy(req)
	if err != nil || httpProxy == nil || httpProxy.String() != requestProxy {
		t.Fatalf("HTTP proxy = %v, err=%v", httpProxy, err)
	}
	conn, err := dialer.NetDialContext(ctx, "tcp", "upstream.example.invalid:443")
	if conn != nil {
		conn.Close()
	}
	if err == nil {
		t.Fatal("expected upstream tunnel refusal from fixture")
	}
	select {
	case got := <-seen:
		if got != "CONNECT upstream.example.invalid:443" {
			t.Errorf("unexpected proxy request %q", got)
		}
	default:
		t.Fatal("WebSocket did not use the explicit request proxy")
	}
}
