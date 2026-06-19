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

// codexRustlsClientHelloProfileID is the project profile that replicates the
// real codex-rs (Rust/rustls) ClientHello, targeting
// JA3 e4d448cdfe06dc1243c1eb026c74ac9a. Like the claude-cli profile it resolves
// to uTLS HelloCustom, but it builds a DIFFERENT spec
// (newCodexRustlsClientHelloSpec): a TLS1.2-only ClientHello with no GREASE / no
// ALPN / no key_share / no supported_versions. Because both custom profiles map
// onto HelloCustom (identical ClientHelloID.Str()), the round tripper must
// remember which custom spec to build via utlsRoundTripper.customSpecID; the
// ClientHelloID alone cannot disambiguate them.
const codexRustlsClientHelloProfileID = "codex_rustls_native_v1"

// CodexRustlsClientHelloProfileID is the exported codex-rs ClientHello profile,
// used by the codex executor's outbound HTTP clients so codex traffic explicitly
// selects the codex-rs (rustls) fingerprint instead of relying on the package
// default. It must stay in sync with codexRustlsClientHelloProfileID.
const CodexRustlsClientHelloProfileID = codexRustlsClientHelloProfileID

// utlsHTTPClientDefaultProfileID is the default ClientHello profile for
// NewUtlsHTTPClient. Its only production caller is the codex executor
// (host chatgpt.com), so the default replicates the real codex-rs (rustls)
// ClientHello. It must NOT be the claude-cli HelloCustom profile (that would
// misrepresent the codex client) and must NOT stay the previously misconfigured
// Chrome-like preset (claude_utls_chrome_133), which does not match the real
// codex-rs JA3.
const utlsHTTPClientDefaultProfileID = codexRustlsClientHelloProfileID

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
// (HelloCustom: the claude-cli spec for claude, the codex-rs spec for codex)
// handshake is attempted before giving up. Because the dominant failure mode is
// a transient proxy dial timeout, retrying the SAME ClientHello spec recovers
// most of these without ever changing the outbound fingerprint. The first
// attempt plus up to (utlsHandshakeMaxAttempts-1) retries.
const utlsHandshakeMaxAttempts = 3

// utlsHandshakeRetryBackoff is the base delay between configured-ClientHello
// handshake attempts. Backoff is linear (attempt index * base) and small, since
// the goal is to ride over a brief proxy hiccup, not to wait out a long outage.
const utlsHandshakeRetryBackoff = 200 * time.Millisecond

