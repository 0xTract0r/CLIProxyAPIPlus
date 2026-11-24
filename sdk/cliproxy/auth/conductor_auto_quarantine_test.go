package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// revokedOAuthTokenError mirrors the real Anthropic response body observed for
// AC04/dasmannmerow (T3, telemetry-farm-ux-hardening): a 401 with
// error.type=="authentication_error" and a message stating the OAuth access
// token was revoked. It is representative of a terminal auth/permission
// failure, as opposed to a transient one (rate limit, overload, timeout).
func revokedOAuthTokenError() *Error {
	return &Error{
		HTTPStatus: http.StatusUnauthorized,
		Message:    `{"type":"error","error":{"type":"authentication_error","message":"OAuth access token has been revoked."}}`,
	}
}

func rateLimitError() *Error {
	return &Error{HTTPStatus: http.StatusTooManyRequests, Message: `{"type":"error","error":{"type":"rate_limit_error","message":"rate limited"}}`}
}

func overloadedError() *Error {
	return &Error{HTTPStatus: http.StatusServiceUnavailable, Message: "upstream overloaded"}
}

// TestIsAuthBlockedForModel_AutoQuarantinedSkipsLikeDisabled covers AC ④: a
// quarantined credential must be skipped by the selector exactly like an
// operator-disabled one (blockReasonDisabled), both for account-level and
// model-scoped lookups, and never surfaced as a cooldown that would be
// silently retried once its previous NextRetryAfter elapses.
func TestIsAuthBlockedForModel_AutoQuarantinedSkipsLikeDisabled(t *testing.T) {
	t.Parallel()

	now := time.Now()
	auth := &Auth{
		ID:              "quarantined",
		Status:          StatusQuarantined,
		AutoQuarantined: true,
	}

	blocked, reason, next := isAuthBlockedForModel(auth, "", now)
	if !blocked {
		t.Fatalf("blocked = false for AutoQuarantined auth (no model), want true")
	}
	if reason != blockReasonDisabled {
		t.Fatalf("reason = %v, want blockReasonDisabled", reason)
	}
	if !next.IsZero() {
		t.Fatalf("next = %v, want zero (no cooldown-style retry)", next)
	}

	blocked, reason, _ = isAuthBlockedForModel(auth, "claude-sonnet", now)
	if !blocked {
		t.Fatalf("blocked = false for AutoQuarantined auth (model-scoped), want true")
	}
	if reason != blockReasonDisabled {
		t.Fatalf("reason = %v, want blockReasonDisabled", reason)
	}
}

// TestFillFirstSelectorPick_SkipsAutoQuarantinedAuth is an end-to-end check
// that the selector actually excludes a quarantined credential from being
// picked, rather than only asserting on the lower-level classifier.
func TestFillFirstSelectorPick_SkipsAutoQuarantinedAuth(t *testing.T) {
	t.Parallel()

	selector := &FillFirstSelector{}
	healthy := &Auth{ID: "healthy"}
	quarantined := &Auth{ID: "quarantined", AutoQuarantined: true, Status: StatusQuarantined}

	got, err := selector.Pick(context.Background(), "claude", "", cliproxyexecutor.Options{}, []*Auth{quarantined, healthy})
	if err != nil {
		t.Fatalf("Pick() error = %v", err)
	}
	if got == nil || got.ID != "healthy" {
		t.Fatalf("Pick() = %+v, want the non-quarantined auth", got)
	}

	_, err = selector.Pick(context.Background(), "claude", "", cliproxyexecutor.Options{}, []*Auth{quarantined})
	if err == nil {
		t.Fatalf("Pick() error = nil when only a quarantined auth is available, want an error")
	}
}

// TestIsTerminalAuthQuarantineResultError_Classification pins the boundary
// between terminal auth/permission failures and every transient/other class
// this feature must never touch.
func TestIsTerminalAuthQuarantineResultError_Classification(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  *Error
		want bool
	}{
		{"nil", nil, false},
		{"401 authentication_error", revokedOAuthTokenError(), true},
		{"401 plain unauthorized", &Error{HTTPStatus: http.StatusUnauthorized, Message: "unauthorized"}, true},
		{"429 rate limit", rateLimitError(), false},
		{"503 overloaded", overloadedError(), false},
		{"500 gateway", &Error{HTTPStatus: http.StatusInternalServerError, Message: "internal error"}, false},
		{"408 timeout", &Error{HTTPStatus: http.StatusRequestTimeout, Message: "timeout"}, false},
		{"402 payment_required", &Error{HTTPStatus: http.StatusPaymentRequired, Message: "payment required"}, false},
		{"403 forbidden", &Error{HTTPStatus: http.StatusForbidden, Message: "forbidden"}, false},
		{
			"401 cloudflare challenge",
			&Error{HTTPStatus: http.StatusUnauthorized, Message: "cf-mitigated: challenge-platform"},
			false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isTerminalAuthQuarantineResultError(tc.err); got != tc.want {
				t.Fatalf("isTerminalAuthQuarantineResultError(%+v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
