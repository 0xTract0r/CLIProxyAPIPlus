package management

import (
	"bufio"
	"context"
	"crypto/md5"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/codex"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// Handler-level egress regression tests for the OAuth token exchange entry
// points (fix-oauth-egress-anticorr). They drive the real gin handlers
// (RequestAnthropicToken / RequestCodexToken), complete the OAuth callback via
// the callback file, and observe the token exchange on a local fake HTTP
// CONNECT proxy that records the CONNECT target and the TLS ClientHello bytes.
// No test in this file ever reaches a real upstream: every exchange either
// goes through a local fake proxy, a local sentinel proxy that refuses CONNECT,
// or a fake codex OAuth service.

const (
	egressCodexRustlsJA3MD5      = "e4d448cdfe06dc1243c1eb026c74ac9a"
	egressClaudeCLIWithSNIJA3MD5 = "d871d02cecbde59abbf8f4806134addf"
	egressGoDefaultJA3MD5        = "03117a8ed39ef02427ebbc39f121275c"
	egressWaitTimeout            = 10 * time.Second
)

type egressCapture struct {
	target string
	hello  []byte
}

// egressConnectProxy is a minimal HTTP CONNECT proxy. With status 200 it
// acknowledges the tunnel, captures the client's first TLS handshake message
// (the ClientHello) and closes the connection, so no upstream is ever dialed.
// With any other status it refuses the CONNECT and only records the target.
type egressConnectProxy struct {
	ln      net.Listener
	status  int
	mu      sync.Mutex
	targets []string
	hellos  []egressCapture
}

func newEgressConnectProxy(t *testing.T, status int) *egressConnectProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen fake proxy: %v", err)
	}
	p := &egressConnectProxy{ln: ln, status: status}
	go p.serve()
	t.Cleanup(func() { _ = ln.Close() })
	return p
}

func (p *egressConnectProxy) URL() string { return "http://" + p.ln.Addr().String() }

func (p *egressConnectProxy) serve() {
	for {
		conn, err := p.ln.Accept()
		if err != nil {
			return
		}
		go p.handle(conn)
	}
}

func (p *egressConnectProxy) handle(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	br := bufio.NewReader(conn)
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	target := req.RequestURI
	if req.Method != http.MethodConnect {
		target = req.Method + " " + req.RequestURI
	}
	p.mu.Lock()
	p.targets = append(p.targets, target)
	p.mu.Unlock()
	if p.status != http.StatusOK || req.Method != http.MethodConnect {
		_, _ = io.WriteString(conn, "HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
		return
	}
	if _, err = io.WriteString(conn, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		return
	}
	hello, err := egressReadClientHello(br)
	if err != nil {
		return
	}
	p.mu.Lock()
	p.hellos = append(p.hellos, egressCapture{target: target, hello: hello})
	p.mu.Unlock()
}

func (p *egressConnectProxy) connects() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.targets...)
}

func (p *egressConnectProxy) waitHello(timeout time.Duration) (egressCapture, bool) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		if len(p.hellos) > 0 {
			c := p.hellos[0]
			p.mu.Unlock()
			return c, true
		}
		p.mu.Unlock()
		time.Sleep(25 * time.Millisecond)
	}
	return egressCapture{}, false
}

// egressReadClientHello reads TLS records until one complete handshake message
// is available and returns it (type(1) + length(3) + body).
func egressReadClientHello(r io.Reader) ([]byte, error) {
	var hs []byte
	for i := 0; i < 8; i++ {
		hdr := make([]byte, 5)
		if _, err := io.ReadFull(r, hdr); err != nil {
			return nil, err
		}
		if hdr[0] != 0x16 {
			return nil, fmt.Errorf("record type %d is not a handshake record", hdr[0])
		}
		body := make([]byte, int(binary.BigEndian.Uint16(hdr[3:5])))
		if _, err := io.ReadFull(r, body); err != nil {
			return nil, err
		}
		hs = append(hs, body...)
		if len(hs) >= 4 {
			want := 4 + (int(hs[1])<<16 | int(hs[2])<<8 | int(hs[3]))
			if len(hs) >= want {
				return hs[:want], nil
			}
		}
	}
	return nil, fmt.Errorf("client hello spans too many records")
}

func egressIsGREASE(v int) bool { return v&0x0f0f == 0x0a0a && v>>8 == v&0xff }

