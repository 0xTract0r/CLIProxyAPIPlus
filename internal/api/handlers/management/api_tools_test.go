package management

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func TestAPICallTransportDirectBypassesGlobalProxy(t *testing.T) {
	t.Parallel()

	h := &Handler{
		cfg: &config.Config{
			SDKConfig: sdkconfig.SDKConfig{ProxyURL: "http://global-proxy.example.com:8080"},
		},
	}

	transport := h.apiCallTransport(&coreauth.Auth{ProxyURL: "direct"})
	httpTransport, ok := transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", transport)
	}
	if httpTransport.Proxy != nil {
		t.Fatal("expected direct transport to disable proxy function")
	}
}

func TestAPICallTransportInvalidAuthFallsBackToGlobalProxy(t *testing.T) {
	t.Parallel()

	h := &Handler{
		cfg: &config.Config{
			SDKConfig: sdkconfig.SDKConfig{ProxyURL: "http://global-proxy.example.com:8080"},
		},
	}

	transport := h.apiCallTransport(&coreauth.Auth{ProxyURL: "bad-value"})
	httpTransport, ok := transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", transport)
	}

	req, errRequest := http.NewRequest(http.MethodGet, "https://example.com", nil)
	if errRequest != nil {
		t.Fatalf("http.NewRequest returned error: %v", errRequest)
	}

	proxyURL, errProxy := httpTransport.Proxy(req)
	if errProxy != nil {
		t.Fatalf("httpTransport.Proxy returned error: %v", errProxy)
	}
	if proxyURL == nil || proxyURL.String() != "http://global-proxy.example.com:8080" {
		t.Fatalf("proxy URL = %v, want http://global-proxy.example.com:8080", proxyURL)
	}
}

func TestAPICallTransportAPIKeyAuthFallsBackToConfigProxyURL(t *testing.T) {
	t.Parallel()

	h := &Handler{
		cfg: &config.Config{
			SDKConfig: sdkconfig.SDKConfig{ProxyURL: "http://global-proxy.example.com:8080"},
			GeminiKey: []config.GeminiKey{{
				APIKey:   "gemini-key",
				ProxyURL: "http://gemini-proxy.example.com:8080",
			}},
			ClaudeKey: []config.ClaudeKey{{
				APIKey:   "claude-key",
				ProxyURL: "http://claude-proxy.example.com:8080",
			}},
			CodexKey: []config.CodexKey{{
				APIKey:   "codex-key",
				ProxyURL: "http://codex-proxy.example.com:8080",
			}},
			XAIKey: []config.XAIKey{{
				APIKey:   "xai-key",
				ProxyURL: "http://xai-proxy.example.com:8080",
			}},
			OpenAICompatibility: []config.OpenAICompatibility{{
				Name:    "bohe",
				BaseURL: "https://bohe.example.com",
				APIKeyEntries: []config.OpenAICompatibilityAPIKey{{
					APIKey:   "compat-key",
					ProxyURL: "http://compat-proxy.example.com:8080",
				}},
			}},
		},
	}

	cases := []struct {
		name      string
		auth      *coreauth.Auth
		wantProxy string
	}{
		{
			name: "gemini",
			auth: &coreauth.Auth{
				Provider:   "gemini",
				Attributes: map[string]string{"api_key": "gemini-key"},
			},
			wantProxy: "http://gemini-proxy.example.com:8080",
		},
		// claude / codex removed from this std-lib proxy-resolution table: after the
		// anti-correlation fix their api-call transport is a uTLS *fallbackRoundTripper,
		// not *http.Transport, so httpTransport.Proxy(req) can no longer assert their
		// proxy here. Their proxy chain is covered by TestAPICallResolvedProxyURL_*
		// and their uTLS routing by api_call_utls_transport_test.go.
		{
			name: "xai",
			auth: &coreauth.Auth{
				Provider:   "xai",
				Attributes: map[string]string{"api_key": "xai-key"},
			},
			wantProxy: "http://xai-proxy.example.com:8080",
		},
		{
			name: "openai-compatibility",
			auth: &coreauth.Auth{
				Provider: "bohe",
				Attributes: map[string]string{
					"api_key":      "compat-key",
					"compat_name":  "bohe",
					"provider_key": "bohe",
				},
			},
			wantProxy: "http://compat-proxy.example.com:8080",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			transport := h.apiCallTransport(tc.auth)
			httpTransport, ok := transport.(*http.Transport)
			if !ok {
				t.Fatalf("transport type = %T, want *http.Transport", transport)
			}

			req, errRequest := http.NewRequest(http.MethodGet, "https://example.com", nil)
			if errRequest != nil {
				t.Fatalf("http.NewRequest returned error: %v", errRequest)
			}

			proxyURL, errProxy := httpTransport.Proxy(req)
			if errProxy != nil {
				t.Fatalf("httpTransport.Proxy returned error: %v", errProxy)
			}
			if proxyURL == nil || proxyURL.String() != tc.wantProxy {
				t.Fatalf("proxy URL = %v, want %s", proxyURL, tc.wantProxy)
			}
		})
	}
}

