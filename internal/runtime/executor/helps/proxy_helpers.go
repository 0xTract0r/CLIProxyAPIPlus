package helps

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/proxyutil"
	log "github.com/sirupsen/logrus"
)

// errAccountProxyURLMissing is returned by the blocking transport installed for
// accounts that have no resolved proxy_url. It is surfaced verbatim to the caller
// so that the egress failure reason is unambiguous in logs and error responses.
var errAccountProxyURLMissing = errors.New("account proxy_url missing: refusing direct egress to prevent IP exposure")

// blockingRoundTripper is an http.RoundTripper that never performs any network
// I/O. It is installed for account-scoped HTTP clients whose effective proxy_url
// resolved to empty. Returning an error before any dial guarantees the real
// client IP is never exposed to the upstream provider via an accidental direct
// connection. It deliberately has no fallback path.
type blockingRoundTripper struct{}

// RoundTrip always fails without opening a connection.
func (blockingRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errAccountProxyURLMissing
}

// accountProxyBlockReason reports why an account-scoped proxy setting must be
// refused for direct egress, or "" when it is safe to use. An empty setting
// returns "empty_proxy_url"; a present-but-malformed/unsupported setting
// (proxyutil.ModeInvalid) returns "invalid_proxy_url". The explicit
// "direct"/"none" sentinels and any valid proxy URL return "" (allowed):
// choosing direct egress explicitly is an operator decision, and a valid URL
// routes through that proxy.
func accountProxyBlockReason(proxyURL string) string {
	trimmed := strings.TrimSpace(proxyURL)
	if trimmed == "" {
		return "empty_proxy_url"
	}
	setting, err := proxyutil.Parse(trimmed)
	if err != nil || setting.Mode == proxyutil.ModeInvalid {
		return "invalid_proxy_url"
	}
	return ""
}

// maskEgressAuthID returns a log-safe, partially masked account identifier so
// egress-blocked logs can be correlated per account without printing the full
// auth ID (which may embed an email or file path). It never includes the proxy
// URL or any credential material.
func maskEgressAuthID(authID string) string {
	trimmed := strings.TrimSpace(authID)
	if trimmed == "" {
		return "<unknown>"
	}
	if len(trimmed) <= 8 {
		return trimmed[:1] + "***"
	}
	return trimmed[:4] + "***" + trimmed[len(trimmed)-2:]
}

// logEgressBlocked emits a redacted structured warning that account egress was
// blocked. It deliberately logs only the reason, a masked auth ID and an
// optional site; it NEVER logs the proxy URL or credentials.
func logEgressBlocked(reason, authID, site string) {
	fields := log.Fields{
		"event":   "egress_blocked",
		"reason":  reason,
		"auth_id": maskEgressAuthID(authID),
	}
	if strings.TrimSpace(site) != "" {
		fields["site"] = site
	}
	log.WithFields(fields).Warn("blocking account egress: refusing direct connection to prevent IP exposure")
}

// hasContextRoundTripper reports whether the context carries an explicitly injected
// RoundTripper. Such a transport is a deliberate caller-controlled egress decision,
// so it is not treated as the accidental "no proxy at all" direct path.
func hasContextRoundTripper(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	rt, ok := ctx.Value("cliproxy.roundtripper").(http.RoundTripper)
	return ok && rt != nil
}

// httpClientCache caches HTTP clients by derived transport key to enable connection reuse.
var (
	httpClientCache      = make(map[string]*http.Client)
	httpClientCacheMutex sync.RWMutex
)