// egressJA3 computes the JA3 string and its md5 from a captured ClientHello
// handshake message (GREASE values removed, per the JA3 spec).
func egressJA3(t *testing.T, hs []byte) (string, string) {
	t.Helper()
	if len(hs) < 4 || hs[0] != 0x01 {
		t.Fatalf("captured bytes are not a ClientHello handshake message (len=%d)", len(hs))
	}
	p := hs[4:]
	off := 0
	need := func(n int) {
		if n < 0 || off+n > len(p) {
			t.Fatalf("truncated ClientHello at offset %d (+%d of %d)", off, n, len(p))
		}
	}
	u8 := func() int { need(1); v := int(p[off]); off++; return v }
	u16 := func() int { need(2); v := int(binary.BigEndian.Uint16(p[off:])); off += 2; return v }
	skip := func(n int) { need(n); off += n }

	version := u16()
	skip(32)
	skip(u8())
	csLen := u16()
	var ciphers []string
	for i := 0; i < csLen; i += 2 {
		if c := u16(); !egressIsGREASE(c) {
			ciphers = append(ciphers, strconv.Itoa(c))
		}
	}
	skip(u8())
	var exts, curves, points []string
	if off+2 <= len(p) {
		extTotal := u16()
		end := off + extTotal
		for off < end {
			et := u16()
			el := u16()
			need(el)
			ev := p[off : off+el]
			off += el
			if egressIsGREASE(et) {
				continue
			}
			exts = append(exts, strconv.Itoa(et))
			switch et {
			case 0x000a:
				n := int(binary.BigEndian.Uint16(ev))
				for i := 0; i < n; i += 2 {
					if g := int(binary.BigEndian.Uint16(ev[2+i:])); !egressIsGREASE(g) {
						curves = append(curves, strconv.Itoa(g))
					}
				}
			case 0x000b:
				n := int(ev[0])
				for i := 0; i < n; i++ {
					points = append(points, strconv.Itoa(int(ev[1+i])))
				}
			}
		}
	}
	ja3 := fmt.Sprintf("%d,%s,%s,%s,%s", version,
		strings.Join(ciphers, "-"), strings.Join(exts, "-"),
		strings.Join(curves, "-"), strings.Join(points, "-"))
	sum := md5.Sum([]byte(ja3))
	return ja3, hex.EncodeToString(sum[:])
}

// egressRecordingCodexService replaces the global (core-proxy) codex OAuth
// service. It never touches the network; it only counts exchanges so a test
// can prove the exchange did NOT go through the global service.
type egressRecordingCodexService struct {
	exchanges atomic.Int32
}

func (s *egressRecordingCodexService) GenerateAuthURL(state string, _ *codex.PKCECodes) (string, error) {
	return "https://auth.example.test/oauth?state=" + state, nil
}

func (s *egressRecordingCodexService) ExchangeCodeForTokens(context.Context, string, *codex.PKCECodes) (*codex.CodexAuthBundle, error) {
	s.exchanges.Add(1)
	return nil, fmt.Errorf("global codex OAuth service must not be used for this exchange")
}

func (s *egressRecordingCodexService) CreateTokenStorage(*codex.CodexAuthBundle) *codex.CodexTokenStorage {
	return &codex.CodexTokenStorage{}
}

func useEgressRecordingCodexService(t *testing.T) *egressRecordingCodexService {
	t.Helper()
	rec := &egressRecordingCodexService{}
	orig := newCodexOAuthService
	newCodexOAuthService = func(*config.Config) codexOAuthService { return rec }
	t.Cleanup(func() { newCodexOAuthService = orig })
	return rec
}

func newEgressTestHandler(t *testing.T, globalProxy string) (*Handler, *coreauth.Manager, string) {
	t.Helper()
	authDir := filepath.Join(t.TempDir(), "auths")
	cfg := &config.Config{AuthDir: authDir}
	cfg.SDKConfig.ProxyURL = globalProxy
	manager := coreauth.NewManager(nil, nil, nil)
	h := NewHandlerWithoutConfigFilePath(cfg, manager)
	h.tokenStore = &memoryAuthStore{}
	return h, manager, authDir
}