func TestAuthByIndexDistinguishesSharedAPIKeysAcrossProviders(t *testing.T) {
	t.Parallel()

	manager := coreauth.NewManager(nil, nil, nil)
	geminiAuth := &coreauth.Auth{
		ID:       "gemini:apikey:123",
		Provider: "gemini",
		Attributes: map[string]string{
			"api_key": "shared-key",
		},
	}
	compatAuth := &coreauth.Auth{
		ID:       "openai-compatibility:bohe:456",
		Provider: "bohe",
		Label:    "bohe",
		Attributes: map[string]string{
			"api_key":      "shared-key",
			"compat_name":  "bohe",
			"provider_key": "bohe",
		},
	}

	if _, errRegister := manager.Register(context.Background(), geminiAuth); errRegister != nil {
		t.Fatalf("register gemini auth: %v", errRegister)
	}
	if _, errRegister := manager.Register(context.Background(), compatAuth); errRegister != nil {
		t.Fatalf("register compat auth: %v", errRegister)
	}

	geminiIndex := geminiAuth.EnsureIndex()
	compatIndex := compatAuth.EnsureIndex()
	if geminiIndex == compatIndex {
		t.Fatalf("shared api key produced duplicate auth_index %q", geminiIndex)
	}

	h := &Handler{authManager: manager}

	gotGemini := h.authByIndex(geminiIndex)
	if gotGemini == nil {
		t.Fatal("expected gemini auth by index")
	}
	if gotGemini.ID != geminiAuth.ID {
		t.Fatalf("authByIndex(gemini) returned %q, want %q", gotGemini.ID, geminiAuth.ID)
	}

	gotCompat := h.authByIndex(compatIndex)
	if gotCompat == nil {
		t.Fatal("expected compat auth by index")
	}
	if gotCompat.ID != compatAuth.ID {
		t.Fatalf("authByIndex(compat) returned %q, want %q", gotCompat.ID, compatAuth.ID)
	}
}

// ---------------------------------------------------------------------------
// P3 — hardenAPICallTransport actually installs a 10s net.Dialer.
//
// We deliberately reach for the *net.Dialer returned by
// apiCallTransportWithDialer instead of probing http.Transport reflectively,
// because that surface is the only stable way to assert that the dial phase
// is bounded by apiCallDialTimeout. The first attempt at this fix shipped
// with a "if transport.DialContext == nil" guard that silently skipped the
// dialer installation on cloned http.DefaultTransport; the assertion below
// is the regression check for that bug.
// ---------------------------------------------------------------------------