// utlsRoundTripper implements http.RoundTripper using utls to replicate a
// target client TLS fingerprint on protected API hosts. The two production
// profiles both resolve to HelloCustom + ALPN http/1.1 and are disambiguated by
// customSpecID: real claude-cli (claude->anthropic) and real codex-rs
// (codex->chatgpt.com). Legacy Chrome-like presets remain only as the strict-mode
// fallback target and for non-production callers. Transport is HTTP/1.1 only.
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
	// customSpecID selects WHICH HelloCustom ClientHelloSpec to build when the
	// configured ClientHelloID is tls.HelloCustom. Both the claude-cli and the
	// codex-rs profiles resolve to HelloCustom (identical ClientHelloID.Str()),
	// so the ID alone cannot tell them apart; this profile string does. Empty
	// (the default) means the claude-cli spec (newClaudeCLIClientHelloSpec),
	// preserving the historical behavior of every existing HelloCustom caller
	// (claude outbound, the TLS evidence probe and capture paths). Only the
	// codex outbound path sets it to codexRustlsClientHelloProfileID.
	customSpecID string
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
// production path for strong-fingerprint profiles (the claude-cli and codex-rs
// HelloCustom profiles; anti-correlation: fail rather than leak a downgraded
// fingerprint). Only non-strict profiles keep the connectivity-first fallback via
// newUtlsRoundTripper.
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
// configured ClientHello is HelloCustom, it applies the claude-cli or codex-rs
// ClientHello spec (selected by customSpecID). The configured ClientHello is
// retried up to utlsHandshakeMaxAttempts times (same spec, same fingerprint) to
// ride over transient proxy dial timeouts, which are the dominant failure mode.
// After the retries are exhausted:
//   - disableFallback=true (the claude-cli and codex-rs HelloCustom strict
//     profiles): return the real handshake error so the request fails. The
//     configured fingerprint is NEVER downgraded to Chrome; downgrading would
//     leak that this is not the real client (claude-cli / codex-rs) and defeat
//     the anti-correlation guarantee.
//   - disableFallback=false (non-strict callers): fall back to the Chrome-like
//     ClientHello so connectivity is preserved.
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
	// If the configured ClientHello already IS the Chrome-like preset, there is
	// nothing to downgrade to: return the error directly.
	if t.clientHello.Str() == fallbackHello.Str() {
		return nil, err
	}
	if t.disableFallback {
		// Strict mode (claude-cli or codex-rs HelloCustom): surface the failure
		// instead of downgrading. The configured fingerprint is preserved or
		// nothing. fallbackCount stays 0; record this as a hard failure so the
		// runtime state reflects "strict profile enforced, did not downgrade".
		atomic.AddInt64(&t.hardFailCount, 1)
		log.Warnf("utls: strict HelloCustom handshake failed for %s after %d attempt(s); request failing without downgrade to preserve fingerprint (%v)", host, utlsHandshakeMaxAttempts, err)
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
		// Both the claude-cli and codex-rs profiles use HelloCustom, so the
		// ClientHelloID cannot disambiguate them; dispatch on the configured
		// customSpecID. Only the codex outbound path sets it; every other
		// HelloCustom caller falls through to the claude-cli spec unchanged.
		if t.customSpecID == codexRustlsClientHelloProfileID {
			return newCodexRustlsClientHelloSpec()
		}
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
// No GREASE, no ALPS(17513), no ECH. A trailing conditional RFC7685 padding
// extension matches Node OpenSSL/BoringSSL behavior (see below).
//
// Note: the JA3 target was captured against an IP (no SNI). For production
// connections to a real host, an SNI(server_name) extension is added first per
// OpenSSL convention so the handshake succeeds; that adds extension 0 to the
// JA3 extensions list and flips the JA4 first segment from t13i to t13d, which
// is expected. The cipher/curve/order structure still matches the target.
//
// padding(21/0x15): real claude-cli runs on Node/OpenSSL, which appends an
// RFC7685 padding extension only when the unpadded ClientHello length falls in
// [256,511] bytes, padding it up to 512 (BoringSSL t1_lib.c convention). With
// SNI(api.anthropic.com) the ClientHello lands in that range, so the real
// client emits padding and its JA3 extension list ends with ...-43-21
// (with-SNI JA3 d871d02cecbde59abbf8f4806134addf). Without SNI the ClientHello
// is < 256 bytes, so no padding is emitted and the structural no-SNI JA3 stays
// e97f5146a7009cc2918b50e903b6ff8d. tls.BoringPaddingStyle reproduces exactly
// this conditional behavior, so it must NOT be replaced with an always-on pad,
// which would corrupt the no-SNI fingerprint.
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
			// 13. padding (0x0015 / 21): conditional RFC7685 padding, matching
			// Node OpenSSL/BoringSSL. BoringPaddingStyle pads to 512 bytes ONLY
			// when the unpadded ClientHello length is in [256,511]; otherwise it
			// emits nothing. With SNI the ClientHello lands in that range and
			// padding(21) appears last (with-SNI JA3 d871d02c...); without SNI
			// the ClientHello is < 256 bytes and no padding is added (no-SNI JA3
			// stays e97f5146...). Must stay conditional, never always-on.
			&tls.UtlsPaddingExtension{GetPaddingLen: tls.BoringPaddingStyle},
		},
	}, nil
}

