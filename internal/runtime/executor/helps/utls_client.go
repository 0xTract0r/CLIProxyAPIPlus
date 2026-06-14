package helps

import (
	"context"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	tls "github.com/refraction-networking/utls"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/proxyutil"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/proxy"
)

// claudeCLIClientHelloProfileID is the project profile that replicates the
// real claude-cli (Node/OpenSSL) ClientHello. Resolving it yields a uTLS
// HelloCustom ID; the matching spec is built by newClaudeCLIClientHelloSpec.
// This profile is wired into the claude->anthropic core-managed default
// outbound path (see coreManagedRuntimeTransportProfile in
// transport_profile.go); it is NOT the default for NewUtlsHTTPClient, whose
// only production caller is the codex executor (chatgpt.com).
const claudeCLIClientHelloProfileID = "claude_cli_clienthello_v1"

// utlsHTTPClientDefaultProfileID is the default ClientHello profile for
// NewUtlsHTTPClient. Its only production caller is the codex executor
// (host chatgpt.com), so the default must stay the Chrome-like preset used
// before the claude-cli ClientHello work; routing codex outbound through the
// claude-cli HelloCustom fingerprint would misrepresent the codex client.
const utlsHTTPClientDefaultProfileID = "claude_utls_chrome_133"

// claudeCLIALPN is the only ALPN protocol real claude-cli advertises.
// claude-cli negotiates http/1.1 and never offers h2, so the outbound
// connection must speak HTTP/1.1 (no HTTP/2, no h2 in ALPN).
var claudeCLIALPN = []string{"http/1.1"}

// utlsDialAttemptTimeout bounds a single TCP dial (including the socks5 proxy
// CONNECT) for the uTLS handshake path. The root cause of the claude
// HelloCustom failures is an occasional slow/unresponsive rotating-residential
// socks5 proxy: golang.org/x/net/proxy.SOCKS5 dials through proxy.Direct (a
// net.Dialer with no timeout), so a stuck proxy connect can hang on OS TCP
// defaults (~30s+). A short per-attempt bound makes that transient failure fail
// fast so the configured ClientHello can be retried, instead of the request
// stalling. This is a credential-acquisition-time connection bound (allowed by
// the executor timeout policy); it is not applied after a connection is
// established. It is a var (not a const) only so tests can shorten it; it is
// never mutated in production code.
var utlsDialAttemptTimeout = 10 * time.Second

// utlsHandshakeMaxAttempts is how many times the configured ClientHello
// (HelloCustom for claude, the Chrome-like preset for codex) handshake is
// attempted before giving up. Because the dominant failure mode is a transient
// proxy dial timeout, retrying the SAME ClientHello spec recovers most of these
// without ever changing the outbound fingerprint. The first attempt plus up to
// (utlsHandshakeMaxAttempts-1) retries.
const utlsHandshakeMaxAttempts = 3

// utlsHandshakeRetryBackoff is the base delay between configured-ClientHello
// handshake attempts. Backoff is linear (attempt index * base) and small, since
// the goal is to ride over a brief proxy hiccup, not to wait out a long outage.
const utlsHandshakeRetryBackoff = 200 * time.Millisecond