func TestHardenAPICallTransportDirectInstalls10sDialer(t *testing.T) {
	t.Parallel()

	h := &Handler{
		cfg: &config.Config{
			SDKConfig: sdkconfig.SDKConfig{ProxyURL: ""},
		},
	}

	transport, dialer := h.apiCallTransportWithDialer(nil)
	if transport == nil {
		t.Fatal("transport must not be nil for direct path")
	}
	if dialer == nil {
		t.Fatal("dialer must not be nil for direct path")
	}
	if dialer.Timeout != apiCallDialTimeout {
		t.Fatalf("dialer.Timeout = %v, want %v", dialer.Timeout, apiCallDialTimeout)
	}
	if dialer.KeepAlive != apiCallKeepAliveProbeInterval {
		t.Fatalf("dialer.KeepAlive = %v, want %v", dialer.KeepAlive, apiCallKeepAliveProbeInterval)
	}
	if transport.TLSHandshakeTimeout != apiCallTLSHandshakeTimeout {
		t.Fatalf("TLSHandshakeTimeout = %v, want %v", transport.TLSHandshakeTimeout, apiCallTLSHandshakeTimeout)
	}
	if transport.ResponseHeaderTimeout != apiCallResponseHeaderTimeout {
		t.Fatalf("ResponseHeaderTimeout = %v, want %v", transport.ResponseHeaderTimeout, apiCallResponseHeaderTimeout)
	}
	if transport.ExpectContinueTimeout != apiCallExpectContinueTimeout {
		t.Fatalf("ExpectContinueTimeout = %v, want %v", transport.ExpectContinueTimeout, apiCallExpectContinueTimeout)
	}
	if transport.IdleConnTimeout != apiCallIdleConnectionTimeout {
		t.Fatalf("IdleConnTimeout = %v, want %v", transport.IdleConnTimeout, apiCallIdleConnectionTimeout)
	}
}

func TestHardenAPICallTransportHTTPProxyInstalls10sDialer(t *testing.T) {
	t.Parallel()

	h := &Handler{
		cfg: &config.Config{
			SDKConfig: sdkconfig.SDKConfig{ProxyURL: "http://global-proxy.example.com:8080"},
		},
	}

	transport, dialer := h.apiCallTransportWithDialer(nil)
	if transport == nil {
		t.Fatal("transport must not be nil for http-proxy path")
	}
	if dialer == nil {
		t.Fatal("dialer must not be nil for http-proxy path")
	}
	if dialer.Timeout != apiCallDialTimeout {
		t.Fatalf("dialer.Timeout = %v, want %v", dialer.Timeout, apiCallDialTimeout)
	}
	if transport.Proxy == nil {
		t.Fatal("http-proxy transport must expose Proxy function")
	}
}

func TestHardenAPICallTransportSOCKS5InstallsTimeoutWrapper(t *testing.T) {
	t.Parallel()

	h := &Handler{
		cfg: &config.Config{
			SDKConfig: sdkconfig.SDKConfig{ProxyURL: "socks5://127.0.0.1:1"},
		},
	}

	transport, dialer := h.apiCallTransportWithDialer(nil)
	if transport == nil {
		t.Fatal("transport must not be nil for socks5 path")
	}
	if dialer == nil {
		t.Fatal("dialer must not be nil for socks5 path")
	}
	if dialer.Timeout != apiCallDialTimeout {
		t.Fatalf("dialer.Timeout = %v, want %v", dialer.Timeout, apiCallDialTimeout)
	}
	if transport.DialContext == nil {
		t.Fatal("socks5 transport must expose DialContext")
	}

	// The SOCKS5 wrapper must immediately surface ctx.Err() when the caller
	// supplies an already-cancelled context. This is the indirect proof that
	// the wrapper still delegates to the proxyutil inner closure (which
	// honours ctx.Done()) and does not block on a fresh dial.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	conn, dialErr := transport.DialContext(ctx, "tcp", "api.anthropic.com:443")
	if conn != nil {
		_ = conn.Close()
		t.Fatal("DialContext returned a connection despite cancelled ctx")
	}
	if !errors.Is(dialErr, context.Canceled) {
		t.Fatalf("DialContext error = %v, want context.Canceled", dialErr)
	}
}