// newCodexRustlsClientHelloSpec builds the uTLS ClientHelloSpec that replicates
// the real codex-rs (Rust/rustls) ClientHello captured from codex-rs 0.140.0,
// targeting JA3 e4d448cdfe06dc1243c1eb026c74ac9a (stable across multiple
// captures). It is deliberately TLS1.2-only and minimal:
//
//   - TLSVersMin/Max are pinned to VersionTLS12. Because no
//     SupportedVersionsExtension is listed, uTLS keeps client_version at 0x0303
//     and emits NO supported_versions extension (TLS1.3 is never advertised).
//   - 22 ordered cipher suites starting with the SCSV (0x00ff). The empty
//     renegotiation info is carried as the SCSV cipher, NOT as a
//     renegotiation_info(0xff01) extension, matching the capture.
//   - exactly 7 ordered extensions: server_name(0), supported_groups(10),
//     ec_point_formats(11), signature_algorithms(13), status_request(5),
//     SCT(18), extended_master_secret(23).
//   - supported_groups are secp256r1/secp384r1/secp521r1 only (NO x25519).
//   - NO GREASE, NO ALPN(16), NO key_share(51), NO psk_key_exchange_modes(45),
//     NO session_ticket(35), NO padding(21). Listing any of those, or a
//     SupportedVersionsExtension, would make uTLS emit extra fields and break
//     the JA3 match, so they are intentionally absent.
//
// The capture was taken against 127.0.0.1 WITH SNI, so the target JA3 already
// includes extension 0 (server_name); the SNI value is filled from
// tls.Config.ServerName by ApplyPreset for real-host handshakes.
func newCodexRustlsClientHelloSpec() (*tls.ClientHelloSpec, error) {
	return &tls.ClientHelloSpec{
		// TLS1.2 only: pin both bounds so uTLS does not default to advertising
		// TLS1.3 via a supported_versions extension.
		TLSVersMin: tls.VersionTLS12,
		TLSVersMax: tls.VersionTLS12,
		// 有序 cipher suites，对应真实 codex-rs 抓样（首位为 SCSV 0x00ff）。
		// JA3 ciphers: 255-49196-49195-49188-49187-49162-49161-49160-49200-49199-49192-49191-49172-49171-49170-157-156-61-60-53-47-10
		CipherSuites: []uint16{
			tls.FAKE_TLS_EMPTY_RENEGOTIATION_INFO_SCSV,           // 0x00ff (SCSV)
			tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,          // 0xc02c
			tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,          // 0xc02b
			tls.DISABLED_TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA384, // 0xc024
			tls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA256,          // 0xc023
			tls.TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA,             // 0xc00a
			tls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA,             // 0xc009
			tls.FAKE_TLS_ECDHE_ECDSA_WITH_3DES_EDE_CBC_SHA,       // 0xc008
			tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,            // 0xc030
			tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,            // 0xc02f
			tls.DISABLED_TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA384,   // 0xc028
			tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA256,            // 0xc027
			tls.TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA,               // 0xc014
			tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA,               // 0xc013
			tls.TLS_ECDHE_RSA_WITH_3DES_EDE_CBC_SHA,              // 0xc012
			tls.TLS_RSA_WITH_AES_256_GCM_SHA384,                  // 0x009d
			tls.TLS_RSA_WITH_AES_128_GCM_SHA256,                  // 0x009c
			tls.DISABLED_TLS_RSA_WITH_AES_256_CBC_SHA256,         // 0x003d
			tls.TLS_RSA_WITH_AES_128_CBC_SHA256,                  // 0x003c
			tls.TLS_RSA_WITH_AES_256_CBC_SHA,                     // 0x0035
			tls.TLS_RSA_WITH_AES_128_CBC_SHA,                     // 0x002f
			tls.TLS_RSA_WITH_3DES_EDE_CBC_SHA,                    // 0x000a
		},
		CompressionMethods: []byte{0x00}, // null
		Extensions: []tls.TLSExtension{
			// 有序扩展：0,10,11,13,5,18,23（与真实 codex-rs 抓样一致）。
			// 1. server_name (0x0000 / 0): SNI 值由 ApplyPreset 从 ServerName 填充。
			&tls.SNIExtension{},
			// 2. supported_groups (0x000a / 10): secp256r1, secp384r1, secp521r1
			//    （无 x25519）。
			&tls.SupportedCurvesExtension{Curves: []tls.CurveID{
				tls.CurveP256, // 23 (0x17)
				tls.CurveP384, // 24 (0x18)
				tls.CurveP521, // 25 (0x19)
			}},
			// 3. ec_point_formats (0x000b / 11): uncompressed
			&tls.SupportedPointsExtension{SupportedPoints: []uint8{0x00}},
			// 4. signature_algorithms (0x000d / 13)
			&tls.SignatureAlgorithmsExtension{SupportedSignatureAlgorithms: []tls.SignatureScheme{
				tls.PKCS1WithSHA256,        // 0x0401
				tls.PKCS1WithSHA1,          // 0x0201
				tls.PKCS1WithSHA384,        // 0x0501
				tls.PKCS1WithSHA512,        // 0x0601
				tls.ECDSAWithP256AndSHA256, // 0x0403
				tls.ECDSAWithSHA1,          // 0x0203
				tls.ECDSAWithP384AndSHA384, // 0x0503
				tls.ECDSAWithP521AndSHA512, // 0x0603
			}},
			// 5. status_request / OCSP (0x0005 / 5)
			&tls.StatusRequestExtension{},
			// 6. signed_certificate_timestamp / SCT (0x0012 / 18)
			&tls.SCTExtension{},
			// 7. extended_master_secret (0x0017 / 23)
			&tls.ExtendedMasterSecretExtension{},
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

// NewUtlsHTTPClient creates an HTTP client that replicates the real codex-rs
// (rustls) ClientHello for protected API hosts. This is a project-managed
// fingerprint, not an official provider-edge parity claim. Its only production
// caller is the codex executor (chatgpt.com), so the default profile is the
// codex-rs HelloCustom profile and runs in no-downgrade (strict) mode: a failed
// handshake fails the request rather than downgrading to a Chrome133 ClientHello
// that would mismatch the codex-rs UA. The claude->anthropic default outbound
// path replicates the claude-cli ClientHello separately via the core-managed
// runtime transport profile. Falls back to the standard transport for non-HTTPS
// requests. A round tripper injected via the "cliproxy.roundtripper" context
// value is honored when no explicit proxy is set.
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
	if profileRequiresNoDowngrade(profileID) {
		// The claude strong-fingerprint HelloCustom profile AND the codex-rs
		// HelloCustom profile must NEVER downgrade to the Chrome-like fallback: a
		// downgrade would change the outbound TLS fingerprint to Chrome133 while
		// the UA still claims claude-cli / codex-rs, leaking that this is a proxy
		// and defeating the anti-correlation guarantee. Build it in strict mode so
		// a failed handshake (after retries) returns an error instead of falling
		// back to Chrome. Profiles outside this set keep the connectivity-first
		// fallback.
		utlsRT = newStrictUtlsRoundTripper(proxyURL, clientHello)
	} else {
		utlsRT = newUtlsRoundTripper(proxyURL, clientHello)
	}
	utlsRT.customSpecID = codexCustomSpecID(profileID)
	return &fallbackRoundTripper{
		utls:     utlsRT,
		fallback: standardTransportForProxy(proxyURL),
	}
}

// codexCustomSpecID returns the customSpecID a round tripper should use for the
// given profile. It only recognizes the codex-rs HelloCustom profile; every
// other profile (claude HelloCustom, Chrome presets, etc.) returns "" so the
// HelloCustom path keeps building the claude-cli spec, leaving claude outbound
// completely unaffected.
func codexCustomSpecID(profileID string) string {
	if strings.EqualFold(strings.TrimSpace(profileID), codexRustlsClientHelloProfileID) {
		return codexRustlsClientHelloProfileID
	}
	return ""
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

// isCodexNoDowngradeProfile reports whether the profile is the codex-rs
// HelloCustom profile (codex_rustls_native_v1) that must also never downgrade to
// the Chrome-like fallback. The codex-rs ClientHello is paired with a codex-rs
// User-Agent; downgrading to a Chrome133 ClientHello while still sending the
// codex-rs UA would re-create exactly the UA/TLS mismatch this profile exists to
// eliminate. Like the claude predicate it is keyed on the profile identity, not
// the resolved ClientHelloID (both custom profiles resolve to HelloCustom).
func isCodexNoDowngradeProfile(profileID string) bool {
	return strings.EqualFold(strings.TrimSpace(profileID), codexRustlsClientHelloProfileID)
}

// profileRequiresNoDowngrade reports whether the profile must run in strict
// (fail-closed) mode: a failed configured handshake returns an error instead of
// silently downgrading to the Chrome-like ClientHello. This covers BOTH the
// claude strong-fingerprint HelloCustom profile and the codex-rs HelloCustom
// profile, because in both cases the outbound User-Agent identifies a specific
// client and a downgraded Chrome133 TLS fingerprint would leak the proxy by
// mismatching that UA. Profiles outside this set keep the connectivity-first
// fallback. Request failure is preferred over a mismatched (leaked) fingerprint.
func profileRequiresNoDowngrade(profileID string) bool {
	return isClaudeStrictHelloCustomProfile(profileID) || isCodexNoDowngradeProfile(profileID)
}

func NewUtlsHTTPClientForProfile(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, timeout time.Duration, profileID string) *http.Client {
	var proxyURL string
	if auth != nil {
		proxyURL = strings.TrimSpace(auth.ProxyURL)
	}
	if proxyURL == "" && cfg != nil {
		proxyURL = strings.TrimSpace(cfg.ProxyURL)
	}

	// effectiveProfileID is the profile actually used to resolve the
	// ClientHelloID. When the requested profile is unknown it falls back to the
	// codex-facing default, so the customSpecID must track that same fallback;
	// otherwise an unknown profile would resolve to the codex HelloCustom ID but
	// build the claude-cli spec, leaking the wrong fingerprint.
	effectiveProfileID := profileID
	clientHello, ok := resolveClaudeClientHelloID(profileID)
	if !ok {
		effectiveProfileID = utlsHTTPClientDefaultProfileID
		clientHello, _ = resolveClaudeClientHelloID(utlsHTTPClientDefaultProfileID)
	}

	var ctxRoundTripper http.RoundTripper
	if ctx != nil {
		ctxRoundTripper, _ = ctx.Value("cliproxy.roundtripper").(http.RoundTripper)
	}

	baseUtlsRT := newUtlsRoundTripper(proxyURL, clientHello)
	baseUtlsRT.customSpecID = codexCustomSpecID(effectiveProfileID)
	if profileRequiresNoDowngrade(effectiveProfileID) {
		// fail-closed (no-downgrade) for strong-fingerprint profiles. This is the
		// production codex path (4 codex executor call sites pass
		// CodexRustlsClientHelloProfileID): if the codex-rs HelloCustom handshake
		// fails after retries, return the error rather than serving a Chrome133
		// ClientHello under a codex-rs UA. Request failure is preferred over
		// leaking a mismatched fingerprint. The claude HelloCustom profile is also
		// covered here for consistency, though claude outbound uses the
		// core-managed transport profile rather than this client.
		baseUtlsRT.disableFallback = true
	}
	var utlsRT http.RoundTripper = baseUtlsRT
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
	case codexRustlsClientHelloProfileID:
		// Replicate real codex-rs (Rust/rustls) ClientHello. Also HelloCustom;
		// the spec is selected by utlsRoundTripper.customSpecID, not the ID.
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
