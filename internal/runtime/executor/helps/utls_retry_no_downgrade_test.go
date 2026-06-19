package helps

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	utls "github.com/refraction-networking/utls"
)

// flakyDialer fails the first failUntil dials, then redirects to target. It lets
// the retry path be exercised deterministically without real network flakiness:
// the early failures stand in for transient socks5 proxy dial timeouts, the
// later success for the proxy recovering on retry.
type flakyDialer struct {
	failUntil int32
	calls     int32
	target    string
}

func (d *flakyDialer) Dial(network, addr string) (net.Conn, error) {
	n := atomic.AddInt32(&d.calls, 1)
	if n <= atomic.LoadInt32(&d.failUntil) {
		return nil, errors.New("transient dial timeout (test stub)")
	}
	return net.Dial(network, d.target)
}

// TestDialTLSContextRetriesHelloCustomThenSucceeds verifies that a transient
// dial failure is retried with the SAME HelloCustom spec (not Chrome), and that
// once the dial recovers the handshake completes on HelloCustom. RetryCount
// reflects the extra attempt, FallbackCount stays 0 (no downgrade happened).
func TestDialTLSContextRetriesHelloCustomThenSucceeds(t *testing.T) {
	t.Parallel()

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	rt := newStrictUtlsRoundTripper("", utls.HelloCustom)
	rt.insecure = true
	// Fail the first dial, succeed on the retry.
	rt.dialer = &flakyDialer{failUntil: 1, target: server.Listener.Addr().String()}

	conn, err := rt.dialTLSContext(context.Background(), "tcp", "api.anthropic.com:443")
	if err != nil {
		t.Fatalf("dialTLSContext should recover via HelloCustom retry, got error: %v", err)
	}
	if conn == nil {
		t.Fatal("expected a connection after successful retry")
	}
	_ = conn.Close()

	state := rt.RuntimeHelloState()
	if state.LastHandshakeHello != utls.HelloCustom.Str() {
		t.Fatalf("LastHandshakeHello = %q, want HelloCustom %q (retry must reuse the configured fingerprint)", state.LastHandshakeHello, utls.HelloCustom.Str())
	}
	if state.FallbackCount != 0 {
		t.Fatalf("FallbackCount = %d, want 0 (a successful retry is not a downgrade)", state.FallbackCount)
	}
	if state.RetryCount != 1 {
		t.Fatalf("RetryCount = %d, want 1 (one transient failure retried)", state.RetryCount)
	}
	if state.HardFailCount != 0 {
		t.Fatalf("HardFailCount = %d, want 0 after a successful retry", state.HardFailCount)
	}
	if state.Downgraded {
		t.Fatal("Downgraded = true, want false after a successful HelloCustom retry")
	}
}

// TestDialTLSContextClaudeNeverDowngradesAfterRetriesExhausted verifies the
// core anti-correlation guarantee: when the claude strict HelloCustom profile
// cannot complete the handshake even after retries, dialTLSContext returns an
// error and NEVER downgrades to Chrome. FallbackCount stays 0, the last
// handshake hello is never Chrome, and the failure is recorded as a hard fail.
func TestDialTLSContextClaudeNeverDowngradesAfterRetriesExhausted(t *testing.T) {
	t.Parallel()

	rt := newStrictUtlsRoundTripper("", utls.HelloCustom)
	// Always fail the dial so every HelloCustom attempt fails and retries are
	// exhausted.
	rt.dialer = failingDialer{}

	conn, err := rt.dialTLSContext(context.Background(), "tcp", "api.anthropic.com:443")
	if err == nil {
		if conn != nil {
			_ = conn.Close()
		}
		t.Fatal("expected claude strict mode to return an error, not a downgraded connection")
	}

	state := rt.RuntimeHelloState()
	if state.FallbackCount != 0 {
		t.Fatalf("FallbackCount = %d, want 0 (claude strict profile must never downgrade)", state.FallbackCount)
	}
	if state.Downgraded {
		t.Fatal("Downgraded = true, want false (claude strict profile must never downgrade)")
	}
	// The last handshake hello must never become Chrome: no successful Chrome
	// handshake may be recorded.
	if state.LastHandshakeHello == utls.HelloChrome_133.Str() {
		t.Fatalf("LastHandshakeHello = %q, must never be Chrome for the claude strict profile", state.LastHandshakeHello)
	}
	if state.LastHandshakeHello != "" {
		t.Fatalf("LastHandshakeHello = %q, want empty (no successful handshake)", state.LastHandshakeHello)
	}
	if state.RetryCount != int64(utlsHandshakeMaxAttempts-1) {
		t.Fatalf("RetryCount = %d, want %d (all retries attempted)", state.RetryCount, utlsHandshakeMaxAttempts-1)
	}
	if state.HardFailCount != 1 {
		t.Fatalf("HardFailCount = %d, want 1 (one hard failure with fingerprint preserved)", state.HardFailCount)
	}
	if state.ConfiguredHello != utls.HelloCustom.Str() {
		t.Fatalf("ConfiguredHello = %q, want HelloCustom %q", state.ConfiguredHello, utls.HelloCustom.Str())
	}
}

