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
	// all, the no-proxy fall-through below would build a direct transport (the utls
	// dialer defaults to proxy.Direct), exposing the real server IP to the upstream
	// provider (a past incident). Instead, hand back a client whose transport always
	// errors before any dial, so no network I/O ever happens. This runs before the
	// runtime transport profile and proxy cache branches.
	//
	// The literal "direct"/"none" sentinels stay non-empty here and remain allowed,
	// because choosing direct egress explicitly is an operator decision. Only the
	// accidental empty case is blocked.
	//
	// auth == nil indicates an infrastructure call (e.g. model registry updates) that
	// is not account egress; those are intentionally left untouched.
	//
	// An explicitly injected context RoundTripper ("cliproxy.roundtripper") is treated
	// as a deliberate caller-controlled egress path (it carries its own proxy/transport
	// decision) and is therefore not an accidental direct connection; in that case the
	// no-proxy fall-through below routes through that RoundTripper instead of a direct
	// dialer, so it is not blocked here.
	if auth != nil && proxyURL == "" && !hasContextRoundTripper(ctx) {
		log.Warnf("blocking account egress: proxy_url missing for auth %q; refusing direct connection to prevent IP exposure", auth.ID)
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
	if transportKey := RuntimeTransportProfileCacheKeyForHost(proxyURL, baseURLHost, auth); transportKey != "" {
		httpClientCacheMutex.RLock()
		if cachedClient, ok := httpClientCache[transportKey]; ok {
			httpClientCacheMutex.RUnlock()
			if timeout > 0 {
				return &http.Client{Transport: cachedClient.Transport, Timeout: timeout}
			}
			return cachedClient
		}
		httpClientCacheMutex.RUnlock()

		if transport, ok := BuildRuntimeTransportRoundTripper(proxyURL, auth); ok && transport != nil {
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
