package claude

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type claudeRoundTripFunc func(*http.Request) (*http.Response, error)

func (f claudeRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestExchangeCodeForTokensDecodesManagedHeaderCompressedResponse(t *testing.T) {
	var captured *http.Request
	auth := NewClaudeAuthWithHTTPClient(&http.Client{
		Transport: claudeRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			clone := req.Clone(req.Context())
			clone.Header = req.Header.Clone()
			if clone.Header.Get("User-Agent") == "" {
				clone.Header.Set("User-Agent", "claude-cli/2.0.0")
			}
			if clone.Header.Get("Accept-Encoding") == "" {
				clone.Header.Set("Accept-Encoding", "gzip, deflate, br, zstd")
			}
			captured = clone
			return &http.Response{
				StatusCode: http.StatusOK,
				Body: io.NopCloser(bytes.NewReader(gzipClaudeTestPayload(t, `{
					"access_token":"access-token",
					"refresh_token":"refresh-token",
					"token_type":"Bearer",
					"expires_in":3600,
					"account":{"email_address":"claude@example.com"}
				}`))),
				Header: http.Header{
					"Content-Encoding": []string{"gzip"},
					"Content-Type":     []string{"application/json"},
				},
				Request: req,
			}, nil
		}),
	})

	bundle, err := auth.ExchangeCodeForTokens(context.Background(), "oauth-code", "oauth-state", &PKCECodes{
		CodeVerifier:  "verifier",
		CodeChallenge: "challenge",
	})
	if err != nil {
		t.Fatalf("exchange code: %v", err)
	}
	if bundle == nil || bundle.TokenData.AccessToken != "access-token" || bundle.TokenData.RefreshToken != "refresh-token" {
		t.Fatalf("unexpected token bundle: %#v", bundle)
	}
	if bundle.TokenData.Email != "claude@example.com" {
		t.Fatalf("email = %q", bundle.TokenData.Email)
	}
	if captured == nil {
		t.Fatal("HTTP client was not used")
	}
	if captured.Header.Get("Accept-Encoding") != "gzip, deflate, br, zstd" {
		t.Fatalf("Accept-Encoding = %q", captured.Header.Get("Accept-Encoding"))
	}
}

func TestRefreshTokensDecodesManagedHeaderCompressedResponse(t *testing.T) {
	auth := NewClaudeAuthWithHTTPClient(&http.Client{
		Transport: claudeRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Header.Get("Accept-Encoding") == "" {
				req.Header.Set("Accept-Encoding", "gzip, deflate, br, zstd")
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Body: io.NopCloser(bytes.NewReader(gzipClaudeTestPayload(t, `{
					"access_token":"new-access-token",
					"refresh_token":"new-refresh-token",
					"token_type":"Bearer",
					"expires_in":3600,
					"account":{"email_address":"claude-refresh@example.com"}
				}`))),
				Header: http.Header{
					"Content-Encoding": []string{"gzip"},
					"Content-Type":     []string{"application/json"},
				},
				Request: req,
			}, nil
		}),
	})

	tokenData, err := auth.RefreshTokens(context.Background(), "refresh-token")
	if err != nil {
		t.Fatalf("refresh tokens: %v", err)
	}
	if tokenData == nil || tokenData.AccessToken != "new-access-token" || tokenData.RefreshToken != "new-refresh-token" {
		t.Fatalf("unexpected token data: %#v", tokenData)
	}
	if tokenData.Email != "claude-refresh@example.com" {
		t.Fatalf("email = %q", tokenData.Email)
	}
}

func TestExchangeCodeForTokensNonJSONResponseIncludesDiagnostics(t *testing.T) {
	auth := NewClaudeAuthWithHTTPClient(&http.Client{
		Transport: claudeRoundTripFunc(func(req *http.Request) (*http.Response, error) {
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

	_, err := auth.ExchangeCodeForTokens(context.Background(), "oauth-code", "oauth-state", &PKCECodes{
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

func gzipClaudeTestPayload(t *testing.T, raw string) []byte {
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

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestRefreshTokensWithRetry_429BlocksImmediateReplay(t *testing.T) {
	resetClaudeRefreshState()
	defer resetClaudeRefreshState()

	var calls int32
	auth := &ClaudeAuth{
		httpClient: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				atomic.AddInt32(&calls, 1)
				return &http.Response{
					StatusCode: http.StatusTooManyRequests,
					Body:       io.NopCloser(strings.NewReader(`{"error":"rate_limited"}`)),
					Header:     http.Header{"Retry-After": []string{"60"}},
					Request:    req,
				}, nil
			}),
		},
	}

	_, err := auth.RefreshTokensWithRetry(context.Background(), "dummy_refresh_token", 3)
	if err == nil {
		t.Fatalf("expected 429 refresh error")
	}
	if !strings.Contains(err.Error(), "status 429") {
		t.Fatalf("expected status 429 in error, got %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected 1 refresh attempt after 429, got %d", got)
	}

	_, err = auth.RefreshTokensWithRetry(context.Background(), "dummy_refresh_token", 3)
	if err == nil {
		t.Fatalf("expected immediate blocked refresh error")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected blocked retry to avoid a second refresh call, got %d attempts", got)
	}
	if blockedUntil := claudeRefreshBlockedUntil("dummy_refresh_token"); !blockedUntil.After(time.Now()) {
		t.Fatalf("expected blocked-until timestamp to be set, got %v", blockedUntil)
	}
}

func TestRefreshTokens_DeduplicatesConcurrentRefresh(t *testing.T) {
	resetClaudeRefreshState()
	defer resetClaudeRefreshState()

	var calls int32
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once

	auth := &ClaudeAuth{
		httpClient: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				atomic.AddInt32(&calls, 1)
				once.Do(func() { close(started) })
				<-release
				return &http.Response{
					StatusCode: http.StatusOK,
					Body: io.NopCloser(strings.NewReader(`{
						"access_token":"new-access",
						"refresh_token":"new-refresh",
						"token_type":"Bearer",
						"expires_in":3600,
						"account":{"email_address":"shared@example.com"}
					}`)),
					Header:  make(http.Header),
					Request: req,
				}, nil
			}),
		},
	}

	results := make(chan *ClaudeTokenData, 2)
	errs := make(chan error, 2)
	runRefresh := func() {
		td, err := auth.RefreshTokens(context.Background(), "shared-refresh-token")
		results <- td
		errs <- err
	}

	go runRefresh()
	go runRefresh()

	<-started
	time.Sleep(20 * time.Millisecond)
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected concurrent refresh to share a single upstream call, got %d", got)
	}
	close(release)

	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("expected refresh to succeed, got %v", err)
		}
		td := <-results
		if td == nil || td.AccessToken != "new-access" {
			t.Fatalf("expected refreshed access token, got %#v", td)
		}
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected exactly 1 upstream refresh call, got %d", got)
	}
}