// TestClaudeStrictProfileWiredFromProfileID verifies that the claude
// strong-fingerprint HelloCustom profile is built with disableFallback=true via
// NewUtlsRoundTripperForProfile, so the no-downgrade behavior is wired by the
// profile and not only available through the explicit strict constructor.
func TestClaudeStrictProfileWiredFromProfileID(t *testing.T) {
	t.Parallel()

	rt := NewUtlsRoundTripperForProfile("", claudeCLIClientHelloProfileID)
	fb, ok := rt.(*fallbackRoundTripper)
	if !ok {
		t.Fatalf("expected *fallbackRoundTripper, got %T", rt)
	}
	inner, ok := fb.utls.(*utlsRoundTripper)
	if !ok {
		t.Fatalf("expected inner *utlsRoundTripper, got %T", fb.utls)
	}
	if !inner.disableFallback {
		t.Fatal("claude HelloCustom profile must be built with disableFallback=true (never downgrade)")
	}
	if inner.configuredHello != utls.HelloCustom.Str() {
		t.Fatalf("configuredHello = %q, want HelloCustom %q", inner.configuredHello, utls.HelloCustom.Str())
	}
}

// TestCodexProfileWiredNoDowngrade verifies the codex production path is now
// fail-closed: NewUtlsHTTPClient (codex's only production caller) and the
// explicit-profile NewUtlsHTTPClientForProfile(CodexRustlsClientHelloProfileID)
// both build with disableFallback=true. The codex-rs ClientHello is paired with a
// codex-rs UA; downgrading to Chrome133 would re-create the UA/TLS mismatch this
// profile exists to remove, so the codex path must never downgrade.
func TestCodexProfileWiredNoDowngrade(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		client *http.Client
	}{
		{"default", NewUtlsHTTPClient(context.Background(), nil, nil, 0)},
		{"explicit-profile", NewUtlsHTTPClientForProfile(context.Background(), nil, nil, 0, CodexRustlsClientHelloProfileID)},
	} {
		fb, ok := tc.client.Transport.(*fallbackRoundTripper)
		if !ok {
			t.Fatalf("[%s] expected *fallbackRoundTripper, got %T", tc.name, tc.client.Transport)
		}
		inner, ok := fb.utls.(*utlsRoundTripper)
		if !ok {
			t.Fatalf("[%s] expected inner *utlsRoundTripper, got %T", tc.name, fb.utls)
		}
		if !inner.disableFallback {
			t.Fatalf("[%s] codex profile must be built with disableFallback=true (never downgrade codex-rs->Chrome133)", tc.name)
		}
		// codex default resolves to the codex-rs HelloCustom + codex spec, not
		// the claude-cli spec and not Chrome133.
		if inner.configuredHello != utls.HelloCustom.Str() {
			t.Fatalf("[%s] configuredHello = %q, want HelloCustom %q (codex-rs default)", tc.name, inner.configuredHello, utls.HelloCustom.Str())
		}
		if inner.customSpecID != codexRustlsClientHelloProfileID {
			t.Fatalf("[%s] codex customSpecID = %q, want codex-rs %q", tc.name, inner.customSpecID, codexRustlsClientHelloProfileID)
		}
	}
}