// utlsRoundTripper implements http.RoundTripper using utls to replicate a
// target client TLS fingerprint on protected API hosts. The default profile
// replicates real claude-cli (HelloCustom + ALPN http/1.1); other profiles
// keep the prior Chrome-like presets. Transport is HTTP/1.1 only.
type utlsRoundTripper struct {
	dialer      proxy.Dialer
	clientHello tls.ClientHelloID
	insecure    bool
	transport   *http.Transport

	// configuredHello is the ClientHello identifier this round tripper is
	// expected to use (e.g. ClientHelloID.Str()). It is set at construction
	// time and never mutated, so it is safe to read concurrently.
	configuredHello string
	// fallbackCount counts how many times the configured HelloCustom handshake
	// failed and the round tripper silently downgraded to the Chrome-like
	// fallback. Read/written via sync/atomic only. With disableFallback=true
	// (the default for the claude HelloCustom profile) this stays 0: the strict
	// profile never downgrades.
	fallbackCount int64
	// retryCount counts how many extra configured-ClientHello handshake attempts
	// were made beyond the first (i.e. transient-failure retries that reused the
	// same ClientHello spec, NOT downgrades). Read/written via sync/atomic only.
	retryCount int64
	// hardFailCount counts how many times the configured ClientHello exhausted
	// all retries and returned an error WITHOUT downgrading (strict mode). For
	// the claude HelloCustom profile this is the "request failed, fingerprint
	// preserved" counter. Read/written via sync/atomic only.
	hardFailCount int64
	// lastHandshakeHello stores (string) the ClientHello identifier actually
	// used by the most recent successful handshake. Read/written via
	// atomic.Value only.
	lastHandshakeHello atomic.Value
	// disableFallback, when true, makes the configured handshake hard-fail
	// instead of silently downgrading to the Chrome-like fallback. Default
	// false preserves the connectivity-first fallback behavior; this is a
	// diagnostic / strict-mode opt-in only.
	disableFallback bool
}

func newUtlsRoundTripper(proxyURL string, clientHello tls.ClientHelloID) *utlsRoundTripper {
	var dialer proxy.Dialer = proxy.Direct
	if proxyURL != "" {
		proxyDialer, mode, errBuild := proxyutil.BuildDialer(proxyURL)
		if errBuild != nil {
			log.Errorf("utls: failed to configure proxy dialer for %q: %v", proxyutil.Redact(proxyURL), errBuild)
		} else if mode != proxyutil.ModeInherit && proxyDialer != nil {
			dialer = proxyDialer
		}
	}
	rt := &utlsRoundTripper{
		dialer:          dialer,
		clientHello:     clientHello,
		configuredHello: clientHello.Str(),
	}
	rt.transport = rt.newHTTP11Transport()
	return rt
}

func newDiagnosticUtlsRoundTripper(dialer proxy.Dialer, clientHello tls.ClientHelloID) *utlsRoundTripper {
	if dialer == nil {
		dialer = proxy.Direct
	}
	rt := &utlsRoundTripper{
		dialer:          dialer,
		clientHello:     clientHello,
		configuredHello: clientHello.Str(),
		insecure:        true,
	}
	rt.transport = rt.newHTTP11Transport()
	return rt
}

// newStrictUtlsRoundTripper builds a utls round tripper that does NOT downgrade
// to the Chrome-like fallback: if the configured HelloCustom handshake fails,
// dialTLSContext retries the same HelloCustom and, once attempts are exhausted,
// returns the original error instead of serving a Chrome fingerprint. This is the
// production path for claude strong-fingerprint profiles (anti-correlation: fail
// rather than leak a downgraded fingerprint). Codex / Chrome-native profiles keep
// the connectivity-first fallback via newUtlsRoundTripper.
func newStrictUtlsRoundTripper(proxyURL string, clientHello tls.ClientHelloID) *utlsRoundTripper {
	rt := newUtlsRoundTripper(proxyURL, clientHello)
	rt.disableFallback = true
	return rt
}

// newHTTP11Transport builds an HTTP/1.1-only transport that performs the uTLS
// handshake via DialTLSContext. ForceAttemptHTTP2 is disabled and the uTLS
// connection only advertises http/1.1, so the upgrade to HTTP/2 never happens.
func (t *utlsRoundTripper) newHTTP11Transport() *http.Transport {
	return &http.Transport{
		ForceAttemptHTTP2:   false,
		DialTLSContext:      t.dialTLSContext,
		MaxIdleConns:        100,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 0,
	}
}

