package claude

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/proxyutil"
)

const (
	anthropicHTTPClientTimeout   = 60 * time.Second
	anthropicTLSHandshakeTimeout = 10 * time.Second
	anthropicIdleConnTimeout     = 90 * time.Second
)

var errInvalidClaudeOAuthProxy = errors.New("invalid Claude OAuth proxy configuration")

type rejectedClaudeOAuthProxy struct{}

func (rejectedClaudeOAuthProxy) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errInvalidClaudeOAuthProxy
}

// NewAnthropicHttpClient constructs a standard HTTP client for Claude OAuth.
func NewAnthropicHttpClient(cfg *config.SDKConfig) *http.Client {
	if cfg == nil {
		return newClaudeOAuthHTTPClient("")
	}
	return newClaudeOAuthHTTPClient(cfg.ProxyURL)
}

func newClaudeOAuthHTTPClient(proxyURL string) *http.Client {
	client := &http.Client{Timeout: anthropicHTTPClientTimeout}
	transport, mode, err := proxyutil.BuildHTTPTransport(proxyURL)
	if err != nil || (mode == proxyutil.ModeProxy && !proxyutil.ValidRequestProxy(proxyURL)) ||
		(transport == nil && mode != proxyutil.ModeInherit) {
		client.Transport = rejectedClaudeOAuthProxy{}
		return client
	}
	if transport == nil {
		transport = &http.Transport{Proxy: http.ProxyFromEnvironment}
		if base, ok := http.DefaultTransport.(*http.Transport); ok && base != nil {
			transport = base.Clone()
		}
	}
	if transport.TLSHandshakeTimeout <= 0 {
		transport.TLSHandshakeTimeout = anthropicTLSHandshakeTimeout
	}
	if transport.IdleConnTimeout <= 0 {
		transport.IdleConnTimeout = anthropicIdleConnTimeout
	}
	// The shared HTTPS proxy transport performs its own first TLS handshake.
	// Give its dial and handshake a deadline even if it cloned zero defaults.
	if dialTLS := transport.DialTLSContext; dialTLS != nil {
		transport.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			ctx, cancel := context.WithTimeout(ctx, transport.TLSHandshakeTimeout)
			defer cancel()
			return dialTLS(ctx, network, addr)
		}
	}
	client.Transport = transport
	return client
}