// TestHardenAPICallTransportDialTimeoutTriggers proves that the 10s dial
// timeout is the bound that fires when the dialer hands control over to a
// stalled connect. We can't use a real network address (flaky and slow), so
// we wire a hardenedDialer.wrapped closure that sleeps longer than the
// timeout and assert the wrapper still returns inside the deadline.
func TestHardenAPICallTransportDialTimeoutTriggers(t *testing.T) {
	t.Parallel()

	stallCh := make(chan struct{})
	defer close(stallCh)

	hd := &hardenedDialer{
		dialer: &net.Dialer{
			Timeout:   100 * time.Millisecond,
			KeepAlive: apiCallKeepAliveProbeInterval,
		},
		wrapped: func(ctx context.Context, network, addr string) (net.Conn, error) {
			// Simulate a SOCKS5 inner dial that hangs until ctx fires.
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}

	start := time.Now()
	conn, dialErr := hd.dialContext(context.Background(), "tcp", "stub:443")
	elapsed := time.Since(start)
	if conn != nil {
		_ = conn.Close()
		t.Fatal("dialContext returned a connection from a stalled wrapper")
	}
	if dialErr == nil {
		t.Fatal("dialContext returned nil error after stall")
	}
	if !errors.Is(dialErr, context.DeadlineExceeded) {
		t.Fatalf("dialContext error = %v, want context.DeadlineExceeded", dialErr)
	}
	if elapsed > 1500*time.Millisecond {
		t.Fatalf("dial took %v, expected to fire near 100ms timeout", elapsed)
	}
}

// ---------------------------------------------------------------------------
// P4 — classifyAPICallError DNS error must beat the generic timeout branch.
// ---------------------------------------------------------------------------

func TestClassifyAPICallError(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "nil",
			err:  nil,
			want: "",
		},
		{
			name: "deadline_exceeded",
			err:  context.DeadlineExceeded,
			want: "context_deadline_exceeded",
		},
		{
			name: "canceled",
			err:  context.Canceled,
			want: "context_canceled",
		},
		{
			name: "dns_timeout_must_not_become_network_timeout",
			err:  &net.DNSError{Err: "i/o timeout", Name: "example.com", IsTimeout: true, IsTemporary: true},
			want: "dns_error",
		},
		{
			name: "dns_no_such_host",
			err:  &net.DNSError{Err: "no such host", Name: "example.com", IsNotFound: true},
			want: "dns_error",
		},
		{
			name: "dns_wrapped_in_op_error",
			err: &net.OpError{
				Op:  "dial",
				Net: "tcp",
				Err: &net.DNSError{Err: "i/o timeout", Name: "example.com", IsTimeout: true},
			},
			want: "dns_error",
		},
		{
			name: "plain_op_error",
			err:  &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("boom")},
			want: "network_op_error:dial",
		},
		{
			name: "fallback",
			err:  errors.New("nothing matches"),
			want: "transport_error",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := classifyAPICallError(tc.err)
			if got != tc.want {
				t.Fatalf("classifyAPICallError(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

// TestClassifyAPICallErrorTimeoutOnlyWhenNotDNS guards against future
// reorderings that would resurrect the dns-error-as-network_timeout bug.
func TestClassifyAPICallErrorTimeoutOnlyWhenNotDNS(t *testing.T) {
	t.Parallel()

	// A pure net.Error timeout (no DNS payload underneath) should still
	// return "network_timeout".
	timeoutErr := &timeoutOnlyError{}
	if got := classifyAPICallError(timeoutErr); got != "network_timeout" {
		t.Fatalf("classifyAPICallError(timeoutOnlyError) = %q, want network_timeout", got)
	}
}

type timeoutOnlyError struct{}

func (timeoutOnlyError) Error() string   { return "i/o timeout" }
func (timeoutOnlyError) Timeout() bool   { return true }
func (timeoutOnlyError) Temporary() bool { return true }

// ---------------------------------------------------------------------------
// P5 — APICall error responses include failure_kind.
// ---------------------------------------------------------------------------

func TestAPICallErrorResponseIncludesFailureKind(t *testing.T) {
	t.Parallel()

	// Spin up a server that closes the connection immediately so the
	// downstream client.Do returns a transport error and we exercise the
	// failure_kind branch.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "no hijacker", http.StatusInternalServerError)
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			return
		}
		_ = conn.Close()
	}))
	defer upstream.Close()

	h := &Handler{
		cfg: &config.Config{
			SDKConfig: sdkconfig.SDKConfig{ProxyURL: ""},
		},
	}

	router := gin.New()
	router.POST("/v0/management/api-call", h.APICall)

	bodyPayload, errMarshal := json.Marshal(map[string]any{
		"method": http.MethodGet,
		"url":    upstream.URL + "/healthz",
	})
	if errMarshal != nil {
		t.Fatalf("marshal payload: %v", errMarshal)
	}

	req := httptest.NewRequest(http.MethodPost, "/v0/management/api-call", strings.NewReader(string(bodyPayload)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d (body=%s)", rec.Code, http.StatusBadGateway, rec.Body.String())
	}

	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v (body=%s)", err, rec.Body.String())
	}
	if _, ok := payload["failure_kind"]; !ok {
		t.Fatalf("response missing failure_kind: %s", rec.Body.String())
	}
	if payload["error"] != "request failed" {
		t.Fatalf("response error = %v, want \"request failed\"", payload["error"])
	}
}