// dialTLSContext dials the upstream and performs the uTLS handshake. When the
// configured ClientHello is HelloCustom, it applies the claude-cli ClientHello
// spec. The configured ClientHello is retried up to utlsHandshakeMaxAttempts
// times (same spec, same fingerprint) to ride over transient proxy dial
// timeouts, which are the dominant failure mode. After the retries are
// exhausted:
//   - disableFallback=true (claude HelloCustom strict profile): return the real
//     handshake error so the request fails. The configured fingerprint is NEVER
//     downgraded to Chrome; downgrading would leak that this is not real
//     claude-cli and defeat the anti-correlation guarantee.
//   - disableFallback=false (e.g. the codex Chrome-like profile): fall back to
//     the Chrome-like ClientHello so connectivity is preserved.
func (t *utlsRoundTripper) dialTLSContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}

	conn, err := t.handshakeWithRetry(ctx, host, addr)
	if err == nil {
		// A configured-ClientHello attempt succeeded (possibly after retries).
		t.lastHandshakeHello.Store(t.clientHello.Str())
		return conn, nil
	}

	fallbackHello := tls.HelloChrome_133
	// If the configured ClientHello already IS the Chrome-like preset (codex
	// default), there is nothing to downgrade to: return the error directly.
	if t.clientHello.Str() == fallbackHello.Str() {
		return nil, err
	}
	if t.disableFallback {
		// Strict mode (claude HelloCustom): surface the failure instead of
		// downgrading. The configured fingerprint is preserved or nothing.
		// fallbackCount stays 0; record this as a hard failure so the runtime
		// state reflects "claude enforced, did not downgrade".
		atomic.AddInt64(&t.hardFailCount, 1)
		log.Warnf("utls: claude HelloCustom handshake failed for %s after %d attempt(s); request failing without downgrade to preserve fingerprint (%v)", host, utlsHandshakeMaxAttempts, err)
		return nil, err
	}
	// Connectivity-first mode (non-claude): make the silent downgrade
	// observable: count it, record the actual handshake fingerprint, and warn
	// (host only, no credentials/proxy auth).
	atomic.AddInt64(&t.fallbackCount, 1)
	log.Warnf("utls: downgraded HelloCustom->HelloChrome_133 for %s: custom ClientHello handshake failed (%v)", host, err)
	conn, err = t.handshake(ctx, host, addr, fallbackHello)
	if err == nil {
		t.lastHandshakeHello.Store(fallbackHello.Str())
	}
	return conn, err
}

// handshakeWithRetry attempts the configured ClientHello handshake up to
// utlsHandshakeMaxAttempts times, reusing the SAME ClientHello spec each time so
// the outbound fingerprint never changes. It retries because the dominant
// failure is a transient proxy dial timeout, which usually succeeds on a second
// attempt. Each extra attempt beyond the first increments retryCount. The ctx
// deadline (if any) is always honored: a cancelled ctx stops the retry loop
// immediately.
func (t *utlsRoundTripper) handshakeWithRetry(ctx context.Context, host, addr string) (net.Conn, error) {
	var lastErr error
	for attempt := 0; attempt < utlsHandshakeMaxAttempts; attempt++ {
		if attempt > 0 {
			atomic.AddInt64(&t.retryCount, 1)
			// Short linear backoff, but never wait past the ctx deadline.
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt) * utlsHandshakeRetryBackoff):
			}
		}
		conn, err := t.handshake(ctx, host, addr, t.clientHello)
		if err == nil {
			return conn, nil
		}
		lastErr = err
		// If the context is done, stop retrying: further attempts cannot help.
		if ctx.Err() != nil {
			return nil, lastErr
		}
	}
	return nil, lastErr
}

