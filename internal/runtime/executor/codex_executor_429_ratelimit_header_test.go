package executor

import (
	"net/http"

	"strconv"

	"testing"
	"time"

	_ "github.com/router-for-me/CLIProxyAPI/v8/internal/translator"
)

// Preserve explicit Retry-After on the current request without choosing a different grant.
func TestNewCodexStatusErr_RespectsPublicRetryAfter(t *testing.T) {
	transientBody := []byte(`{"error":{"type":"rate_limit","message":"TPM burst, retry shortly"}}`)
	rateLimitBody := []byte(`{"error":{"type":"rate_limit_reached","message":"You've hit your usage limit."}}`)

	exhausted := http.Header{}
	exhausted.Set("Retry-After", "8000")

	notExhausted := http.Header{}
	notExhausted.Set("X-Codex-Secondary-Used-Percent", "77")
	notExhausted.Set("X-Codex-Secondary-Reset-After-Seconds", "8000")

	tests := []struct {
		name       string
		status     int
		body       []byte
		headers    http.Header
		wantSet    bool
		wantApprox time.Duration
	}{
		{name: "standard Retry-After is preserved", status: http.StatusTooManyRequests, body: rateLimitBody, headers: exhausted, wantSet: true, wantApprox: 8000 * time.Second},
		{name: "429 without a valid Retry-After stays nil", status: http.StatusTooManyRequests, body: transientBody, headers: notExhausted, wantSet: false},
		{name: "transient 429 without headers stays nil", status: http.StatusTooManyRequests, body: transientBody, headers: nil, wantSet: false},
		{name: "503 preserves public Retry-After", status: http.StatusServiceUnavailable, body: rateLimitBody, headers: exhausted, wantSet: true, wantApprox: 8000 * time.Second},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := newCodexStatusErr(tc.status, tc.body, tc.headers)
			got := err.RetryAfter()
			if tc.wantSet {
				if got == nil {
					t.Fatalf("RetryAfter() = nil, want ~%s", tc.wantApprox)
				}
				if *got < tc.wantApprox-time.Minute || *got > tc.wantApprox+time.Minute {
					t.Fatalf("RetryAfter() = %s, want ~%s", *got, tc.wantApprox)
				}
			} else if got != nil {
				t.Fatalf("RetryAfter() = %s, want nil", *got)
			}
		})
	}
}

// TestCodexRateLimitRetryAfterFromHeaders covers the header parser directly,
// including the unix Reset-At form and the latest-window-wins behavior.
func TestCodexRateLimitRetryAfterFromHeaders(t *testing.T) {
	now := time.Unix(1_780_000_000, 0)

	t.Run("reset-after-seconds", func(t *testing.T) {
		h := http.Header{}
		h.Set("X-Codex-Primary-Used-Percent", "100")
		h.Set("X-Codex-Primary-Reset-After-Seconds", "3600")
		got := codexRateLimitRetryAfterFromHeaders(h, now)
		if got == nil || *got != time.Hour {
			t.Fatalf("got %v, want 1h", got)
		}
	})

	t.Run("reset-at unix and latest window wins", func(t *testing.T) {
		h := http.Header{}
		h.Set("X-Codex-Primary-Used-Percent", "100")
		h.Set("X-Codex-Primary-Reset-After-Seconds", "3600")
		h.Set("X-Codex-Secondary-Used-Percent", "100")
		h.Set("X-Codex-Secondary-Reset-At", strconv.FormatInt(now.Add(48*time.Hour).Unix(), 10))
		got := codexRateLimitRetryAfterFromHeaders(h, now)
		if got == nil || *got < 47*time.Hour || *got > 49*time.Hour {
			t.Fatalf("got %v, want ~48h (latest window)", got)
		}
	})

	t.Run("not exhausted returns nil", func(t *testing.T) {
		h := http.Header{}
		h.Set("X-Codex-Primary-Used-Percent", "99")
		h.Set("X-Codex-Primary-Reset-After-Seconds", "3600")
		if got := codexRateLimitRetryAfterFromHeaders(h, now); got != nil {
			t.Fatalf("got %v, want nil (window not exhausted)", got)
		}
	})

	t.Run("empty headers return nil", func(t *testing.T) {
		if got := codexRateLimitRetryAfterFromHeaders(nil, now); got != nil {
			t.Fatalf("got %v, want nil", got)
		}
	})
}