// TestAPICallErrorResponseDNSFailureKindLabel exercises the full handler
// path with a guaranteed-DNS-failure hostname and asserts the response is
// labelled "dns_error" rather than "network_timeout". This is the
// integration-level guard for the P4 ordering fix.
func TestAPICallErrorResponseDNSFailureKindLabel(t *testing.T) {
	t.Parallel()

	h := &Handler{
		cfg: &config.Config{
			SDKConfig: sdkconfig.SDKConfig{ProxyURL: ""},
		},
	}

	router := gin.New()
	router.POST("/v0/management/api-call", h.APICall)

	// .invalid is RFC 2606-reserved and never resolves, so the dial phase
	// surfaces a *net.DNSError. We pair it with a short URL to keep the
	// test fast even on hosts with slow resolvers.
	target := fmt.Sprintf("http://nonexistent-host-%d.invalid/healthz", time.Now().UnixNano())
	bodyPayload, errMarshal := json.Marshal(map[string]any{
		"method": http.MethodGet,
		"url":    target,
	})
	if errMarshal != nil {
		t.Fatalf("marshal payload: %v", errMarshal)
	}

	req := httptest.NewRequest(http.MethodPost, "/v0/management/api-call", strings.NewReader(string(bodyPayload)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d (body=%s)", rec.Code, http.StatusBadGateway, rec.Body.String())
	}

	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v (body=%s)", err, rec.Body.String())
	}
	kind, ok := payload["failure_kind"].(string)
	if !ok {
		t.Fatalf("response missing failure_kind string: %s", rec.Body.String())
	}
	// The strict regression guard is: a DNS failure must NEVER be labelled
	// "network_timeout" (that was the original bug). On most stdlib
	// resolvers we expect "dns_error", but cgo / platform resolvers can
	// surface the failure as a generic transport_error or context_* label.
	// We accept any non-network_timeout label here; the precise mapping is
	// covered by the dns_wrapped_in_op_error and
	// dns_timeout_must_not_become_network_timeout unit cases above.
	if kind == "network_timeout" {
		t.Fatalf("DNS failure was mislabelled as network_timeout: %s", rec.Body.String())
	}
	allowed := map[string]struct{}{
		"dns_error":                 {},
		"context_deadline_exceeded": {},
		"context_canceled":          {},
		"transport_error":           {},
	}
	if _, ok := allowed[kind]; !ok && !strings.HasPrefix(kind, "network_op_error") {
		t.Fatalf("unexpected failure_kind = %q (body=%s)", kind, rec.Body.String())
	}
}