// handshake performs a single uTLS handshake with the given ClientHelloID.
// The resulting ClientHello always advertises ALPN http/1.1 only, so the
// stdlib transport speaks HTTP/1.1 (no HTTP/2 negotiation) for both the
// replicated claude-cli profile and the Chrome-like fallback.
func (t *utlsRoundTripper) handshake(ctx context.Context, host, addr string, clientHelloID tls.ClientHelloID) (net.Conn, error) {
	spec, err := t.clientHelloSpec(clientHelloID)
	if err != nil {
		return nil, err
	}

	rawConn, err := t.dialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}

	tlsConfig := &tls.Config{ServerName: host, InsecureSkipVerify: t.insecure}
	tlsConn := tls.UClient(rawConn, tlsConfig, tls.HelloCustom)
	if errApply := tlsConn.ApplyPreset(spec); errApply != nil {
		_ = rawConn.Close()
		return nil, errApply
	}

	if errHS := tlsConn.HandshakeContext(ctx); errHS != nil {
		_ = rawConn.Close()
		return nil, errHS
	}
	return tlsConn, nil
}

// clientHelloSpec returns the ClientHelloSpec for the given ID. The replicated
// claude-cli profile (HelloCustom) uses newClaudeCLIClientHelloSpec; any other
// preset is materialized via UTLSIdToSpec and then forced to advertise ALPN
// http/1.1 only, so the fallback path also speaks HTTP/1.1 cleanly.
func (t *utlsRoundTripper) clientHelloSpec(clientHelloID tls.ClientHelloID) (*tls.ClientHelloSpec, error) {
	if clientHelloID.Str() == tls.HelloCustom.Str() {
		return newClaudeCLIClientHelloSpec()
	}
	spec, err := tls.UTLSIdToSpec(clientHelloID)
	if err != nil {
		return nil, err
	}
	forceALPNHTTP11(&spec)
	return &spec, nil
}

// forceALPNHTTP11 rewrites any ALPN extension in the spec to advertise only
// http/1.1, ensuring the negotiated protocol is HTTP/1.1.
func forceALPNHTTP11(spec *tls.ClientHelloSpec) {
	for _, ext := range spec.Extensions {
		if alpn, ok := ext.(*tls.ALPNExtension); ok {
			alpn.AlpnProtocols = append([]string(nil), claudeCLIALPN...)
		}
	}
}

// dialContext dials using the configured proxy dialer, honoring ctx when the
// dialer supports it. Each dial is bounded by utlsDialAttemptTimeout so a stuck
// socks5 proxy connect fails fast and the configured ClientHello can be retried,
// instead of hanging on OS TCP defaults (~30s+). The derived deadline never
// extends an already-shorter ctx deadline.
func (t *utlsRoundTripper) dialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	dialCtx, cancel := context.WithTimeout(ctx, utlsDialAttemptTimeout)
	defer cancel()
	ctx = dialCtx
	if ctxDialer, ok := t.dialer.(proxy.ContextDialer); ok {
		return ctxDialer.DialContext(ctx, network, addr)
	}
	type result struct {
		conn net.Conn
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		conn, err := t.dialer.Dial(network, addr)
		ch <- result{conn: conn, err: err}
	}()
	select {
	case <-ctx.Done():
		go func() {
			if r := <-ch; r.conn != nil {
				_ = r.conn.Close()
			}
		}()
		return nil, ctx.Err()
	case r := <-ch:
		return r.conn, r.err
	}
}

func (t *utlsRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return t.transport.RoundTrip(req)
}

// RuntimeHelloState reports the runtime (actually-used) ClientHello state for
// this round tripper, so callers can detect a silent HelloCustom->Chrome
// downgrade. It satisfies RuntimeHelloObserver. The returned Downgraded flag is
// true when any fallback has occurred or the last successful handshake used a
// ClientHello other than the configured one.
func (t *utlsRoundTripper) RuntimeHelloState() RuntimeHelloState {
	last, _ := t.lastHandshakeHello.Load().(string)
	count := atomic.LoadInt64(&t.fallbackCount)
	downgraded := count > 0 || (last != "" && last != t.configuredHello)
	return RuntimeHelloState{
		ConfiguredHello:    t.configuredHello,
		LastHandshakeHello: last,
		FallbackCount:      count,
		RetryCount:         atomic.LoadInt64(&t.retryCount),
		HardFailCount:      atomic.LoadInt64(&t.hardFailCount),
		Downgraded:         downgraded,
	}
}

