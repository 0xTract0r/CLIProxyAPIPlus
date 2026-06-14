package helps

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	utls "github.com/refraction-networking/utls"
)

// failingDialer always fails the underlying TCP dial, so every handshake
// attempt (primary and fallback) fails before any TLS exchange. This makes the
// fallback/strict branching of dialTLSContext deterministic without real
// network access.
type failingDialer struct{}

func (failingDialer) Dial(network, addr string) (net.Conn, error) {
	return nil, errors.New("dial refused by test stub")
}

// redirectDialer ignores the requested addr and dials a fixed test target,
// letting dialTLSContext perform a real uTLS handshake against a local TLS
// server while the round tripper still believes it is connecting to a
// protected host.
type redirectDialer struct {
	target string
}

func (d redirectDialer) Dial(network, addr string) (net.Conn, error) {
	return net.Dial(network, d.target)
}

func TestDialTLSContextFallbackRecordsObservableState(t *testing.T) {
	t.Parallel()

	rt := newUtlsRoundTripper("", utls.HelloCustom)
	rt.dialer = failingDialer{}

	conn, err := rt.dialTLSContext(context.Background(), "tcp", "api.anthropic.com:443")
	if err == nil {
		if conn != nil {
			_ = conn.Close()
		}
		t.Fatal("expected dialTLSContext to fail when underlying dial fails")
	}

	state := rt.RuntimeHelloState()
	if state.ConfiguredHello != utls.HelloCustom.Str() {
		t.Fatalf("ConfiguredHello = %q, want %q", state.ConfiguredHello, utls.HelloCustom.Str())
	}
	if state.FallbackCount != 1 {
		t.Fatalf("FallbackCount = %d, want 1 (one silent downgrade attempt)", state.FallbackCount)
	}
	if !state.Downgraded {
		t.Fatal("Downgraded = false, want true after a fallback occurred")
	}
	// No handshake succeeded, so the last handshake hello stays empty.
	if state.LastHandshakeHello != "" {
		t.Fatalf("LastHandshakeHello = %q, want empty (no successful handshake)", state.LastHandshakeHello)
	}
}

func TestDialTLSContextStrictModeDoesNotFallBack(t *testing.T) {
	t.Parallel()

	rt := newStrictUtlsRoundTripper("", utls.HelloCustom)
	rt.dialer = failingDialer{}

	conn, err := rt.dialTLSContext(context.Background(), "tcp", "api.anthropic.com:443")
	if err == nil {
		if conn != nil {
			_ = conn.Close()
		}
		t.Fatal("expected strict-mode dialTLSContext to return the primary error")
	}

	state := rt.RuntimeHelloState()
	if state.FallbackCount != 0 {
		t.Fatalf("FallbackCount = %d, want 0 in strict mode (no downgrade)", state.FallbackCount)
	}
	if state.Downgraded {
		t.Fatal("Downgraded = true, want false in strict mode")
	}
	if state.LastHandshakeHello != "" {
		t.Fatalf("LastHandshakeHello = %q, want empty in strict mode", state.LastHandshakeHello)
	}
}

func TestDialTLSContextSuccessRecordsConfiguredHello(t *testing.T) {
	t.Parallel()

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	serverAddr := server.Listener.Addr().String()

	rt := newUtlsRoundTripper("", utls.HelloCustom)
	rt.dialer = redirectDialer{target: serverAddr}
	// The local test server presents a self-signed cert; skip verification so
	// the handshake itself (not cert chain validation) is what we exercise.
	rt.insecure = true

	conn, err := rt.dialTLSContext(context.Background(), "tcp", "api.anthropic.com:443")
	if err != nil {
		t.Fatalf("dialTLSContext returned error against local TLS server: %v", err)
	}
	if conn == nil {
		t.Fatal("dialTLSContext returned nil conn without error")
	}
	if errClose := conn.Close(); errClose != nil {
		t.Fatalf("conn close returned error: %v", errClose)
	}

	state := rt.RuntimeHelloState()
	if state.FallbackCount != 0 {
		t.Fatalf("FallbackCount = %d, want 0 when the configured handshake succeeds", state.FallbackCount)
	}
	if state.Downgraded {
		t.Fatal("Downgraded = true, want false when the configured handshake succeeds")
	}
	if state.LastHandshakeHello != utls.HelloCustom.Str() {
		t.Fatalf("LastHandshakeHello = %q, want configured %q", state.LastHandshakeHello, utls.HelloCustom.Str())
	}
}

func TestFallbackRoundTripperForwardsRuntimeHelloState(t *testing.T) {
	t.Parallel()

	inner := newUtlsRoundTripper("", utls.HelloCustom)
	inner.dialer = failingDialer{}
	if _, err := inner.dialTLSContext(context.Background(), "tcp", "api.anthropic.com:443"); err == nil {
		t.Fatal("expected primary dial to fail for fallback accounting")
	}

	wrapper := &fallbackRoundTripper{utls: inner, fallback: http.DefaultTransport}

	var observer RuntimeHelloObserver = wrapper
	state := observer.RuntimeHelloState()
	if state.FallbackCount != 1 || !state.Downgraded {
		t.Fatalf("forwarded state = %#v, want FallbackCount=1 Downgraded=true", state)
	}
	if state.ConfiguredHello != utls.HelloCustom.Str() {
		t.Fatalf("forwarded ConfiguredHello = %q, want %q", state.ConfiguredHello, utls.HelloCustom.Str())
	}
}

func TestFallbackRoundTripperRuntimeHelloStateZeroWhenInnerNotObserver(t *testing.T) {
	t.Parallel()

	wrapper := &fallbackRoundTripper{utls: http.DefaultTransport, fallback: http.DefaultTransport}
	state := wrapper.RuntimeHelloState()
	if state.ConfiguredHello != "" || state.LastHandshakeHello != "" || state.FallbackCount != 0 || state.Downgraded {
		t.Fatalf("expected zero RuntimeHelloState for non-observer inner transport, got %#v", state)
	}
}

func TestApplyRuntimeHelloStateFoldsIntoProbeResult(t *testing.T) {
	t.Parallel()

	inner := newUtlsRoundTripper("", utls.HelloCustom)
	inner.dialer = failingDialer{}
	if _, err := inner.dialTLSContext(context.Background(), "tcp", "api.anthropic.com:443"); err == nil {
		t.Fatal("expected primary dial to fail for fallback accounting")
	}
	wrapper := &fallbackRoundTripper{utls: inner, fallback: http.DefaultTransport}

	result := ProviderTLSProbeResult{}
	applyRuntimeHelloState(&result, wrapper)

	if result.RuntimeHelloConfigured != utls.HelloCustom.Str() {
		t.Fatalf("RuntimeHelloConfigured = %q, want %q", result.RuntimeHelloConfigured, utls.HelloCustom.Str())
	}
	if result.RuntimeHelloFallbackCount != 1 {
		t.Fatalf("RuntimeHelloFallbackCount = %d, want 1", result.RuntimeHelloFallbackCount)
	}
	if !result.RuntimeHelloDowngraded {
		t.Fatal("RuntimeHelloDowngraded = false, want true")
	}
}

func TestApplyRuntimeHelloStateNoOpForNonObserver(t *testing.T) {
	t.Parallel()

	result := ProviderTLSProbeResult{RuntimeHelloConfigured: "sentinel"}
	applyRuntimeHelloState(&result, http.DefaultTransport)
	if result.RuntimeHelloConfigured != "sentinel" {
		t.Fatalf("applyRuntimeHelloState mutated result for non-observer transport: %q", result.RuntimeHelloConfigured)
	}
}