func registerEgressTarget(t *testing.T, manager *coreauth.Manager, provider, id, proxyURL string) {
	t.Helper()
	metadata := map[string]any{
		"type":  provider,
		"email": "target-" + provider + "@example.test",
		"note":  "keep me",
	}
	if provider == "codex" {
		metadata["account_id"] = "acct-target"
	}
	if proxyURL != "" {
		metadata["proxy_url"] = proxyURL
	}
	auth := &coreauth.Auth{
		ID:       id,
		FileName: id,
		Provider: provider,
		ProxyURL: proxyURL,
		Status:   coreauth.StatusActive,
		Metadata: metadata,
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register target auth %s: %v", id, err)
	}
}

// egressCallAuthURL performs a real GET against the OAuth start handler and
// returns (status, state, body). Any session it opens is cancelled on cleanup.
func egressCallAuthURL(t *testing.T, h *Handler, path string, query url.Values) (int, string, string) {
	t.Helper()
	router := gin.New()
	router.GET("/anthropic-auth-url", h.RequestAnthropicToken)
	router.GET("/codex-auth-url", h.RequestCodexToken)
	target := path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, target, nil))
	var payload map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &payload)
	state, _ := payload["state"].(string)
	if state != "" {
		t.Cleanup(func() { CancelOAuthSession(state) })
	}
	return w.Code, state, w.Body.String()
}

func egressDescribeJA3(md5sum string) string {
	switch md5sum {
	case egressGoDefaultJA3MD5:
		return "Go default TLS (03117a8e leak)"
	case egressCodexRustlsJA3MD5:
		return "codex-rs"
	case egressClaudeCLIWithSNIJA3MD5:
		return "claude-cli"
	default:
		return "unknown"
	}
}

// TestRequestAnthropicTokenReauthUsesTargetAccountProxy: a card re-auth
// (auth_name=<file>) must exchange the code through the TARGET account's
// proxy_url with the claude-cli ClientHello, never through the core global
// proxy. The global proxy is a local sentinel that refuses CONNECT, so a
// regression is observable without any real egress.
func TestRequestAnthropicTokenReauthUsesTargetAccountProxy(t *testing.T) {
	targetProxy := newEgressConnectProxy(t, http.StatusOK)
	globalSentinel := newEgressConnectProxy(t, http.StatusForbidden)
	h, manager, authDir := newEgressTestHandler(t, globalSentinel.URL())
	const targetName = "claude-target@example.test.json"
	registerEgressTarget(t, manager, "claude", targetName, targetProxy.URL())

	code, state, body := egressCallAuthURL(t, h, "/anthropic-auth-url", url.Values{"auth_name": {targetName}})
	if code != http.StatusOK || state == "" {
		t.Fatalf("anthropic-auth-url?auth_name=%s: status=%d body=%s, want 200 with state", targetName, code, body)
	}
	if _, err := WriteOAuthCallbackFileForPendingSession(authDir, "anthropic", state, "test-code", ""); err != nil {
		t.Fatalf("write callback file: %v", err)
	}

	capture, ok := targetProxy.waitHello(egressWaitTimeout)
	if !ok {
		t.Fatalf("token exchange never reached the target account proxy within %s; global sentinel CONNECTs=%v (the exchange used the core global proxy instead of auth_name's account proxy)", egressWaitTimeout, globalSentinel.connects())
	}
	if capture.target != "api.anthropic.com:443" {
		t.Fatalf("target proxy CONNECT = %q, want api.anthropic.com:443", capture.target)
	}
	if got := globalSentinel.connects(); len(got) != 0 {
		t.Fatalf("global proxy must not be used for a card re-auth exchange, got CONNECTs=%v", got)
	}
	ja3, sum := egressJA3(t, capture.hello)
	if sum != egressClaudeCLIWithSNIJA3MD5 {
		t.Fatalf("claude re-auth exchange JA3 md5=%s (%s), want claude-cli %s\nJA3=%s", sum, egressDescribeJA3(sum), egressClaudeCLIWithSNIJA3MD5, ja3)
	}
}

