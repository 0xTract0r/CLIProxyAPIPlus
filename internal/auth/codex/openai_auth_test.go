package codex

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestRefreshTokensWithRetry_NonRetryableOnlyAttemptsOnce(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "refresh_token_reused code",
			body: `{"error":"invalid_grant","code":"refresh_token_reused"}`,
			want: "refresh_token_reused",
		},
		{
			name: "already used description",
			body: `{"error":"invalid_grant","error_description":"Refresh token has already been used"}`,
			want: "already been used",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls int32
			auth := &CodexAuth{
				httpClient: &http.Client{
					Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
						atomic.AddInt32(&calls, 1)
						return &http.Response{
							StatusCode: http.StatusBadRequest,
							Body:       io.NopCloser(strings.NewReader(tt.body)),
							Header:     make(http.Header),
							Request:    req,
						}, nil
					}),
				},
			}

			_, err := auth.RefreshTokensWithRetry(context.Background(), "dummy_refresh_token", 3)
			if err == nil {
				t.Fatalf("expected error for non-retryable refresh failure")
			}
			if !strings.Contains(strings.ToLower(err.Error()), tt.want) {
				t.Fatalf("expected %q in error, got: %v", tt.want, err)
			}
			if got := atomic.LoadInt32(&calls); got != 1 {
				t.Fatalf("expected 1 refresh attempt, got %d", got)
			}
		})
	}
}

func TestCodexBareClientAppliesServingUserAgentOnRefresh(t *testing.T) {
	var captured *http.Request
	auth := &CodexAuth{
		httpClient: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				captured = req.Clone(req.Context())
				return &http.Response{
					StatusCode: http.StatusBadRequest,
					Body:       io.NopCloser(strings.NewReader(`{"error":"invalid_grant"}`)),
					Header:     make(http.Header),
					Request:    req,
				}, nil
			}),
		},
		userAgent: codexOAuthUserAgent,
	}

	// The bare client used for background refresh must present the serving
	// User-Agent instead of an empty Go default. The request is sent before the
	// error response is parsed, so the captured request reflects the headers.
	_, _ = auth.RefreshTokensWithRetry(context.Background(), "refresh-token", 1)
	if captured == nil {
		t.Fatal("refresh request was not sent")
	}
	if got := captured.Header.Get("User-Agent"); got != codexOAuthUserAgent {
		t.Fatalf("User-Agent = %q, want %q", got, codexOAuthUserAgent)
	}
}

// TestNewCodexAuthWithProxyURL_UsesUtlsRefreshTransport asserts the bare codex
// OAuth refresh client no longer uses a Go-default *http.Transport (the
// anti-correlation leak 03117a8e signature) but the serving uTLS refresh round
// tripper (codex_rustls_native_v1). The deep profile/proxy/strict assertions live
// in the helps package test (oauth_refresh_utls_client_test.go).
func TestNewCodexAuthWithProxyURL_UsesUtlsRefreshTransport(t *testing.T) {
	cfg := &config.Config{SDKConfig: config.SDKConfig{ProxyURL: "socks5://proxy.example.com:1080"}}
	auth := NewCodexAuthWithProxyURL(cfg, "")

	if auth.httpClient == nil || auth.httpClient.Transport == nil {
		t.Fatal("expected non-nil refresh http client and transport")
	}
	if _, isStdlib := auth.httpClient.Transport.(*http.Transport); isStdlib {
		t.Fatal("codex refresh transport is *http.Transport (Go-default TLS); want serving uTLS refresh transport to avoid anti-correlation leak")
	}
}

// TestNewCodexAuthWithProxyURL_DirectOverrideStillConstructs asserts the "direct"
// proxy override is accepted and yields a usable uTLS refresh client (resolved to
// ModeDirect by proxyutil inside the round tripper's dialer). Proxy-vs-direct
// dialer correctness is asserted in the helps test.
func TestNewCodexAuthWithProxyURL_DirectOverrideStillConstructs(t *testing.T) {
	cfg := &config.Config{SDKConfig: config.SDKConfig{ProxyURL: "socks5://proxy.example.com:1080"}}
	auth := NewCodexAuthWithProxyURL(cfg, "direct")

	if auth.httpClient == nil || auth.httpClient.Transport == nil {
		t.Fatal("expected non-nil refresh http client and transport for direct override")
	}
	if _, isStdlib := auth.httpClient.Transport.(*http.Transport); isStdlib {
		t.Fatal("codex refresh (direct override) transport is *http.Transport; want serving uTLS refresh transport")
	}
}

