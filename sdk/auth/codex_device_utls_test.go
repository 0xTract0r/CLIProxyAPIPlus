package auth

import (
	"fmt"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

// TestNewCodexDeviceLoginHTTPClient_UsesUtlsRefreshTransport is an
// anti-correlation red line. The codex device-login flow (usercode + token poll
// to auth.openai.com) must dial through the replicated codex-rs uTLS ClientHello
// (the OAuth-refresh uTLS client, which the helps package red-line tests pin to
// codex_rustls_native_v1 / strict no-downgrade / HTTP/1.1 only), never the
// Go-default TLS stack. A regression to util.SetProxy(&cfg.SDKConfig,
// &http.Client{}) would yield a plain *http.Transport and leak JA3 03117a8e on
// these calls. Asserting the transport type guards that wiring.
func TestNewCodexDeviceLoginHTTPClient_UsesUtlsRefreshTransport(t *testing.T) {
	client := newCodexDeviceLoginHTTPClient(&config.Config{})
	if client == nil {
		t.Fatal("newCodexDeviceLoginHTTPClient returned nil client")
	}

	got := fmt.Sprintf("%T", client.Transport)
	const want = "*helps.oauthRefreshRoundTripper"
	if got != want {
		t.Fatalf("device-login client transport = %s, want %s (a Go-default *http.Transport would leak JA3 03117a8e on auth.openai.com)", got, want)
	}

	// timeout=0 keeps the prior context-governed behavior; a hard client timeout
	// here would truncate the device poll loop.
	if client.Timeout != 0 {
		t.Fatalf("device-login client timeout = %v, want 0 (context-governed; poll bounded by codexDeviceTimeout)", client.Timeout)
	}
}

// TestNewCodexDeviceLoginHTTPClient_NilConfig ensures the proxy lookup tolerates a
// nil config without panicking (the helper guards cfg == nil).
func TestNewCodexDeviceLoginHTTPClient_NilConfig(t *testing.T) {
	if client := newCodexDeviceLoginHTTPClient(nil); client == nil {
		t.Fatal("newCodexDeviceLoginHTTPClient(nil) returned nil client")
	}
}