// TestRequestCodexTokenReauthUsesTargetAccountProxy: a codex card re-auth must
// exchange through the target account's proxy_url with the codex-rs
// ClientHello. The global proxy is empty and the global codex service is a
// recording fake, so falling back to it is observable without network I/O.
func TestRequestCodexTokenReauthUsesTargetAccountProxy(t *testing.T) {
	globalService := useEgressRecordingCodexService(t)
	targetProxy := newEgressConnectProxy(t, http.StatusOK)
	h, manager, authDir := newEgressTestHandler(t, "")
	const targetName = "codex-target@example.test-plus.json"
	registerEgressTarget(t, manager, "codex", targetName, targetProxy.URL())

	code, state, body := egressCallAuthURL(t, h, "/codex-auth-url", url.Values{"auth_name": {targetName}})
	if code != http.StatusOK || state == "" {
		t.Fatalf("codex-auth-url?auth_name=%s: status=%d body=%s, want 200 with state", targetName, code, body)
	}
	if _, err := WriteOAuthCallbackFileForPendingSession(authDir, "codex", state, "test-code", ""); err != nil {
		t.Fatalf("write callback file: %v", err)
	}

	capture, ok := targetProxy.waitHello(egressWaitTimeout)
	if !ok {
		t.Fatalf("codex token exchange never reached the target account proxy within %s; global codex service exchanges=%d (auth_name ignored, exchange fell back to the global service)", egressWaitTimeout, globalService.exchanges.Load())
	}
	if capture.target != "auth.openai.com:443" {
		t.Fatalf("target proxy CONNECT = %q, want auth.openai.com:443", capture.target)
	}
	if n := globalService.exchanges.Load(); n != 0 {
		t.Fatalf("global codex OAuth service exchanged %d time(s); card re-auth must use the target account", n)
	}
	ja3, sum := egressJA3(t, capture.hello)
	if sum != egressCodexRustlsJA3MD5 {
		t.Fatalf("codex re-auth exchange JA3 md5=%s (%s), want codex-rs %s\nJA3=%s", sum, egressDescribeJA3(sum), egressCodexRustlsJA3MD5, ja3)
	}
}

// TestRequestOAuthReauthRejectsTargetWithoutProxy: a card re-auth whose target
// is missing, belongs to another provider, or has an empty / invalid proxy_url
// must be rejected with 400 before any session or outbound connection exists,
// and must not fall back to the core global proxy (a sentinel here).
func TestRequestOAuthReauthRejectsTargetWithoutProxy(t *testing.T) {
	providers := []struct {
		name  string
		path  string
		other string
	}{
		{name: "claude", path: "/anthropic-auth-url", other: "codex"},
		{name: "codex", path: "/codex-auth-url", other: "claude"},
	}
	cases := []struct {
		name     string
		register func(t *testing.T, m *coreauth.Manager, provider, other, id string)
	}{
		{name: "empty proxy", register: func(t *testing.T, m *coreauth.Manager, provider, _, id string) {
			registerEgressTarget(t, m, provider, id, "")
		}},
		{name: "invalid proxy", register: func(t *testing.T, m *coreauth.Manager, provider, _, id string) {
			registerEgressTarget(t, m, provider, id, "ftp://bad-proxy.example:21")
		}},
		{name: "missing target", register: func(*testing.T, *coreauth.Manager, string, string, string) {}},
		{name: "other provider", register: func(t *testing.T, m *coreauth.Manager, _, other, id string) {
			registerEgressTarget(t, m, other, id, "http://127.0.0.1:9")
		}},
	}
	for _, pv := range providers {
		for _, tc := range cases {
			t.Run(pv.name+"/"+tc.name, func(t *testing.T) {
				globalService := useEgressRecordingCodexService(t)
				globalSentinel := newEgressConnectProxy(t, http.StatusForbidden)
				h, manager, _ := newEgressTestHandler(t, globalSentinel.URL())
				id := pv.name + "-reject-target.json"
				tc.register(t, manager, pv.name, pv.other, id)

				code, state, body := egressCallAuthURL(t, h, pv.path, url.Values{"auth_name": {id}})
				if code != http.StatusBadRequest {
					t.Fatalf("%s?auth_name=%s (%s): status=%d body=%s, want 400 (fail-closed, no fallback to the global proxy)", pv.path, id, tc.name, code, body)
				}
				if state != "" {
					t.Fatalf("rejected request must not open an OAuth session, got state %q", state)
				}
				if got := globalSentinel.connects(); len(got) != 0 {
					t.Fatalf("rejected request dialed the global proxy: %v", got)
				}
				if n := globalService.exchanges.Load(); n != 0 {
					t.Fatalf("rejected request reached the global codex service %d time(s)", n)
				}
			})
		}
	}
}

