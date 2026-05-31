package executor

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestNewClaudeStatusErrDetectsUsageLimitRetryAfter(t *testing.T) {
	now := time.Date(2026, 5, 20, 16, 30, 0, 0, time.Local)
	body := []byte(`{"error":{"message":"Usage limit reached. Please try again at 5:03 PM."}}`)

	err := newClaudeStatusErr(http.StatusTooManyRequests, body, nil, now)

	if err.StatusCode() != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", err.StatusCode())
	}
	retryAfter := err.RetryAfter()
	if retryAfter == nil {
		t.Fatalf("expected retryAfter, got nil")
	}
	if *retryAfter != 33*time.Minute {
		t.Fatalf("retryAfter = %v, want 33m", *retryAfter)
	}
}

func TestNewClaudeStatusErrIgnoresTransient429(t *testing.T) {
	body := []byte(`{"error":{"message":"model is overloaded, please retry"}}`)

	err := newClaudeStatusErr(http.StatusTooManyRequests, body, nil, time.Now())

	if err.RetryAfter() != nil {
		t.Fatalf("expected nil retryAfter for transient 429, got %v", *err.RetryAfter())
	}
}

func TestNewClaudeStatusErrUsesRetryAfterHeaderForUsageLimit(t *testing.T) {
	headers := http.Header{"Retry-After": []string{"123"}}
	body := []byte(`{"error":{"message":"usage limit reached"}}`)

	err := newClaudeStatusErr(http.StatusTooManyRequests, body, headers, time.Now())

	retryAfter := err.RetryAfter()
	if retryAfter == nil {
		t.Fatalf("expected retryAfter, got nil")
	}
	if *retryAfter != 123*time.Second {
		t.Fatalf("retryAfter = %v, want 123s", *retryAfter)
	}
}

func TestClaudeUpstreamTransportErrorRedactsNetworkDetails(t *testing.T) {
	rawErr := errors.New(`Post "https://api.anthropic.com/v1/messages": read tcp 172.25.0.2:37824->80.174.217.1:12324: read: connection reset by peer; proxy=http://user:pass@80.174.217.1:12324`)

	err := claudeUpstreamTransportError(rawErr)
	if err == nil {
		t.Fatal("expected transport error, got nil")
	}
	statusErr, ok := err.(interface{ StatusCode() int })
	if !ok {
		t.Fatalf("expected status error, got %T", err)
	}
	if got := statusErr.StatusCode(); got != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", got, http.StatusBadGateway)
	}
	if got := err.Error(); got != claudeClientSafeTransportErrorMessage {
		t.Fatalf("message = %q, want %q", got, claudeClientSafeTransportErrorMessage)
	}

	for _, forbidden := range []string{
		"172.25.0.2",
		"80.174.217.1",
		"37824",
		"12324",
		"user",
		"pass",
		"read tcp",
		"->",
		"api.anthropic.com",
	} {
		if strings.Contains(err.Error(), forbidden) {
			t.Fatalf("client error leaked %q in %q", forbidden, err.Error())
		}
	}
}

func TestClaudeUpstreamTransportErrorNil(t *testing.T) {
	if err := claudeUpstreamTransportError(nil); err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
}
