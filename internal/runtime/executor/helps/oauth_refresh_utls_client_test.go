package helps

import (
	"net/http"
	"testing"
	"time"

	utls "github.com/refraction-networking/utls"
)

// oauthRefreshUtlsRTFromClient extracts the inner *utlsRoundTripper from a client
// built by NewOAuthRefreshUtlsHTTPClient, asserting the OAuth-refresh transport
// shape (oauthRefreshRoundTripper, NOT fallbackRoundTripper: the OAuth path must
// not consult utlsProtectedHosts so auth.openai.com still gets uTLS).
func oauthRefreshUtlsRTFromClient(t *testing.T, client *http.Client) *utlsRoundTripper {
	t.Helper()
	if client == nil {
		t.Fatal("NewOAuthRefreshUtlsHTTPClient returned nil client")
	}
	rt, ok := client.Transport.(*oauthRefreshRoundTripper)
	if !ok {
		t.Fatalf("client transport = %T, want *oauthRefreshRoundTripper", client.Transport)
	}
	utlsRT, ok := rt.utls.(*utlsRoundTripper)
	if !ok {
		t.Fatalf("oauth refresh utls leg = %T, want *utlsRoundTripper", rt.utls)
	}
	return utlsRT
}

// TestOAuthRefreshUtlsHTTPClient_ClaudeProfile asserts the claude->anthropic OAuth
// refresh client uses the claude-cli HelloCustom spec (empty customSpecID), runs
// strict no-downgrade, and speaks HTTP/1.1 only — i.e. the same serving
// fingerprint, closing anti-correlation leak 03117a8e for token refresh.
func TestOAuthRefreshUtlsHTTPClient_ClaudeProfile(t *testing.T) {
	client := NewOAuthRefreshUtlsHTTPClient("", ClaudeCLIClientHelloProfileID, 60*time.Second)

	utlsRT := oauthRefreshUtlsRTFromClient(t, client)

	if utlsRT.configuredHello != utls.HelloCustom.Str() {
		t.Fatalf("claude refresh configuredHello = %q, want HelloCustom %q", utlsRT.configuredHello, utls.HelloCustom.Str())
	}
	// Empty customSpecID => the HelloCustom path builds the claude-cli spec
	// (newClaudeCLIClientHelloSpec), matching claude serving, not the codex spec.
	if utlsRT.customSpecID != "" {
		t.Fatalf("claude refresh customSpecID = %q, want empty (claude-cli spec)", utlsRT.customSpecID)
	}
	// Strict no-downgrade: a failed claude-cli handshake must NOT fall back to a
	// Chrome133 ClientHello (which would emit Chrome TLS under a claude-cli UA).
	if !utlsRT.disableFallback {
		t.Fatal("claude refresh disableFallback = false, want true (strict no-downgrade)")
	}
	// HTTP/1.1 only: the OAuth endpoint must never negotiate HTTP/2, structurally
	// avoiding the historical uTLS-only HTTP/2 re-auth deadlock.
	if utlsRT.transport == nil {
		t.Fatal("claude refresh utls transport is nil")
	}
	if utlsRT.transport.ForceAttemptHTTP2 {
		t.Fatal("claude refresh ForceAttemptHTTP2 = true, want false (HTTP/1.1 only)")
	}
	if client.Timeout != 60*time.Second {
		t.Fatalf("claude refresh client timeout = %v, want 60s", client.Timeout)
	}
}

// TestOAuthRefreshUtlsHTTPClient_CodexProfile asserts the codex->openai OAuth
// refresh client uses the codex-rs HelloCustom spec (customSpecID =
// codex_rustls_native_v1), runs strict no-downgrade, and speaks HTTP/1.1 only.
func TestOAuthRefreshUtlsHTTPClient_CodexProfile(t *testing.T) {
	client := NewOAuthRefreshUtlsHTTPClient("", CodexRustlsClientHelloProfileID, 0)

	utlsRT := oauthRefreshUtlsRTFromClient(t, client)

	if utlsRT.configuredHello != utls.HelloCustom.Str() {
		t.Fatalf("codex refresh configuredHello = %q, want HelloCustom %q", utlsRT.configuredHello, utls.HelloCustom.Str())
	}
	// customSpecID must be the codex-rs profile so the HelloCustom path builds the
	// codex-rs spec (newCodexRustlsClientHelloSpec), matching codex serving.
	if utlsRT.customSpecID != codexRustlsClientHelloProfileID {
		t.Fatalf("codex refresh customSpecID = %q, want codex-rs %q", utlsRT.customSpecID, codexRustlsClientHelloProfileID)
	}
	if !utlsRT.disableFallback {
		t.Fatal("codex refresh disableFallback = false, want true (strict no-downgrade)")
	}
	if utlsRT.transport == nil {
		t.Fatal("codex refresh utls transport is nil")
	}
	if utlsRT.transport.ForceAttemptHTTP2 {
		t.Fatal("codex refresh ForceAttemptHTTP2 = true, want false (HTTP/1.1 only)")
	}
	// timeout=0 preserves the prior context-governed behavior (no http.Client.Timeout).
	if client.Timeout != 0 {
		t.Fatalf("codex refresh client timeout = %v, want 0 (context-governed)", client.Timeout)
	}
}

// TestOAuthRefreshUtlsHTTPClient_HonorsProxy asserts the per-account proxy URL is
// threaded into the uTLS round tripper's dialer (non-Direct), so refresh egresses
// through the same proxy as serving instead of leaking the host's direct IP.
func TestOAuthRefreshUtlsHTTPClient_HonorsProxy(t *testing.T) {
	directClient := NewOAuthRefreshUtlsHTTPClient("", CodexRustlsClientHelloProfileID, 0)
	directRT := oauthRefreshUtlsRTFromClient(t, directClient)
	if directRT.dialer == nil {
		t.Fatal("direct refresh dialer is nil")
	}

	proxiedClient := NewOAuthRefreshUtlsHTTPClient("socks5://127.0.0.1:1080", CodexRustlsClientHelloProfileID, 0)
	proxiedRT := oauthRefreshUtlsRTFromClient(t, proxiedClient)
	if proxiedRT.dialer == nil {
		t.Fatal("proxied refresh dialer is nil")
	}
	// A configured socks5 proxy must yield a different dialer than the direct
	// (proxy.Direct) path; identical dialers would mean the proxy was dropped.
	if proxiedRT.dialer == directRT.dialer {
		t.Fatal("proxied refresh dialer == direct dialer, want proxy dialer (proxy URL was dropped)")
	}
}

// TestOAuthRefreshUtlsHTTPClient_UnknownProfileFallsBackToCodexDefault asserts an
// unknown profile resolves to the codex-rs default (matching
// NewUtlsHTTPClientForProfile) rather than silently using a non-strict or
// wrong-spec client.
func TestOAuthRefreshUtlsHTTPClient_UnknownProfileFallsBackToCodexDefault(t *testing.T) {
	client := NewOAuthRefreshUtlsHTTPClient("", "totally-unknown-profile", 0)
	utlsRT := oauthRefreshUtlsRTFromClient(t, client)

	if utlsRT.customSpecID != codexRustlsClientHelloProfileID {
		t.Fatalf("unknown-profile refresh customSpecID = %q, want codex-rs default %q", utlsRT.customSpecID, codexRustlsClientHelloProfileID)
	}
	if !utlsRT.disableFallback {
		t.Fatal("unknown-profile refresh disableFallback = false, want true (codex-rs default is strict)")
	}
}
