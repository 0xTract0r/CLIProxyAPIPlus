package proxyutil

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"

	"golang.org/x/net/proxy"
)

// Mode describes how a proxy setting should be interpreted.
type Mode int

const (
	// ModeInherit means no explicit proxy behavior was configured.
	ModeInherit Mode = iota
	// ModeDirect means outbound requests must bypass proxies explicitly.
	ModeDirect
	// ModeProxy means a concrete proxy URL was configured.
	ModeProxy
	// ModeInvalid means the proxy setting is present but malformed or unsupported.
	ModeInvalid
)

// Setting is the normalized interpretation of a proxy configuration value.
type Setting struct {
	Raw  string
	Mode Mode
	URL  *url.URL
}

// Parse normalizes a proxy configuration value into inherit, direct, or proxy modes.
func Parse(raw string) (Setting, error) {
	trimmed := strings.TrimSpace(raw)
	setting := Setting{Raw: trimmed}

	if trimmed == "" {
		setting.Mode = ModeInherit
		return setting, nil
	}

	if strings.EqualFold(trimmed, "direct") || strings.EqualFold(trimmed, "none") {
		setting.Mode = ModeDirect
		return setting, nil
	}

	parsedURL, errParse := url.Parse(trimmed)
	if errParse != nil {
		setting.Mode = ModeInvalid
		return setting, fmt.Errorf("parse proxy URL failed")
	}
	if parsedURL.Scheme == "" || parsedURL.Host == "" {
		setting.Mode = ModeInvalid
		return setting, fmt.Errorf("proxy URL missing scheme/host")
	}

	switch parsedURL.Scheme {
	case "socks5", "socks5h", "http", "https":
		setting.Mode = ModeProxy
		setting.URL = parsedURL
		return setting, nil
	default:
		setting.Mode = ModeInvalid
		return setting, fmt.Errorf("unsupported proxy scheme: %s", parsedURL.Scheme)
	}
}

// ErrProxyEgressBlocked is returned by the fail-closed dialer / round tripper
// installed when an account-scoped proxy setting is missing or invalid. It is
// surfaced verbatim so the egress-blocked reason is unambiguous in logs and
// error responses, and it guarantees no network I/O happened before the failure
// (anti-correlation: never fall through to a direct dial that would expose the
// real server IP under the account identity).
var ErrProxyEgressBlocked = errors.New("proxy egress blocked: refusing direct connection to prevent IP exposure")

// blockingDialer is a proxy.Dialer + proxy.ContextDialer that performs zero
// network I/O and always fails with ErrProxyEgressBlocked. It is installed at
// the connection layer for account transports whose proxy setting resolved to
// invalid, so the real client IP can never leak via an accidental direct dial.
type blockingDialer struct{}

// Dial always fails without opening a connection.
func (blockingDialer) Dial(string, string) (net.Conn, error) {
	return nil, ErrProxyEgressBlocked
}

// DialContext always fails without opening a connection. Implementing it makes
// blockingDialer satisfy proxy.ContextDialer as well, so context-aware callers
// (e.g. the uTLS dialContext) also fail closed instead of falling back.
func (blockingDialer) DialContext(context.Context, string, string) (net.Conn, error) {
	return nil, ErrProxyEgressBlocked
}

// BlockingDialer returns a proxy.Dialer (also a proxy.ContextDialer) that fails
// every dial with ErrProxyEgressBlocked without performing any network I/O.
func BlockingDialer() proxy.Dialer {
	return blockingDialer{}
}

// blockingRoundTripper is an http.RoundTripper that always fails with
// ErrProxyEgressBlocked before any dial, for account HTTP clients whose proxy
// setting resolved to invalid. It deliberately has no fallback path.
type blockingRoundTripper struct{}

// RoundTrip always fails without opening a connection.
func (blockingRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, ErrProxyEgressBlocked
}

// BlockingRoundTripper returns an http.RoundTripper that fails every request
// with ErrProxyEgressBlocked without performing any network I/O.
func BlockingRoundTripper() http.RoundTripper {
	return blockingRoundTripper{}
}

func cloneDefaultTransport() *http.Transport {
	if transport, ok := http.DefaultTransport.(*http.Transport); ok && transport != nil {
		return transport.Clone()
	}
	return &http.Transport{}
}