// TestDialTLSContextCodexNeverDowngradesAfterRetriesExhausted is the codex
// counterpart to TestDialTLSContextClaudeNeverDowngradesAfterRetriesExhausted:
// when the codex-rs HelloCustom handshake cannot complete even after retries,
// dialTLSContext returns an error and NEVER downgrades to Chrome133. This guards
// the core PR #35 fix — a codex-rs UA must never be served over a Chrome133 TLS
// fingerprint. FallbackCount stays 0, the last handshake hello is never Chrome,
// and the failure is recorded as a hard fail.
func TestDialTLSContextCodexNeverDowngradesAfterRetriesExhausted(t *testing.T) {
	t.Parallel()

	// Build the codex round tripper the same way the production codex path does,
	// then force every dial to fail so all retries are exhausted.
	client := NewUtlsHTTPClientForProfile(context.Background(), nil, nil, 0, CodexRustlsClientHelloProfileID)
	fb, ok := client.Transport.(*fallbackRoundTripper)
	if !ok {
		t.Fatalf("expected *fallbackRoundTripper, got %T", client.Transport)
	}
	rt, ok := fb.utls.(*utlsRoundTripper)
	if !ok {
		t.Fatalf("expected inner *utlsRoundTripper, got %T", fb.utls)
	}
	rt.dialer = failingDialer{}

	conn, err := rt.dialTLSContext(context.Background(), "tcp", "chatgpt.com:443")
	if err == nil {
		if conn != nil {
			_ = conn.Close()
		}
		t.Fatal("expected codex strict mode to return an error, not a downgraded connection")
	}

	state := rt.RuntimeHelloState()
	if state.FallbackCount != 0 {
		t.Fatalf("FallbackCount = %d, want 0 (codex profile must never downgrade)", state.FallbackCount)
	}
	if state.Downgraded {
		t.Fatal("Downgraded = true, want false (codex profile must never downgrade)")
	}
	if state.LastHandshakeHello == utls.HelloChrome_133.Str() {
		t.Fatalf("LastHandshakeHello = %q, must never be Chrome133 for the codex profile", state.LastHandshakeHello)
	}
	if state.LastHandshakeHello != "" {
		t.Fatalf("LastHandshakeHello = %q, want empty (no successful handshake)", state.LastHandshakeHello)
	}
	if state.RetryCount != int64(utlsHandshakeMaxAttempts-1) {
		t.Fatalf("RetryCount = %d, want %d (all retries attempted)", state.RetryCount, utlsHandshakeMaxAttempts-1)
	}
	if state.HardFailCount != 1 {
		t.Fatalf("HardFailCount = %d, want 1 (one hard failure with fingerprint preserved)", state.HardFailCount)
	}
	if state.ConfiguredHello != utls.HelloCustom.Str() {
		t.Fatalf("ConfiguredHello = %q, want HelloCustom %q", state.ConfiguredHello, utls.HelloCustom.Str())
	}
	if rt.customSpecID != codexRustlsClientHelloProfileID {
		t.Fatalf("customSpecID = %q, want codex-rs %q (must fail on codex spec, not claude spec)", rt.customSpecID, codexRustlsClientHelloProfileID)
	}
}

// TestNonStrictHelloCustomStillDowngrades verifies that the downgrade path is
// preserved for a non-strict HelloCustom round tripper (built via
// newUtlsRoundTripper, not the claude strict profile), so the no-downgrade
// behavior is scoped to the strict profile only and the legacy connectivity
// fallback still works elsewhere.
func TestNonStrictHelloCustomStillDowngrades(t *testing.T) {
	t.Parallel()

	rt := newUtlsRoundTripper("", utls.HelloCustom)
	rt.dialer = failingDialer{}

	conn, err := rt.dialTLSContext(context.Background(), "tcp", "api.anthropic.com:443")
	if err == nil && conn != nil {
		_ = conn.Close()
	}
	state := rt.RuntimeHelloState()
	if state.FallbackCount != 1 {
		t.Fatalf("FallbackCount = %d, want 1 (non-strict HelloCustom still downgrades)", state.FallbackCount)
	}
	if !state.Downgraded {
		t.Fatal("Downgraded = false, want true for non-strict HelloCustom fallback")
	}
	if state.HardFailCount != 0 {
		t.Fatalf("HardFailCount = %d, want 0 (non-strict path downgrades instead of hard-failing)", state.HardFailCount)
	}
}

// TestDialContextHonorsShortAttemptTimeout verifies the per-attempt dial timeout
// is applied: a dialer that blocks longer than utlsDialAttemptTimeout is
// cancelled rather than hanging on OS TCP defaults. The dial returns within a
// bound comfortably above the attempt timeout but well below a 30s OS default.
func TestDialContextHonorsShortAttemptTimeout(t *testing.T) {
	// Not parallel: this test mutates the package-level utlsDialAttemptTimeout.

	// Shorten the per-attempt timeout for the test; restore afterwards.
	original := utlsDialAttemptTimeout
	utlsDialAttemptTimeout = 300 * time.Millisecond
	defer func() { utlsDialAttemptTimeout = original }()

	rt := newStrictUtlsRoundTripper("", utls.HelloCustom)
	rt.dialer = blockingContextDialer{}

	start := time.Now()
	conn, err := rt.dialContext(context.Background(), "tcp", "api.anthropic.com:443")
	elapsed := time.Since(start)
	if conn != nil {
		_ = conn.Close()
	}
	if err == nil {
		t.Fatal("expected dialContext to fail when the dial exceeds the attempt timeout")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context.DeadlineExceeded from the per-attempt timeout", err)
	}
	// Allow generous slack over the configured attempt timeout, but it must be
	// far below the ~30s OS default this change is meant to avoid.
	if elapsed > utlsDialAttemptTimeout+2*time.Second {
		t.Fatalf("dialContext took %v, want it bounded near utlsDialAttemptTimeout=%v", elapsed, utlsDialAttemptTimeout)
	}
}

// blockingContextDialer blocks until its DialContext ctx is cancelled, so the
// per-attempt timeout (not the dialer) is what ends the dial.
type blockingContextDialer struct{}

func (blockingContextDialer) Dial(network, addr string) (net.Conn, error) {
	select {}
}

func (blockingContextDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