// newProxyAwareHTTPClient creates an HTTP client with proper proxy configuration priority:
// 1. Use runtime transport_profile if supported for this auth (highest priority)
// 2. Use auth.ProxyURL if configured
// 3. Use cfg.ProxyURL if auth proxy is not configured
// 4. Use RoundTripper from context if neither are configured
//
// This function caches HTTP clients by derived transport key to enable TCP/TLS connection reuse.
//
// Parameters:
//   - ctx: The context containing optional RoundTripper
//   - cfg: The application configuration
//   - auth: The authentication information
//   - timeout: The client timeout (0 means no timeout)
//
// Returns:
//   - *http.Client: An HTTP client with configured proxy or transport
func newProxyAwareHTTPClient(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, timeout time.Duration) *http.Client {
	var proxyURL string
	if auth != nil {
		proxyURL = strings.TrimSpace(auth.ProxyURL)
	}
	if proxyURL == "" && cfg != nil {
		proxyURL = strings.TrimSpace(cfg.ProxyURL)
	}

	// Global egress guard. When an account-scoped request resolves to no proxy at
	// all (empty) OR to a present-but-invalid proxy_url, the fall-through below
	// would build a direct transport (the utls dialer defaults to proxy.Direct, and
	// an invalid proxy makes buildProxyTransport return nil so the client keeps a
	// direct transport), exposing the real server IP to the upstream provider (a
	// past incident). Instead, hand back a client whose transport always errors
	// before any dial, so no network I/O ever happens. This runs before the runtime
	// transport profile and proxy cache branches.
	//
	// The literal "direct"/"none" sentinels and any VALID proxy URL are allowed:
	// choosing direct egress explicitly is an operator decision, and a valid proxy
	// routes through that proxy. Only empty and invalid values are blocked (see
	// accountProxyBlockReason).
	//
	// auth == nil indicates an infrastructure call (e.g. model registry updates) that
	// is not account egress; those are intentionally left untouched.
	//
	// An explicitly injected context RoundTripper ("cliproxy.roundtripper") is treated
	// as a deliberate caller-controlled egress path (it carries its own proxy/transport
	// decision) and is therefore not an accidental direct connection; in that case the
	// no-proxy fall-through below routes through that RoundTripper instead of a direct
	// dialer, so it is not blocked here.
	if reason := accountProxyBlockReason(proxyURL); auth != nil && reason != "" && !hasContextRoundTripper(ctx) {
		logEgressBlocked(reason, auth.ID, "")
		httpClient := &http.Client{Transport: blockingRoundTripper{}}
		if timeout > 0 {
			httpClient.Timeout = timeout
		}
		return httpClient
	}

	baseURLHost := RuntimeTransportHostFromContext(ctx)
	if baseURLHost == "" {
		baseURLHost = runtimeTransportBaseURLHost(auth)
	}
	// Route A (JA4H "_hd"): replay real claude-cli request-header wire order +
	// casing on the claude serving/quota transport when the config flag is on.
	// The flag is folded into the cache key (claude auths only) so a hot config
	// toggle yields a distinct cached transport instead of reusing a stale one.
	replayClaudeHeaderOrder := ClaudeWireHeaderOrderReplayEnabled(cfg)
	if transportKey := RuntimeTransportProfileCacheKeyForHost(proxyURL, baseURLHost, auth); transportKey != "" {
		// Fold the header-order flag into the cache key using the SAME provider
		// resolution as the wrapping gate (BuildRuntimeTransportRoundTripperWithOptions
		// switches on ResolveRuntimeTransportProfile(auth).Provider). Keying off the
		// raw auth.Provider instead would miss the edge where auth.Provider is empty
		// but account_settings transport_profile.provider == "claude", letting a hot
		// flag toggle reuse a stale cached transport.
		if replayClaudeHeaderOrder {
			if profile := ResolveRuntimeTransportProfile(auth); profile != nil && profile.Provider == "claude" {
				transportKey += "|claude_hdr_order=v1"
			}
		}
		httpClientCacheMutex.RLock()
		if cachedClient, ok := httpClientCache[transportKey]; ok {
			httpClientCacheMutex.RUnlock()
			if timeout > 0 {
				return &http.Client{Transport: cachedClient.Transport, Timeout: timeout}
			}
			return cachedClient
		}
		httpClientCacheMutex.RUnlock()

		if transport, ok := BuildRuntimeTransportRoundTripperWithOptions(proxyURL, auth, RuntimeTransportRoundTripperOptions{ReplayClaudeHeaderOrder: replayClaudeHeaderOrder}); ok && transport != nil {
			httpClient := &http.Client{Transport: transport}
			if timeout > 0 {
				httpClient.Timeout = timeout
			}
			httpClientCacheMutex.Lock()
			httpClientCache[transportKey] = httpClient
			httpClientCacheMutex.Unlock()
			return httpClient
		}
	}

	// Only cache explicit proxy transports. The no-proxy path may depend on a
	// context-scoped RoundTripper, so reusing a cached empty-key client would
	// accidentally discard per-request transport injection.
	if proxyURL != "" {
		httpClientCacheMutex.RLock()
		if cachedClient, ok := httpClientCache[proxyURL]; ok {
			httpClientCacheMutex.RUnlock()
			if timeout > 0 {
				return &http.Client{
					Transport: cachedClient.Transport,
					Timeout:   timeout,
				}
			}
			return cachedClient
		}
		httpClientCacheMutex.RUnlock()
	}

	// Create new client
	httpClient := &http.Client{}
	if timeout > 0 {
		httpClient.Timeout = timeout
	}

	// If we have a proxy URL configured, set up the transport
	if proxyURL != "" {
		transport := buildProxyTransport(proxyURL)
		if transport != nil {
			httpClient.Transport = transport
			// Cache the client
			httpClientCacheMutex.Lock()
			httpClientCache[proxyURL] = httpClient
			httpClientCacheMutex.Unlock()
			return httpClient
		}
		// If proxy setup failed, log and fall through to context RoundTripper
		log.Debugf("failed to setup proxy from URL: %s, falling back to context transport", proxyutil.Redact(proxyURL))
	}

	// Priority 4: Use RoundTripper from context (typically from RoundTripperFor)
	if rt, ok := ctx.Value("cliproxy.roundtripper").(http.RoundTripper); ok && rt != nil {
		httpClient.Transport = rt
	}

	return httpClient
}

// buildProxyTransport creates an HTTP transport configured for the given proxy URL.
// It supports SOCKS5, HTTP, and HTTPS proxy protocols.
//
// Parameters:
//   - proxyURL: The proxy URL string (e.g., "socks5://user:pass@host:port", "http://host:port")
//
// Returns:
//   - *http.Transport: A configured transport, or nil if the proxy URL is invalid
func buildProxyTransport(proxyURL string) *http.Transport {
	transport, _, errBuild := proxyutil.BuildHTTPTransport(proxyURL)
	if errBuild != nil {
		log.Errorf("%v", errBuild)
		return nil
	}
	return transport
}
