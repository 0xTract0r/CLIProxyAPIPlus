package executor

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/cache"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/sirupsen/logrus/hooks/test"
	"github.com/tidwall/gjson"
)

func testGeminiSignaturePayload() string {
	payload := append([]byte{0x0A}, bytes.Repeat([]byte{0x56}, 48)...)
	return base64.StdEncoding.EncodeToString(payload)
}

// testFakeClaudeSignature returns a base64 string starting with 'E' that passes
// the lightweight hasValidClaudeSignature check but has invalid protobuf content
// (first decoded byte 0x12 is correct, but no valid protobuf field 2 follows),
// so it fails deep validation in strict mode.
func testFakeClaudeSignature() string {
	return base64.StdEncoding.EncodeToString([]byte{0x12, 0xFF, 0xFE, 0xFD})
}

func testAntigravityAuth(baseURL string) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{ProxyURL: "direct",
		Attributes: map[string]string{
			"base_url": baseURL,
		},
		Metadata: map[string]any{
			"access_token": "token-123",
			"expired":      time.Now().Add(24 * time.Hour).Format(time.RFC3339),
		},
	}
}

func invalidClaudeThinkingPayload() []byte {
	return []byte(`{
		"model": "claude-sonnet-4-5-thinking",
		"messages": [
			{
				"role": "assistant",
				"content": [
					{"type": "thinking", "thinking": "bad", "signature": "` + testFakeClaudeSignature() + `"},
					{"type": "text", "text": "hello"}
				]
			}
		]
	}`)
}

func newSignatureDebugHook(t *testing.T) *test.Hook {
	t.Helper()

	previousLevel := log.GetLevel()
	log.SetLevel(log.DebugLevel)
	hook := test.NewLocal(log.StandardLogger())
	t.Cleanup(func() {
		hook.Reset()
		log.SetLevel(previousLevel)
	})
	return hook
}

func assertSignatureDebugDoesNotLeak(t *testing.T, hook *test.Hook, forbidden string) {
	t.Helper()

	if forbidden == "" {
		return
	}
	for _, entry := range hook.AllEntries() {
		if strings.Contains(entry.Message, forbidden) {
			t.Fatalf("debug log leaked signature in message: %q", entry.Message)
		}
		for key, value := range entry.Data {
			if strings.Contains(fmt.Sprint(value), forbidden) {
				t.Fatalf("debug log leaked signature in field %q: %v", key, value)
			}
		}
	}
}

