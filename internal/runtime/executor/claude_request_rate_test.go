package executor

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestOrdinaryClaudeRateLimitKeepsOnlyValidRetryAfter(t *testing.T) {
	for _, tc := range []struct {
		name   string
		values []string
		want   time.Duration
	}{
		{"single numeric header", []string{"30"}, 30 * time.Second},
		{"ambiguous headers", []string{"30", "14400"}, 0},
		{"missing header", nil, 0},
		{"malformed header", []string{"soon"}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{
				"Retry-After": tc.values, "Anthropic-Ratelimit-Unified-Reset": {"9999999999"},
			}, Body: io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"rate_limit_error","message":"The requests per minute limit was exceeded."}}`))}
			err := ordinaryClaudeHTTPError(resp).(statusErr)
			if err.StatusCode() != http.StatusTooManyRequests {
				t.Fatalf("status=%d", err.StatusCode())
			}
			got := err.RetryAfter()
			if tc.want == 0 && got != nil || tc.want != 0 && (got == nil || *got != tc.want) {
				t.Fatalf("retry hint=%v want=%v", got, tc.want)
			}
		})
	}
}
