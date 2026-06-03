package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

type claudeRetryRoundTripper func(*http.Request) (*http.Response, error)

func (f claudeRetryRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func disableClaudeTransportRetryBackoff(t *testing.T) {
	t.Helper()
	old := claudeProxyTransportRetryBackoffs
	claudeProxyTransportRetryBackoffs = []time.Duration{0, 0}
	t.Cleanup(func() { claudeProxyTransportRetryBackoffs = old })
}

func TestClaudeExecutor_Execute_RetriesTransientProxyTransportError(t *testing.T) {
	disableClaudeTransportRetryBackoff(t)

	var calls int
	rt := claudeRetryRoundTripper(func(req *http.Request) (*http.Response, error) {
		calls++
		body := mustReadClaudeRetryRequestBody(t, req)
		if !strings.Contains(body, "messages") {
			t.Fatalf("request body missing messages: %s", body)
		}
		if calls == 1 {
			return nil, fmt.Errorf("socks connect tcp 80.174.217.1:12324->api.anthropic.com:443: unknown error connection not allowed by ruleset")
		}
		return claudeRetryResponse(req, http.StatusOK, claudeRetryMessageBody()), nil
	})

	exec := NewClaudeExecutor(&config.Config{})
	_, err := exec.Execute(claudeRetryContext(rt), claudeRetryAuth(), claudeRetryRequest(), claudeRetryOptions())
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if calls != 2 {
		t.Fatalf("RoundTrip calls = %d, want 2", calls)
	}
}

func TestClaudeExecutor_Execute_DoesNotRetryNonProxyTransportError(t *testing.T) {
	disableClaudeTransportRetryBackoff(t)

	var calls int
	rt := claudeRetryRoundTripper(func(req *http.Request) (*http.Response, error) {
		calls++
		_ = mustReadClaudeRetryRequestBody(t, req)
		return nil, fmt.Errorf("tls: failed to verify certificate")
	})

	exec := NewClaudeExecutor(&config.Config{})
	_, err := exec.Execute(claudeRetryContext(rt), claudeRetryAuth(), claudeRetryRequest(), claudeRetryOptions())
	if err == nil {
		t.Fatal("Execute() error = nil, want transport error")
	}
	if calls != 1 {
		t.Fatalf("RoundTrip calls = %d, want 1", calls)
	}
}

func TestClaudeExecutor_Execute_DoesNotRetryHTTPAuthOrRateLimitStatus(t *testing.T) {
	disableClaudeTransportRetryBackoff(t)

	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests} {
		t.Run(fmt.Sprintf("status_%d", status), func(t *testing.T) {
			var calls int
			rt := claudeRetryRoundTripper(func(req *http.Request) (*http.Response, error) {
				calls++
				_ = mustReadClaudeRetryRequestBody(t, req)
				return claudeRetryResponse(req, status, `{"error":{"message":"no retry"}}`), nil
			})

			exec := NewClaudeExecutor(&config.Config{})
			_, err := exec.Execute(claudeRetryContext(rt), claudeRetryAuth(), claudeRetryRequest(), claudeRetryOptions())
			if err == nil {
				t.Fatalf("Execute() error = nil, want HTTP %d", status)
			}
			if calls != 1 {
				t.Fatalf("RoundTrip calls = %d, want 1", calls)
			}
		})
	}
}

func TestClaudeExecutor_ExecuteStream_RetriesTransientProxyTransportErrorBeforeFirstByte(t *testing.T) {
	disableClaudeTransportRetryBackoff(t)

	var calls int
	rt := claudeRetryRoundTripper(func(req *http.Request) (*http.Response, error) {
		calls++
		_ = mustReadClaudeRetryRequestBody(t, req)
		if calls == 1 {
			return nil, fmt.Errorf("proxyconnect tcp: dial tcp 127.0.0.1:12324: connect: connection refused")
		}
		return claudeRetryResponse(req, http.StatusOK, claudeRetryStreamBody()), nil
	})

	exec := NewClaudeExecutor(&config.Config{})
	result, err := exec.ExecuteStream(claudeRetryContext(rt), claudeRetryAuth(), claudeRetryRequest(), claudeRetryOptions())
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	for range result.Chunks {
	}
	if calls != 2 {
		t.Fatalf("RoundTrip calls = %d, want 2", calls)
	}
}

func TestClaudeExecutor_CountTokens_RetriesTransientProxyTransportError(t *testing.T) {
	disableClaudeTransportRetryBackoff(t)

	var calls int
	rt := claudeRetryRoundTripper(func(req *http.Request) (*http.Response, error) {
		calls++
		_ = mustReadClaudeRetryRequestBody(t, req)
		if calls == 1 {
			return nil, fmt.Errorf("read tcp 127.0.0.1:12324->93.184.216.34:443: read: connection reset by peer")
		}
		return claudeRetryResponse(req, http.StatusOK, `{"input_tokens":7}`), nil
	})

	exec := NewClaudeExecutor(&config.Config{})
	_, err := exec.CountTokens(claudeRetryContext(rt), claudeRetryAuth(), claudeRetryRequest(), claudeRetryOptions())
	if err != nil {
		t.Fatalf("CountTokens() error = %v", err)
	}
	if calls != 2 {
		t.Fatalf("RoundTrip calls = %d, want 2", calls)
	}
}

func claudeRetryContext(rt http.RoundTripper) context.Context {
	return context.WithValue(context.Background(), "cliproxy.roundtripper", rt)
}

func claudeRetryAuth() *cliproxyauth.Auth {
	return &cliproxyauth.Auth{
		Provider: "claude-test",
		Attributes: map[string]string{
			"api_key":  "test-api-key",
			"base_url": "http://claude.test",
		},
	}
}

func claudeRetryRequest() cliproxyexecutor.Request {
	return cliproxyexecutor.Request{
		Model:   "claude-3-5-sonnet-20241022",
		Payload: []byte(`{"model":"claude-3-5-sonnet-20241022","messages":[{"role":"user","content":"hi"}]}`),
	}
}

func claudeRetryOptions() cliproxyexecutor.Options {
	return cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")}
}

func claudeRetryResponse(req *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}
}

func mustReadClaudeRetryRequestBody(t *testing.T, req *http.Request) string {
	t.Helper()
	if req.Body == nil {
		return ""
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("ReadAll(request body) error = %v", err)
	}
	if err := req.Body.Close(); err != nil {
		t.Fatalf("request body close error = %v", err)
	}
	return string(body)
}

func claudeRetryMessageBody() string {
	return `{"id":"msg_1","type":"message","role":"assistant","model":"claude-3-5-sonnet-20241022","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`
}

func claudeRetryStreamBody() string {
	return "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-3-5-sonnet-20241022\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n" +
		"data: {\"type\":\"message_stop\"}\n\n"
}