// NewDirectTransport returns a transport that bypasses environment proxies.
func NewDirectTransport() *http.Transport {
	clone := cloneDefaultTransport()
	clone.Proxy = nil
	return clone
}

// BuildHTTPTransport constructs an HTTP transport for the provided proxy setting.
func BuildHTTPTransport(raw string) (*http.Transport, Mode, error) {
	setting, errParse := Parse(raw)
	if errParse != nil {
		return nil, setting.Mode, errParse
	}

	switch setting.Mode {
	case ModeInherit:
		return nil, setting.Mode, nil
	case ModeDirect:
		transport := NewDirectTransport()
		applyTestRootCA(transport)
		return transport, setting.Mode, nil
	case ModeProxy:
		if setting.URL.Scheme == "socks5" || setting.URL.Scheme == "socks5h" {
			var proxyAuth *proxy.Auth
			if setting.URL.User != nil {
				username := setting.URL.User.Username()
				password, _ := setting.URL.User.Password()
				proxyAuth = &proxy.Auth{User: username, Password: password}
			}
			dialer, errSOCKS5 := proxy.SOCKS5("tcp", setting.URL.Host, proxyAuth, proxy.Direct)
			if errSOCKS5 != nil {
				return nil, setting.Mode, fmt.Errorf("create SOCKS5 dialer failed: %w", errSOCKS5)
			}
			transport := cloneDefaultTransport()
			transport.Proxy = nil
			transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				type dialResult struct {
					conn net.Conn
					err  error
				}
				resultCh := make(chan dialResult, 1)
				go func() {
					conn, err := dialer.Dial(network, addr)
					resultCh <- dialResult{conn: conn, err: err}
				}()
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case result := <-resultCh:
					return result.conn, result.err
				}
			}
			applyTestRootCA(transport)
			return transport, setting.Mode, nil
		}
		transport := cloneDefaultTransport()
		transport.Proxy = http.ProxyURL(setting.URL)
		applyTestRootCA(transport)
		return transport, setting.Mode, nil
	default:
		return nil, setting.Mode, nil
	}
}

func applyTestRootCA(transport *http.Transport) {
	if transport == nil {
		return
	}
	caFile := strings.TrimSpace(os.Getenv("QUOTIO_TEST_CA_FILE"))
	if caFile == "" {
		return
	}
	pemData, err := os.ReadFile(caFile)
	if err != nil {
		return
	}
	rootCAs, _ := x509.SystemCertPool()
	if rootCAs == nil {
		rootCAs = x509.NewCertPool()
	}
	if ok := rootCAs.AppendCertsFromPEM(pemData); !ok {
		return
	}
	if transport.TLSClientConfig == nil {
		transport.TLSClientConfig = &tls.Config{}
	}
	transport.TLSClientConfig.RootCAs = rootCAs
}

// BuildDialer constructs a proxy dialer for settings that operate at the connection layer.
func BuildDialer(raw string) (proxy.Dialer, Mode, error) {
	setting, errParse := Parse(raw)
	if errParse != nil {
		return nil, setting.Mode, errParse
	}

	switch setting.Mode {
	case ModeInherit:
		return nil, setting.Mode, nil
	case ModeDirect:
		return proxy.Direct, setting.Mode, nil
	case ModeProxy:
		if setting.URL.Scheme == "http" || setting.URL.Scheme == "https" {
			return &httpConnectDialer{proxyURL: setting.URL, dialer: proxy.Direct}, setting.Mode, nil
		}
		dialer, errDialer := proxy.FromURL(setting.URL, proxy.Direct)
		if errDialer != nil {
			return nil, setting.Mode, fmt.Errorf("create proxy dialer failed: %w", errDialer)
		}
		return dialer, setting.Mode, nil
	default:
		return nil, setting.Mode, nil
	}
}

type httpConnectDialer struct {
	proxyURL *url.URL
	dialer   proxy.Dialer
}

func (d *httpConnectDialer) Dial(network, addr string) (net.Conn, error) {
	return d.DialContext(context.Background(), network, addr)
}