func TestNewCodexAuthWithHTTPClientUsesCallerClientForTokenExchange(t *testing.T) {
	var captured *http.Request
	client := &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			clone := req.Clone(req.Context())
			clone.Header = req.Header.Clone()
			if clone.Header.Get("User-Agent") == "" {
				clone.Header.Set("User-Agent", "managed-codex/26.318.11754")
			}
			if clone.Header.Get("Version") == "" {
				clone.Header.Set("Version", "26.318.11754")
			}
			if clone.Header.Get("Accept-Encoding") == "" {
				clone.Header.Set("Accept-Encoding", "gzip, deflate, br, zstd")
			}
			if clone.Header.Get("Content-Type") == "" {
				clone.Header.Set("Content-Type", "should-not-be-used")
			}
			captured = clone
			return &http.Response{
				StatusCode: http.StatusOK,
				Body: io.NopCloser(bytes.NewReader(gzipTestPayload(t, `{
					"access_token":"access-token",
					"refresh_token":"refresh-token",
					"id_token":"not-a-jwt",
					"token_type":"Bearer",
					"expires_in":3600
				}`))),
				Header: http.Header{
					"Content-Encoding": []string{"gzip"},
					"Content-Type":     []string{"application/json"},
				},
				Request: req,
			}, nil
		}),
	}
	auth := NewCodexAuthWithHTTPClient(client)

	bundle, err := auth.ExchangeCodeForTokens(context.Background(), "oauth-code", &PKCECodes{
		CodeVerifier:  "verifier",
		CodeChallenge: "challenge",
	})
	if err != nil {
		t.Fatalf("exchange code: %v", err)
	}
	if bundle == nil || bundle.TokenData.AccessToken != "access-token" || bundle.TokenData.RefreshToken != "refresh-token" {
		t.Fatalf("unexpected token bundle: %#v", bundle)
	}
	if captured == nil {
		t.Fatal("caller HTTP client was not used")
	}
	if captured.URL.String() != TokenURL {
		t.Fatalf("token URL = %q", captured.URL.String())
	}
	if captured.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
		t.Fatalf("Content-Type = %q", captured.Header.Get("Content-Type"))
	}
	if captured.Header.Get("Accept") != "application/json" {
		t.Fatalf("Accept = %q", captured.Header.Get("Accept"))
	}
	if captured.Header.Get("User-Agent") != "managed-codex/26.318.11754" {
		t.Fatalf("User-Agent = %q", captured.Header.Get("User-Agent"))
	}
	if captured.Header.Get("Version") != "26.318.11754" {
		t.Fatalf("Version = %q", captured.Header.Get("Version"))
	}
	if captured.Header.Get("Accept-Encoding") != "gzip, deflate, br, zstd" {
		t.Fatalf("Accept-Encoding = %q", captured.Header.Get("Accept-Encoding"))
	}
}

func TestExchangeCodeForTokensNonJSONResponseIncludesDiagnostics(t *testing.T) {
	auth := NewCodexAuthWithHTTPClient(&http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader("\x1b[31mproxy returned a terminal banner")),
				Header: http.Header{
					"Content-Type": []string{"text/plain; charset=utf-8"},
				},
				Request: req,
			}, nil
		}),
	})

	_, err := auth.ExchangeCodeForTokens(context.Background(), "oauth-code", &PKCECodes{
		CodeVerifier:  "verifier",
		CodeChallenge: "challenge",
	})
	if err == nil {
		t.Fatal("expected parse error")
	}
	msg := err.Error()
	for _, want := range []string{
		"failed to parse token response",
		"status=200",
		"content_type=text/plain; charset=utf-8",
		"content_encoding=<empty>",
		`body_preview="\x1b[31mproxy returned a terminal banner"`,
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error %q does not contain %q", msg, want)
		}
	}
}

func gzipTestPayload(t *testing.T, raw string) []byte {
	t.Helper()
	var buf bytes.Buffer
	writer := gzip.NewWriter(&buf)
	if _, err := writer.Write([]byte(raw)); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}