// Fork increment: in strict bypass mode the Antigravity executor rejects invalid
// Claude signatures with HTTP 400 instead of silently stripping them, so this test
// asserts the request is rejected before any upstream call is made.
func TestAntigravityExecutor_StrictBypassRejectsInvalidSignature(t *testing.T) {
	previousCache := cache.SignatureCacheEnabled()
	previousStrict := cache.SignatureBypassStrictMode()
	cache.SetSignatureCacheEnabled(false)
	cache.SetSignatureBypassStrictMode(true)
	t.Cleanup(func() {
		cache.SetSignatureCacheEnabled(previousCache)
		cache.SetSignatureBypassStrictMode(previousStrict)
	})

	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"response":{"candidates":[{"content":{"parts":[{"text":"ok"}]}}]}}`))
	}))
	defer server.Close()

	executor := NewAntigravityExecutor(nil)
	auth := testAntigravityAuth(server.URL)
	payload := invalidClaudeThinkingPayload()
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude"), OriginalRequest: payload}
	req := cliproxyexecutor.Request{Model: "claude-sonnet-4-5-thinking", Payload: payload}

	tests := []struct {
		name   string
		invoke func() error
	}{
		{
			name: "execute",
			invoke: func() error {
				_, err := executor.Execute(context.Background(), auth, req, opts)
				return err
			},
		},
		{
			name: "stream",
			invoke: func() error {
				_, err := executor.ExecuteStream(context.Background(), auth, req, cliproxyexecutor.Options{SourceFormat: opts.SourceFormat, OriginalRequest: payload, Stream: true})
				return err
			},
		},
		{
			name: "count tokens",
			invoke: func() error {
				_, err := executor.CountTokens(context.Background(), auth, req, opts)
				return err
			},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			err := tt.invoke()
			if err == nil {
				t.Fatal("expected invalid signature to return an error")
			}
			statusProvider, ok := err.(interface{ StatusCode() int })
			if !ok {
				t.Fatalf("expected status error, got %T: %v", err, err)
			}
			if statusProvider.StatusCode() != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d", statusProvider.StatusCode(), http.StatusBadRequest)
			}
		})
	}

	if got := hits.Load(); got != 0 {
		t.Fatalf("expected invalid signature to be rejected before upstream request, got %d upstream hits", got)
	}
}

// Upstream increment: the Claude executor logs (and does not leak) signatures it
// sanitizes before forwarding to the Claude upstream. This path is independent of
// the Antigravity strict-bypass rejection above.
func TestClaudeExecutor_LogsSanitizedClaudeUpstreamSignatures(t *testing.T) {
	hook := newSignatureDebugHook(t)
	rawSignature := "skip_thought_signature_validator"
	body := []byte(`{
		"model": "claude-sonnet-4-5",
		"messages": [
			{
				"role": "assistant",
				"content": [
					{"type": "thinking", "thinking": "bad", "signature": "` + rawSignature + `"},
					{"type": "text", "text": "hello"},
					{"type": "tool_use", "id": "call_123", "name": "get_weather", "input": {}, "signature": "` + rawSignature + `"}
				]
			}
		]
	}`)

	output := sanitizeClaudeMessagesForClaudeUpstreamWithDebug(context.Background(), body, "claude-sonnet-4-5")
	parts := gjson.GetBytes(output, "messages.0.content").Array()
	if len(parts) != 2 {
		t.Fatalf("content length = %d, want 2 after invalid thinking strip: %s", len(parts), output)
	}
	if parts[1].Get("signature").Exists() {
		t.Fatalf("tool_use signature should be removed before Claude upstream: %s", output)
	}

	found := false
	for _, entry := range hook.AllEntries() {
		if entry.Level != log.DebugLevel {
			continue
		}
		if entry.Data["component"] != "signature_sanitizer" ||
			entry.Data["executor"] != "claude" ||
			entry.Data["action"] != "sanitize_claude_messages" {
			continue
		}
		if entry.Data["dropped_blocks"] != 1 {
			t.Fatalf("dropped_blocks = %v, want 1", entry.Data["dropped_blocks"])
		}
		if entry.Data["dropped_signatures"] != 1 {
			t.Fatalf("dropped_signatures = %v, want 1", entry.Data["dropped_signatures"])
		}
		found = true
	}
	if !found {
		t.Fatal("expected debug log for Claude upstream signature sanitization")
	}
	assertSignatureDebugDoesNotLeak(t, hook, rawSignature)
}

func TestAntigravityExecutor_NonStrictBypassSkipsPrecheck(t *testing.T) {
	previousCache := cache.SignatureCacheEnabled()
	previousStrict := cache.SignatureBypassStrictMode()
	cache.SetSignatureCacheEnabled(false)
	cache.SetSignatureBypassStrictMode(false)
	t.Cleanup(func() {
		cache.SetSignatureCacheEnabled(previousCache)
		cache.SetSignatureBypassStrictMode(previousStrict)
	})

	payload := invalidClaudeThinkingPayload()
	from := sdktranslator.FromString("claude")

	_, err := validateAntigravityRequestSignatures(from, payload)
	if err != nil {
		t.Fatalf("non-strict bypass should skip precheck, got: %v", err)
	}
}

func TestAntigravityExecutor_CacheModeSkipsPrecheck(t *testing.T) {
	previous := cache.SignatureCacheEnabled()
	cache.SetSignatureCacheEnabled(true)
	t.Cleanup(func() {
		cache.SetSignatureCacheEnabled(previous)
	})

	payload := invalidClaudeThinkingPayload()
	from := sdktranslator.FromString("claude")

	_, err := validateAntigravityRequestSignatures(from, payload)
	if err != nil {
		t.Fatalf("cache mode should skip precheck, got: %v", err)
	}
}
