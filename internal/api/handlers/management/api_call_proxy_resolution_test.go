package management

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func TestAPICallProxyCandidatePriority(t *testing.T) {
	const providerProxy = "http://127.0.0.1:12002"
	const globalProxy = "http://127.0.0.1:12003"
	for _, tc := range []struct{ name, request, account, provider, global, want string }{
		{"invalid account uses provider", "", "bad-account", providerProxy, globalProxy, providerProxy},
		{"invalid provider uses global", "", "", "bad-provider", globalProxy, globalProxy},
		{"invalid account and provider use global", "", "bad-account", "bad-provider", globalProxy, globalProxy},
		{"invalid account skips provider direct", "", "bad-account", "direct", globalProxy, globalProxy},
		{"valid account remains first", "", "http://127.0.0.1:12001", providerProxy, globalProxy, "http://127.0.0.1:12001"},
		{"direct account remains first", "", "direct", "bad-provider", globalProxy, "direct"},
		{"valid request remains first", "http://127.0.0.1:12000", "bad-account", providerProxy, globalProxy, "http://127.0.0.1:12000"},
		{"invalid request never falls through", "bad-request", "http://127.0.0.1:12001", providerProxy, globalProxy, "bad-request"},
		{"direct request remains first", "direct", "bad-account", providerProxy, globalProxy, "direct"},
		{"all empty inherits direct", "", "", "", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &Handler{cfg: &config.Config{SDKConfig: sdkconfig.SDKConfig{ProxyURL: tc.global}, GeminiKey: []config.GeminiKey{{APIKey: "synthetic", ProxyURL: tc.provider}}}}
			a := &coreauth.Auth{Provider: "gemini", ProxyURL: tc.account, Attributes: map[string]string{"api_key": "synthetic"}}
			if got := h.apiCallResolvedProxyURL(a, tc.request); got != tc.want {
				t.Fatalf("proxy=%q want=%q", got, tc.want)
			}
		})
	}
}

type apiCallCountedListener struct {
	net.Listener
	accepted atomic.Int32
}

func (l *apiCallCountedListener) Accept() (net.Conn, error) {
	c, e := l.Listener.Accept()
	if e == nil {
		l.accepted.Add(1)
	}
	return c, e
}

func TestAPICallInvalidCandidateCannotFallBackToDirect(t *testing.T) {
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	counted := &apiCallCountedListener{Listener: upstream.Listener}
	upstream.Listener = counted
	upstream.StartTLS()
	defer upstream.Close()
	var envCalls atomic.Int32
	envProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { envCalls.Add(1); w.WriteHeader(502) }))
	defer envProxy.Close()
	t.Setenv("HTTP_PROXY", envProxy.URL)
	t.Setenv("HTTPS_PROXY", envProxy.URL)
	t.Setenv("ALL_PROXY", envProxy.URL)
	t.Setenv("NO_PROXY", "")
	for _, badSource := range []string{"account", "provider", "request"} {
		for _, global := range []string{"", "bad-global", "direct", "none"} {
			t.Run(fmt.Sprintf("%s/global=%s", badSource, global), func(t *testing.T) {
				cfg := &config.Config{SDKConfig: sdkconfig.SDKConfig{ProxyURL: global}}
				a := &coreauth.Auth{Provider: "gemini", Attributes: map[string]string{"api_key": "synthetic"}}
				request := ""
				switch badSource {
				case "account":
					a.ProxyURL = "bad-account"
				case "provider":
					cfg.GeminiKey = []config.GeminiKey{{APIKey: "synthetic", ProxyURL: "bad-provider"}}
				case "request":
					request = "bad-request"
					a.ProxyURL = envProxy.URL
				}
				h := &Handler{cfg: cfg}
				tr, _ := h.apiCallTransportWithDialer(a, request)
				if tr.Proxy != nil || tr.DialContext == nil || tr.DialTLSContext == nil {
					t.Fatal("invalid chain did not install both blocking dial functions")
				}
				for _, dial := range []func(context.Context, string, string) (net.Conn, error){tr.DialContext, tr.DialTLSContext} {
					conn, err := dial(context.Background(), "tcp", counted.Addr().String())
					if conn != nil {
						conn.Close()
					}
					if err == nil || !strings.Contains(err.Error(), "api-call proxy configuration is invalid") {
						t.Fatalf("unexpected blocked dial result: %v", err)
					}
				}
				for _, provider := range []string{"claude", "codex"} {
					clone := a.Clone()
					clone.Provider = provider
					if badSource == "provider" {
						cfg.ClaudeKey = []config.ClaudeKey{{APIKey: "synthetic", ProxyURL: "bad-provider"}}
						cfg.CodexKey = []config.CodexKey{{APIKey: "synthetic", ProxyURL: "bad-provider"}}
					}
					ctx, cancel := context.WithTimeout(context.Background(), time.Second)
					req, _ := http.NewRequestWithContext(ctx, http.MethodGet, upstream.URL, nil)
					resp, err := h.apiCallTransport(clone, request).RoundTrip(req)
					if resp != nil && resp.Body != nil {
						resp.Body.Close()
					}
					expired := ctx.Err()
					cancel()
					if err == nil || expired != nil {
						t.Fatalf("%s did not immediately block: err=%v deadline=%v", provider, err, expired)
					}
				}
				if counted.accepted.Load() != 0 || envCalls.Load() != 0 {
					t.Fatal("invalid chain attempted direct or environment-proxy I/O")
				}
			})
		}
	}
}

func TestAPICallInvalidAccountUsesGlobalProxyForEveryTransport(t *testing.T) {
	for _, provider := range []string{"gemini", "claude", "codex"} {
		t.Run(provider, func(t *testing.T) {
			seen := make(chan string, 1)
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { seen <- r.Method + " " + r.Host; w.WriteHeader(502) }))
			defer proxy.Close()
			h := &Handler{cfg: &config.Config{SDKConfig: sdkconfig.SDKConfig{ProxyURL: proxy.URL}}}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://127.0.0.1:1/controlled", nil)
			rt := h.apiCallTransport(&coreauth.Auth{Provider: provider, ProxyURL: "bad-account"})
			resp, err := rt.RoundTrip(req)
			if resp != nil && resp.Body != nil {
				resp.Body.Close()
			}
			if closer, ok := rt.(interface{ CloseIdleConnections() }); ok {
				defer closer.CloseIdleConnections()
			}
			if err == nil {
				t.Fatal("controlled proxy denial should return error")
			}
			select {
			case got := <-seen:
				if got != "CONNECT 127.0.0.1:1" {
					t.Fatalf("proxy request=%q", got)
				}
			default:
				t.Fatalf("%s did not use the selected loopback proxy: %v", provider, err)
			}
		})
	}
}
