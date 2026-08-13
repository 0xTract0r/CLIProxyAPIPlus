package helps

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	tls "github.com/refraction-networking/utls"
)

// TestNewCodexRustlsStrictRoundTripperUsesCodexSpec verifies the shared codex
// WebSocket TLS construction is strict, HelloCustom, and materializes the codex-rs
// (rustls) ClientHelloSpec whose JA3 equals the real codex-rs target. If the
// construction ever leaked the claude-cli spec, dropped no-downgrade, or resolved
// to Chrome, the JA3 assertion (real marshalled ClientHello -> JA3) would fail.
func TestNewCodexRustlsStrictRoundTripperUsesCodexSpec(t *testing.T) {
	t.Parallel()

	rt, err := newCodexRustlsStrictRoundTripper("")
	if err != nil {
		t.Fatalf("newCodexRustlsStrictRoundTripper: %v", err)
	}
	if rt.customSpecID != codexRustlsClientHelloProfileID {
		t.Fatalf("customSpecID = %q, want codex-rs %q", rt.customSpecID, codexRustlsClientHelloProfileID)
	}
	if !rt.disableFallback {
		t.Fatal("codex WebSocket TLS dialer must be strict (disableFallback=true); it must never downgrade codex-rs->Chrome")
	}
	if rt.configuredHello != tls.HelloCustom.Str() {
		t.Fatalf("configuredHello = %q, want HelloCustom %q", rt.configuredHello, tls.HelloCustom.Str())
	}

	spec, err := rt.clientHelloSpec(rt.clientHello)
	if err != nil {
		t.Fatalf("clientHelloSpec: %v", err)
	}
	// Real marshal -> parse -> JA3, asserting the codex-rs target fingerprint.
	assertSpecJA3(t, spec, "chatgpt.com", expectedCodexRustlsJA3, expectedCodexRustlsJA3MD5)
}

// TestNewCodexRustlsTLSDialerReturnsNonNilFailClosed verifies the exported factory
// honors its fail-closed contract: it returns a usable dialing function and no
// error (never a nil dialer that would let a caller silently fall back to bare Go
// TLS).
func TestNewCodexRustlsTLSDialerReturnsNonNilFailClosed(t *testing.T) {
	t.Parallel()

	fn, err := NewCodexRustlsTLSDialer("")
	if err != nil {
		t.Fatalf("NewCodexRustlsTLSDialer returned error: %v", err)
	}
	if fn == nil {
		t.Fatal("NewCodexRustlsTLSDialer returned a nil dialer (fail-closed contract requires a non-nil dialer or an error)")
	}
}

// TestCodexRustlsTLSDialerCompletesCodexHandshake exercises the codex WebSocket TLS
// dialer end-to-end against a local TLS server: it must complete the handshake and
// the round tripper must record the configured HelloCustom (codex-rs) fingerprint,
// proving the codex spec is what actually dials (not a downgrade). The round
// tripper is built the same way NewCodexRustlsTLSDialer builds it; insecure is set
// only so the throwaway test cert is accepted (production keeps verification on).
func TestCodexRustlsTLSDialerCompletesCodexHandshake(t *testing.T) {
	t.Parallel()

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	rt, err := newCodexRustlsStrictRoundTripper("")
	if err != nil {
		t.Fatalf("newCodexRustlsStrictRoundTripper: %v", err)
	}
	rt.insecure = true
	// flakyDialer with failUntil:0 never fails; it just redirects the dial to the
	// local TLS server so the codex-rs handshake runs against a real endpoint.
	rt.dialer = &flakyDialer{failUntil: 0, target: server.Listener.Addr().String()}

	conn, err := rt.dialTLSContext(context.Background(), "tcp", "chatgpt.com:443")
	if err != nil {
		t.Fatalf("codex-rs uTLS dialTLSContext failed: %v", err)
	}
	if conn == nil {
		t.Fatal("expected a connection from the codex-rs uTLS dialer")
	}
	_ = conn.Close()

	state := rt.RuntimeHelloState()
	if state.LastHandshakeHello != tls.HelloCustom.Str() {
		t.Fatalf("LastHandshakeHello = %q, want HelloCustom %q (codex-rs spec must be the one that handshakes)", state.LastHandshakeHello, tls.HelloCustom.Str())
	}
	if state.Downgraded {
		t.Fatal("Downgraded = true, want false (codex-rs WebSocket TLS must never downgrade)")
	}
	if state.FallbackCount != 0 {
		t.Fatalf("FallbackCount = %d, want 0", state.FallbackCount)
	}
}
