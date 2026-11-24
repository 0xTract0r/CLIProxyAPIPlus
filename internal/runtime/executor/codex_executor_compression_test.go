package executor

import (
	"bytes"
	"compress/gzip"

	"io"
	"net/http"
	"net/http/httptest"

	"strconv"

	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"

	log "github.com/sirupsen/logrus"
)

func codexCompressionConfig() *config.Config {
	return &config.Config{Codex: config.CodexConfig{UpstreamRequestCompression: true}}
}

func codexCompressionAuth() *cliproxyauth.Auth {
	return &cliproxyauth.Auth{Metadata: map[string]any{"access_token": "test-oauth"}}
}

func readCodexCompressionBody(t *testing.T, body io.ReadCloser) []byte {
	t.Helper()
	defer func() {
		if errClose := body.Close(); errClose != nil {
			log.Errorf("compression test: close body error: %v", errClose)
		}
	}()
	data, errRead := io.ReadAll(body)
	if errRead != nil {
		t.Fatalf("read body: %v", errRead)
	}
	return data
}

func codexCompressedResponse(t *testing.T, encoding string, plain []byte) []byte {
	t.Helper()
	var output bytes.Buffer
	var writer io.WriteCloser
	switch encoding {
	case "gzip":
		writer = gzip.NewWriter(&output)
	case "br":
		writer = brotli.NewWriter(&output)
	case "zstd":
		var errWriter error
		writer, errWriter = zstd.NewWriter(&output)
		if errWriter != nil {
			t.Fatal(errWriter)
		}
	}
	if _, errWrite := writer.Write(plain); errWrite != nil {
		t.Fatal(errWrite)
	}
	if errClose := writer.Close(); errClose != nil {
		t.Fatal(errClose)
	}
	return output.Bytes()
}

func TestCodexExecutor_CompressedUpstreamResponseDecoded(t *testing.T) {
	for _, mode := range []string{"execute", "stream", "compact"} {
		for _, encoding := range []string{"gzip", "zstd", "br"} {
			t.Run(mode+"/"+encoding, func(t *testing.T) {
				plain := []byte("data: " + codexCompletedEventBody + "\n\n")
				if mode == "compact" {
					plain = []byte(`{"id":"resp_1","object":"response","status":"completed","output":[]}`)
				}
				wire := codexCompressedResponse(t, encoding, plain)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					w.Header().Set("Content-Encoding", encoding)
					w.Header().Set("Content-Length", strconv.Itoa(len(wire)))
					if _, errWrite := w.Write(wire); errWrite != nil {
						t.Errorf("write compressed response: %v", errWrite)
					}
				}))
				defer server.Close()
				executor := NewCodexExecutor(&config.Config{})
				auth := codexTestAuth(server.URL)
				auth.ProxyURL = "direct"
				// Force negotiation even before cacheHelper supports it, keeping RED deterministic.
				auth.Attributes["header:Accept-Encoding"] = "gzip, deflate, br, zstd"
				req, opts := codexTestRequest()
				var headers http.Header
				if mode == "stream" {
					result, errStream := executor.ExecuteStream(t.Context(), auth, req, opts)
					if errStream != nil {
						t.Fatal(errStream)
					}
					headers = result.Headers
					var output []byte
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							t.Fatalf("compressed stream failed: %v", chunk.Err)
						}
						output = append(output, chunk.Payload...)
					}
					if !bytes.Contains(output, []byte("hello")) {
						t.Fatalf("decoded completion not found: %s", output)
					}
				} else {
					if mode == "compact" {
						opts.Alt = "responses/compact"
					}
					result, errExecute := executor.Execute(t.Context(), auth, req, opts)
					if errExecute != nil {
						t.Fatal(errExecute)
					}
					headers = result.Headers
					if !ojson.ValidBytes(result.Payload) || ojson.GetBytes(result.Payload, "id").String() != "resp_1" {
						t.Fatalf("invalid decoded payload: %s", result.Payload)
					}
				}
				if headers.Get("Content-Encoding") != "" || headers.Get("Content-Length") != "" {
					t.Fatalf("decoded response retained encoding/length: %v", headers)
				}
			})
		}
	}
}
