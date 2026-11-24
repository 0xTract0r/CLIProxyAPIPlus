package codex

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"context"
	"errors"
	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestOrdinaryRefreshLegacyErrors(t *testing.T) {
	for _, message := range []string{"refresh-token-already-used", "refresh token was already used", "refresh-token-reuse", "invalid_grant"} {
		if !isNonRetryableRefreshErr(errors.New(message)) {
			t.Errorf("terminal error treated as retryable: %s", message)
		}
	}
	if isNonRetryableRefreshErr(errors.New("token refresh failed with status 429: rate limited")) {
		t.Fatal("standard 429 classification changed")
	}
}

type ordinaryOAuthRoundTripper func(*http.Request) (*http.Response, error)

func (f ordinaryOAuthRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestOrdinaryExchangeDiagnosticsOmitSecrets(t *testing.T) {
	const marker = "synthetic-secret-must-not-appear"
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"malformed", 200, "broken-" + marker, "status=200 invalid JSON token payload"},
		{"invalid grant", 400, `{"error":"invalid_grant","error_description":"` + marker + `"}`, "status 400: invalid_grant"},
		{"throttled", 429, `{"error":{"message":"` + marker + `"}}`, "status 429: unrecognized OAuth error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			gz := gzip.NewWriter(&buf)
			if _, err := io.WriteString(gz, tc.body); err != nil {
				t.Fatal(err)
			}
			if err := gz.Close(); err != nil {
				t.Fatal(err)
			}
			client := &http.Client{Transport: ordinaryOAuthRoundTripper(func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(bytes.NewReader(buf.Bytes())), Header: http.Header{"Content-Encoding": {"gzip"}}}, nil
			})}
			auth := NewCodexAuthWithHTTPClient(client)
			_, err := auth.ExchangeCodeForTokens(context.Background(), "synthetic-code", &PKCECodes{CodeVerifier: "fixture"})
			if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), marker) {
				t.Fatalf("diagnostic: %v", err)
			}
		})
	}
}

func TestOrdinaryTokenResponseContentEncodings(t *testing.T) {
	encode := func(name string, plain []byte) []byte {
		t.Helper()
		var out bytes.Buffer
		var writer io.WriteCloser
		switch name {
		case "gzip":
			writer = gzip.NewWriter(&out)
		case "deflate":
			writer = zlib.NewWriter(&out)
		case "br":
			writer = brotli.NewWriter(&out)
		case "zstd":
			w, err := zstd.NewWriter(&out)
			if err != nil {
				t.Fatal(err)
			}
			writer = w
		default:
			t.Fatalf("unknown fixture encoding: %s", name)
		}
		if _, err := writer.Write(plain); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		return out.Bytes()
	}
	plain := []byte(`{"fixture":"ordinary OAuth response"}`)
	read := func(enc string, encoded []byte) ([]byte, error) {
		return readTokenResponseBody(&http.Response{Header: http.Header{"Content-Encoding": {enc}},
			Body: io.NopCloser(bytes.NewReader(encoded))})
	}
	for _, name := range []string{"gzip", "deflate", "br", "zstd"} {
		t.Run(name, func(t *testing.T) {
			got, err := read(name, encode(name, plain))
			if err != nil || !bytes.Equal(got, plain) {
				t.Fatalf("decode %s: %q, %v", name, got, err)
			}
		})
	}
	t.Run("stacked", func(t *testing.T) {
		got, err := read("gzip, br", encode("br", encode("gzip", plain)))
		if err != nil || !bytes.Equal(got, plain) {
			t.Fatalf("stacked decoding: %q, %v", got, err)
		}
	})
	t.Run("limits", func(t *testing.T) {
		large := bytes.Repeat([]byte{'x'}, (4<<20)+1)
		for _, enc := range []string{"identity", "gzip"} {
			input := large
			if enc == "gzip" {
				input = encode(enc, input)
			}
			if _, err := read(enc, input); err == nil {
				t.Fatalf("accepted oversized %s response", enc)
			}
		}
	})
}