// RuntimeHelloState forwards to the inner utls round tripper so the runtime
// hello state is observable through the fallback wrapper used by the HTTP
// clients. It returns false-equivalent zero state when the inner transport is
// not a utls observer.
func (f *fallbackRoundTripper) RuntimeHelloState() RuntimeHelloState {
	if observer, ok := f.utls.(RuntimeHelloObserver); ok {
		return observer.RuntimeHelloState()
	}
	return RuntimeHelloState{}
}

// newClaudeCLIClientHelloSpec builds the uTLS ClientHelloSpec that replicates
// the real claude-cli (Node/OpenSSL) ClientHello, targeting
// JA3 e97f5146a7009cc2918b50e903b6ff8d. Cipher suites and the 12 extensions
// (with their wire order) follow docs/fingerprint/cpa-reqs/03-tls-target.md.
// No GREASE, no ALPS(17513), no ECH, no padding.
//
// Note: the JA3 target was captured against an IP (no SNI). For production
// connections to a real host, an SNI(server_name) extension is added first per
// OpenSSL convention so the handshake succeeds; that adds extension 0 to the
// JA3 extensions list and flips the JA4 first segment from t13i to t13d, which
// is expected. The cipher/curve/order structure still matches the target.
func newClaudeCLIClientHelloSpec() (*tls.ClientHelloSpec, error) {
	return &tls.ClientHelloSpec{
		// JA3 ciphers: 4865-4866-4867-49195-49199-49196-49200-52393-52392-49161-49171-49162-49172-156-157-47-53
		CipherSuites: []uint16{
			tls.TLS_AES_128_GCM_SHA256,                        // 0x1301
			tls.TLS_AES_256_GCM_SHA384,                        // 0x1302
			tls.TLS_CHACHA20_POLY1305_SHA256,                  // 0x1303
			tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,       // 0xc02b
			tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,         // 0xc02f
			tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,       // 0xc02c
			tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,         // 0xc030
			tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256, // 0xcca9
			tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256,   // 0xcca8
			tls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA,          // 0xc009
			tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA,            // 0xc013
			tls.TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA,          // 0xc00a
			tls.TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA,            // 0xc014
			tls.TLS_RSA_WITH_AES_128_GCM_SHA256,               // 0x009c
			tls.TLS_RSA_WITH_AES_256_GCM_SHA384,               // 0x009d
			tls.TLS_RSA_WITH_AES_128_CBC_SHA,                  // 0x002f
			tls.TLS_RSA_WITH_AES_256_CBC_SHA,                  // 0x0035
		},
		CompressionMethods: []byte{0x00}, // null
		Extensions: []tls.TLSExtension{
			// server_name(0): not in the IP-capture JA3 (extension list
			// 23-65281-10-11-35-16-5-13-18-51-45-43). Added first per OpenSSL
			// convention so the handshake works against a real host. SNI value
			// is filled from tls.Config.ServerName by ApplyPreset.
			&tls.SNIExtension{},
			// 1. extended_master_secret (0x0017 / 23)
			&tls.ExtendedMasterSecretExtension{},
			// 2. renegotiation_info (0xff01 / 65281)
			&tls.RenegotiationInfoExtension{Renegotiation: tls.RenegotiateOnceAsClient},
			// 3. supported_groups (0x000a / 10): x25519, secp256r1, secp384r1
			&tls.SupportedCurvesExtension{Curves: []tls.CurveID{
				tls.X25519,    // 29
				tls.CurveP256, // 23
				tls.CurveP384, // 24
			}},
			// 4. ec_point_formats (0x000b / 11): uncompressed
			&tls.SupportedPointsExtension{SupportedPoints: []uint8{0x00}},
			// 5. session_ticket (0x0023 / 35)
			&tls.SessionTicketExtension{},
			// 6. ALPN (0x0010 / 16): only http/1.1
			&tls.ALPNExtension{AlpnProtocols: append([]string(nil), claudeCLIALPN...)},
			// 7. status_request (0x0005 / 5)
			&tls.StatusRequestExtension{},
			// 8. signature_algorithms (0x000d / 13)
			&tls.SignatureAlgorithmsExtension{SupportedSignatureAlgorithms: []tls.SignatureScheme{
				tls.ECDSAWithP256AndSHA256, // 0x0403
				tls.PSSWithSHA256,          // 0x0804
				tls.PKCS1WithSHA256,        // 0x0401
				tls.ECDSAWithP384AndSHA384, // 0x0503
				tls.PSSWithSHA384,          // 0x0805
				tls.PKCS1WithSHA384,        // 0x0501
				tls.PSSWithSHA512,          // 0x0806
				tls.PKCS1WithSHA512,        // 0x0601
				tls.PKCS1WithSHA1,          // 0x0201
			}},
			// 9. signed_certificate_timestamp / SCT (0x0012 / 18)
			&tls.SCTExtension{},
			// 10. key_share (0x0033 / 51): only x25519 (Data auto-filled by ApplyPreset)
			&tls.KeyShareExtension{KeyShares: []tls.KeyShare{
				{Group: tls.X25519},
			}},
			// 11. psk_key_exchange_modes (0x002d / 45): psk_dhe_ke
			&tls.PSKKeyExchangeModesExtension{Modes: []uint8{tls.PskModeDHE}},
			// 12. supported_versions (0x002b / 43): TLS1.3, TLS1.2
			&tls.SupportedVersionsExtension{Versions: []uint16{
				tls.VersionTLS13,
				tls.VersionTLS12,
			}},
		},
	}, nil
}