func (d *httpConnectDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	contextDialer, ok := d.dialer.(proxy.ContextDialer)
	if !ok {
		return nil, errors.New("HTTP proxy base dialer does not support context cancellation")
	}
	proxyConn, errDial := contextDialer.DialContext(ctx, network, proxyDialAddr(d.proxyURL))
	if errDial != nil {
		return nil, fmt.Errorf("dial HTTP proxy failed: %w", errDial)
	}

	conn := proxyConn
	cancelDone := make(chan struct{})
	stopCancel := context.AfterFunc(ctx, func() {
		_ = proxyConn.Close()
		close(cancelDone)
	})
	defer func() {
		if !stopCancel() {
			<-cancelDone
		}
	}()
	if d.proxyURL.Scheme == "https" {
		tlsConn := tls.Client(conn, &tls.Config{ServerName: d.proxyURL.Hostname()})
		if errHandshake := tlsConn.HandshakeContext(ctx); errHandshake != nil {
			if errClose := conn.Close(); errClose != nil {
				return nil, fmt.Errorf("HTTPS proxy TLS handshake failed: %w; close failed: %v", errHandshake, errClose)
			}
			return nil, fmt.Errorf("HTTPS proxy TLS handshake failed: %w", errHandshake)
		}
		conn = tlsConn
	}

	req := (&http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Host: addr},
		Host:   addr,
		Header: make(http.Header),
	}).WithContext(ctx)
	if d.proxyURL.User != nil {
		req.Header.Set("Proxy-Authorization", proxyAuthorization(d.proxyURL.User))
	}
	if errWrite := req.Write(conn); errWrite != nil {
		if errClose := conn.Close(); errClose != nil {
			return nil, fmt.Errorf("write CONNECT request failed: %w; close failed: %v", errWrite, errClose)
		}
		return nil, fmt.Errorf("write CONNECT request failed: %w", errWrite)
	}

	reader := bufio.NewReader(conn)
	resp, errRead := http.ReadResponse(reader, req)
	if errRead != nil {
		if errClose := conn.Close(); errClose != nil {
			return nil, fmt.Errorf("read CONNECT response failed: %w; close failed: %v", errRead, errClose)
		}
		return nil, fmt.Errorf("read CONNECT response failed: %w", errRead)
	}
	if resp.StatusCode != http.StatusOK {
		if resp.Body != nil {
			_ = resp.Body.Close()
		}
		if errClose := conn.Close(); errClose != nil {
			return nil, fmt.Errorf("proxy CONNECT returned status %s; close failed: %v", resp.Status, errClose)
		}
		return nil, fmt.Errorf("proxy CONNECT returned status %s", resp.Status)
	}

	if errContext := ctx.Err(); errContext != nil {
		if errClose := conn.Close(); errClose != nil && !errors.Is(errClose, net.ErrClosed) {
			return nil, fmt.Errorf("HTTP proxy context ended: %w; close failed: %v", errContext, errClose)
		}
		return nil, errContext
	}
	if reader.Buffered() > 0 {
		return &bufferedConn{Conn: conn, reader: reader}, nil
	}
	return conn, nil
}

func proxyDialAddr(proxyURL *url.URL) string {
	port := proxyURL.Port()
	if port == "" {
		port = "80"
		if proxyURL.Scheme == "https" {
			port = "443"
		}
	}
	return net.JoinHostPort(proxyURL.Hostname(), port)
}

func proxyAuthorization(user *url.Userinfo) string {
	username := user.Username()
	password, _ := user.Password()
	encoded := base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
	return "Basic " + encoded
}

// Redact returns a log-safe proxy URL with credentials and path-like data removed.
func Redact(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}

	parsedURL, errParse := url.Parse(trimmed)
	if errParse != nil || parsedURL.Scheme == "" || parsedURL.Host == "" {
		return "<invalid proxy URL>"
	}

	redacted := &url.URL{
		Scheme: parsedURL.Scheme,
		Host:   parsedURL.Host,
	}
	if parsedURL.User != nil {
		redacted.User = url.User("redacted")
	}
	return redacted.String()
}

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) {
	if c.reader.Buffered() > 0 {
		return c.reader.Read(p)
	}
	return c.Conn.Read(p)
}
