package executor

import (
	"net/http"
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