// TestRequestOAuthWithoutSetupRejectsWhenGlobalProxyEmpty: an OAuth start with
// neither an account setup proxy nor auth_name has no proxy to exchange
// through when the global proxy-url is empty, so it must be rejected (400)
// instead of exchanging over a direct connection. A setup that carries only a
// note (no proxy_url) is rejected as well. With a global proxy configured the
// legacy no-parameter flow keeps working.
func TestRequestOAuthWithoutSetupRejectsWhenGlobalProxyEmpty(t *testing.T) {
	providers := []struct{ name, path string }{
		{name: "claude", path: "/anthropic-auth-url"},
		{name: "codex", path: "/codex-auth-url"},
	}
	for _, pv := range providers {
		t.Run(pv.name+"/no params, global empty", func(t *testing.T) {
			useEgressRecordingCodexService(t)
			h, _, _ := newEgressTestHandler(t, "")
			code, state, body := egressCallAuthURL(t, h, pv.path, nil)
			if code != http.StatusBadRequest || state != "" {
				t.Fatalf("%s with no setup/auth_name and empty global proxy: status=%d body=%s, want 400 without a session", pv.path, code, body)
			}
		})
		t.Run(pv.name+"/note only", func(t *testing.T) {
			useEgressRecordingCodexService(t)
			globalSentinel := newEgressConnectProxy(t, http.StatusForbidden)
			h, _, _ := newEgressTestHandler(t, globalSentinel.URL())
			code, state, body := egressCallAuthURL(t, h, pv.path, url.Values{"note": {"new account"}})
			if code != http.StatusBadRequest || state != "" {
				t.Fatalf("%s?note=... without proxy_url: status=%d body=%s, want 400 without a session", pv.path, code, body)
			}
		})
		t.Run(pv.name+"/no params, global set", func(t *testing.T) {
			useEgressRecordingCodexService(t)
			globalSentinel := newEgressConnectProxy(t, http.StatusForbidden)
			h, _, _ := newEgressTestHandler(t, globalSentinel.URL())
			code, state, body := egressCallAuthURL(t, h, pv.path, nil)
			if code != http.StatusOK || state == "" {
				t.Fatalf("%s with a configured global proxy: status=%d body=%s, want 200 with state", pv.path, code, body)
			}
		})
	}
}

// TestRequestCodexTokenExchangeUsesCodexRustlsClientHello: the OAuth login
// page flow (proxy_url supplied) must exchange the codex code with the
// codex-rs ClientHello over the supplied proxy, not the Go default TLS
// fingerprint (JA3 03117a8e). The ClientHello is captured on the wire at the
// fake proxy and hashed, so this is a real JA3 check of the bytes sent.
func TestRequestCodexTokenExchangeUsesCodexRustlsClientHello(t *testing.T) {
	globalService := useEgressRecordingCodexService(t)
	accountProxy := newEgressConnectProxy(t, http.StatusOK)
	h, _, authDir := newEgressTestHandler(t, "")

	code, state, body := egressCallAuthURL(t, h, "/codex-auth-url", url.Values{
		"proxy_url": {accountProxy.URL()},
		"note":      {"new codex account"},
	})
	if code != http.StatusOK || state == "" {
		t.Fatalf("codex-auth-url?proxy_url=...: status=%d body=%s, want 200 with state", code, body)
	}
	if _, err := WriteOAuthCallbackFileForPendingSession(authDir, "codex", state, "test-code", ""); err != nil {
		t.Fatalf("write callback file: %v", err)
	}

	capture, ok := accountProxy.waitHello(egressWaitTimeout)
	if !ok {
		t.Fatalf("codex token exchange never reached the account proxy within %s (global service exchanges=%d)", egressWaitTimeout, globalService.exchanges.Load())
	}
	if capture.target != "auth.openai.com:443" {
		t.Fatalf("account proxy CONNECT = %q, want auth.openai.com:443", capture.target)
	}
	ja3, sum := egressJA3(t, capture.hello)
	if sum != egressCodexRustlsJA3MD5 {
		t.Fatalf("codex OAuth exchange JA3 md5=%s (%s), want codex-rs %s\nJA3=%s", sum, egressDescribeJA3(sum), egressCodexRustlsJA3MD5, ja3)
	}
}