// utlsProtectedHosts contains the hosts that should use the utls replicated TLS
// fingerprint to match the claimed client identity on protected upstreams.
var utlsProtectedHosts = map[string]struct{}{
	"api.anthropic.com": {},
	"chatgpt.com":       {},
}

// fallbackRoundTripper uses utls for protected HTTPS hosts and falls back to
// standard transport for all other requests (non-HTTPS or non-protected hosts).
type fallbackRoundTripper struct {
	utls     http.RoundTripper
	fallback http.RoundTripper
}

func (f *fallbackRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme == "https" {
		if _, ok := utlsProtectedHosts[strings.ToLower(req.URL.Hostname())]; ok {
			return f.utls.RoundTrip(req)
		}
	}
	return f.fallback.RoundTrip(req)
}

// NewUtlsHTTPClient creates an HTTP client using a Chrome-like uTLS preset for
// protected API hosts. This is a project-managed preset, not an official Claude
// Code TLS fingerprint or provider-edge parity claim. Its only production
// caller is the codex executor (chatgpt.com); the claude->anthropic default
// outbound path replicates the claude-cli ClientHello separately via the
// core-managed runtime transport profile. Falls back to the standard transport
// for non-HTTPS requests. A round tripper injected via the
// "cliproxy.roundtripper" context value is honored when no explicit proxy is set.
func NewUtlsHTTPClient(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, timeout time.Duration) *http.Client {
	return NewUtlsHTTPClientForProfile(ctx, cfg, auth, timeout, utlsHTTPClientDefaultProfileID)
}

