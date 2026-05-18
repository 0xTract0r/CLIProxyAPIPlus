package claude

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
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