func NewUtlsRoundTripperForProfile(proxyURL string, profileID string) http.RoundTripper {
	clientHello, ok := resolveClaudeClientHelloID(profileID)
	if !ok {
		profileID = claudeCLIClientHelloProfileID
		clientHello, _ = resolveClaudeClientHelloID(claudeCLIClientHelloProfileID)
	}
	var utlsRT *utlsRoundTripper
	if isClaudeStrictHelloCustomProfile(profileID) {
		// The claude strong-fingerprint HelloCustom profile must NEVER downgrade
		// to the Chrome-like fallback: a downgrade would change the outbound
		// fingerprint away from real claude-cli and leak that this is a proxy,
		// defeating the anti-correlation guarantee. Build it in strict mode so a
		// failed handshake (after retries) returns an error instead of falling
		// back to Chrome. This only affects the claude HelloCustom path; the
		// codex Chrome-like default (NewUtlsHTTPClient*) keeps the
		// connectivity-first fallback.
		utlsRT = newStrictUtlsRoundTripper(proxyURL, clientHello)
	} else {
		utlsRT = newUtlsRoundTripper(proxyURL, clientHello)
	}
	return &fallbackRoundTripper{
		utls:     utlsRT,
		fallback: standardTransportForProxy(proxyURL),
	}
}

// isClaudeStrictHelloCustomProfile reports whether the profile is the claude
// strong-fingerprint HelloCustom profile (claude_cli_clienthello_v1 and its
// aliases) that must never downgrade to the Chrome-like fallback. It is keyed on
// the profile identity, not on the resolved ClientHelloID, so other future
// HelloCustom profiles are not implicitly forced into strict mode.
func isClaudeStrictHelloCustomProfile(profileID string) bool {
	switch strings.ToLower(strings.TrimSpace(profileID)) {
	case claudeCLIClientHelloProfileID, "claude_cli_clienthello", "claude_node_openssl_v1":
		return true
	default:
		return false
	}
}

func NewUtlsHTTPClientForProfile(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, timeout time.Duration, profileID string) *http.Client {
	var proxyURL string
	if auth != nil {
		proxyURL = strings.TrimSpace(auth.ProxyURL)
	}
	if proxyURL == "" && cfg != nil {
		proxyURL = strings.TrimSpace(cfg.ProxyURL)
	}

	clientHello, ok := resolveClaudeClientHelloID(profileID)
	if !ok {
		clientHello, _ = resolveClaudeClientHelloID(utlsHTTPClientDefaultProfileID)
	}

	var ctxRoundTripper http.RoundTripper
	if ctx != nil {
		ctxRoundTripper, _ = ctx.Value("cliproxy.roundtripper").(http.RoundTripper)
	}

	var utlsRT http.RoundTripper = newUtlsRoundTripper(proxyURL, clientHello)
	standardTransport := standardTransportForProxy(proxyURL)
	if proxyURL == "" && ctxRoundTripper != nil {
		utlsRT = ctxRoundTripper
		standardTransport = ctxRoundTripper
	}

	client := &http.Client{
		Transport: &fallbackRoundTripper{
			utls:     utlsRT,
			fallback: standardTransport,
		},
	}
	if timeout > 0 {
		client.Timeout = timeout
	}
	return client
}

func standardTransportForProxy(proxyURL string) http.RoundTripper {
	var standardTransport http.RoundTripper = &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
	}
	if proxyURL != "" {
		if transport := buildProxyTransport(proxyURL); transport != nil {
			standardTransport = transport
		}
	}
	return standardTransport
}

func resolveClaudeClientHelloID(profileID string) (tls.ClientHelloID, bool) {
	switch strings.ToLower(strings.TrimSpace(profileID)) {
	case claudeCLIClientHelloProfileID, "claude_cli_clienthello", "claude_node_openssl_v1":
		// P1.5: replicate real claude-cli (Node/OpenSSL) ClientHello.
		return tls.HelloCustom, true
	case "claude_utls_chrome_133", "claude_chrome_like_mac_v3", "chrome_133":
		return tls.HelloChrome_133, true
	case "claude_chrome_like_mac_v1", "chrome_120":
		return tls.HelloChrome_120, true
	case "claude_chrome_like_mac_v2", "chrome_131":
		return tls.HelloChrome_131, true
	default:
		return tls.ClientHelloID{}, false
	}
}
