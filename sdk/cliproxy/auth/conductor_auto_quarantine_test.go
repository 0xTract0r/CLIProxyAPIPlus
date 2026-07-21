package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
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

// TestManagerMarkResult_TwoTerminalAuthFailuresAutoQuarantines covers AC ①:
// two consecutive terminal auth failures (HTTP 401 authentication_error) with
// zero successes in between must flip AutoQuarantined within the rolling
// window, going through the full Manager.MarkResult path (not just the
// isolated helper) so the wiring inside the existing per-status-code switch
// is exercised too.
func TestManagerMarkResult_TwoTerminalAuthFailuresAutoQuarantines(t *testing.T) {
	mgr := NewManager(nil, nil, nil)
	ctx := WithSkipPersist(context.Background())
	auth := &Auth{ID: "ac04", Provider: "claude"}
	if _, err := mgr.Register(ctx, auth); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	mgr.MarkResult(ctx, Result{AuthID: "ac04", Provider: "claude", Success: false, Error: revokedOAuthTokenError()})
	got, ok := mgr.GetByID("ac04")
	if !ok || got == nil {
		t.Fatalf("GetByID returned ok=%v auth=%v", ok, got)
	}
	if got.AutoQuarantined {
		t.Fatalf("AutoQuarantined = true after a single terminal auth failure, want false (threshold is %d)", authAutoQuarantineFailureThreshold)
	}

	mgr.MarkResult(ctx, Result{AuthID: "ac04", Provider: "claude", Success: false, Error: revokedOAuthTokenError()})
	got, ok = mgr.GetByID("ac04")
	if !ok || got == nil {
		t.Fatalf("GetByID returned ok=%v auth=%v", ok, got)
	}
	if !got.AutoQuarantined {
		t.Fatalf("AutoQuarantined = false after two terminal auth failures with zero successes, want true")
	}
	if got.Status != StatusQuarantined {
		t.Fatalf("Status = %q, want %q", got.Status, StatusQuarantined)
	}
	if got.QuarantineReason != quarantineReasonTerminalAuthFailure {
		t.Fatalf("QuarantineReason = %q, want %q", got.QuarantineReason, quarantineReasonTerminalAuthFailure)
	}
	if got.QuarantinedAt.IsZero() {
		t.Fatalf("QuarantinedAt is zero, want set")
	}
}

// TestManagerMarkResult_TransientFailuresNeverAutoQuarantine covers AC ②:
// repeated transient failures (429 rate limit, 5xx overload) must never flip
// AutoQuarantined, no matter how many times they occur.
func TestManagerMarkResult_TransientFailuresNeverAutoQuarantine(t *testing.T) {
	mgr := NewManager(nil, nil, nil)
	ctx := WithSkipPersist(context.Background())
	auth := &Auth{ID: "flaky", Provider: "claude"}
	if _, err := mgr.Register(ctx, auth); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	for i := 0; i < 5; i++ {
		mgr.MarkResult(ctx, Result{AuthID: "flaky", Provider: "claude", Success: false, Error: rateLimitError()})
		mgr.MarkResult(ctx, Result{AuthID: "flaky", Provider: "claude", Success: false, Error: overloadedError()})
	}

	got, ok := mgr.GetByID("flaky")
	if !ok || got == nil {
		t.Fatalf("GetByID returned ok=%v auth=%v", ok, got)
	}
	if got.AutoQuarantined {
		t.Fatalf("AutoQuarantined = true after only transient (429/5xx) failures, want false")
	}
	if got.Status == StatusQuarantined {
		t.Fatalf("Status = %q, want anything but %q", got.Status, StatusQuarantined)
	}
}

// TestManagerMarkResult_TransientFailureDoesNotWashOutTerminalAuthStreak
// documents a deliberate design decision: a transient failure interleaved
// between two terminal auth failures must not reset the in-progress
// terminal-auth streak (it is not a success), so the account is still
// quarantined once the second terminal auth failure lands.
func TestManagerMarkResult_TransientFailureDoesNotWashOutTerminalAuthStreak(t *testing.T) {
	mgr := NewManager(nil, nil, nil)
	ctx := WithSkipPersist(context.Background())
	auth := &Auth{ID: "interleaved", Provider: "claude"}
	if _, err := mgr.Register(ctx, auth); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	mgr.MarkResult(ctx, Result{AuthID: "interleaved", Provider: "claude", Success: false, Error: revokedOAuthTokenError()})
	mgr.MarkResult(ctx, Result{AuthID: "interleaved", Provider: "claude", Success: false, Error: rateLimitError()})
	mgr.MarkResult(ctx, Result{AuthID: "interleaved", Provider: "claude", Success: false, Error: revokedOAuthTokenError()})

	got, ok := mgr.GetByID("interleaved")
	if !ok || got == nil {
		t.Fatalf("GetByID returned ok=%v auth=%v", ok, got)
	}
	if !got.AutoQuarantined {
		t.Fatalf("AutoQuarantined = false after 401,429,401, want true (transient failure must not reset the terminal-auth streak)")
	}
}

// TestManagerMarkResult_RealSuccessClearsAutoQuarantine covers AC ③: once
// quarantined, a real successful request (e.g. right after the operator
// completes a fresh re-auth) must automatically lift the lock. The account
// must never be permanently blacklisted by this heuristic alone.
func TestManagerMarkResult_RealSuccessClearsAutoQuarantine(t *testing.T) {
	mgr := NewManager(nil, nil, nil)
	ctx := WithSkipPersist(context.Background())
	auth := &Auth{ID: "recovered", Provider: "claude"}
	if _, err := mgr.Register(ctx, auth); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	mgr.MarkResult(ctx, Result{AuthID: "recovered", Provider: "claude", Success: false, Error: revokedOAuthTokenError()})
	mgr.MarkResult(ctx, Result{AuthID: "recovered", Provider: "claude", Success: false, Error: revokedOAuthTokenError()})
	got, ok := mgr.GetByID("recovered")
	if !ok || got == nil || !got.AutoQuarantined {
		t.Fatalf("precondition failed: auth not quarantined before success, got=%+v ok=%v", got, ok)
	}

	mgr.MarkResult(ctx, Result{AuthID: "recovered", Provider: "claude", Success: true})

	got, ok = mgr.GetByID("recovered")
	if !ok || got == nil {
		t.Fatalf("GetByID returned ok=%v auth=%v", ok, got)
	}
	if got.AutoQuarantined {
		t.Fatalf("AutoQuarantined = true after a real successful request, want false")
	}
	if got.Status != StatusActive {
		t.Fatalf("Status = %q after recovery, want %q", got.Status, StatusActive)
	}
	if got.QuarantineReason != "" {
		t.Fatalf("QuarantineReason = %q after recovery, want empty", got.QuarantineReason)
	}
	if !got.QuarantinedAt.IsZero() {
		t.Fatalf("QuarantinedAt = %v after recovery, want zero", got.QuarantinedAt)
	}

	// The 3-days-4-times revoke/reauth cycle in the T3 background must keep
	// working: quarantine must be able to trigger again after recovery.
	mgr.MarkResult(ctx, Result{AuthID: "recovered", Provider: "claude", Success: false, Error: revokedOAuthTokenError()})
	mgr.MarkResult(ctx, Result{AuthID: "recovered", Provider: "claude", Success: false, Error: revokedOAuthTokenError()})
	got, ok = mgr.GetByID("recovered")
	if !ok || got == nil || !got.AutoQuarantined {
		t.Fatalf("auth did not re-quarantine after a fresh pair of terminal auth failures post-recovery, got=%+v ok=%v", got, ok)
	}
}

// TestManagerMarkResult_ModelScopedTerminalAuthFailuresAutoQuarantine ensures
// the per-model failure branch of MarkResult (result.Model != "") is covered
// by the same quarantine evaluation as the auth-level branch, since a real
// caller almost always supplies a model.
func TestManagerMarkResult_ModelScopedTerminalAuthFailuresAutoQuarantine(t *testing.T) {
	mgr := NewManager(nil, nil, nil)
	ctx := WithSkipPersist(context.Background())
	auth := &Auth{ID: "model-scoped", Provider: "claude"}
	if _, err := mgr.Register(ctx, auth); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	mgr.MarkResult(ctx, Result{AuthID: "model-scoped", Provider: "claude", Model: "claude-sonnet", Success: false, Error: revokedOAuthTokenError()})
	mgr.MarkResult(ctx, Result{AuthID: "model-scoped", Provider: "claude", Model: "claude-sonnet", Success: false, Error: revokedOAuthTokenError()})

	got, ok := mgr.GetByID("model-scoped")
	if !ok || got == nil {
		t.Fatalf("GetByID returned ok=%v auth=%v", ok, got)
	}
	if !got.AutoQuarantined {
		t.Fatalf("AutoQuarantined = false after two model-scoped terminal auth failures, want true")
	}
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

// TestEvaluateAutoQuarantineLocked_WindowExpiryResetsStreak asserts the
// rolling-window boundary directly: a terminal auth failure that lands after
// authAutoQuarantineWindow has elapsed since the first one in the streak must
// restart the streak at 1 instead of completing it at 2.
func TestEvaluateAutoQuarantineLocked_WindowExpiryResetsStreak(t *testing.T) {
	mgr := NewManager(nil, nil, nil)
	auth := &Auth{ID: "window-expiry"}
	t0 := time.Now()

	mgr.evaluateAutoQuarantineLocked(auth, false, revokedOAuthTokenError(), t0)
	if auth.AutoQuarantined {
		t.Fatalf("AutoQuarantined = true after first terminal auth failure, want false")
	}

	outsideWindow := t0.Add(authAutoQuarantineWindow + time.Minute)
	mgr.evaluateAutoQuarantineLocked(auth, false, revokedOAuthTokenError(), outsideWindow)
	if auth.AutoQuarantined {
		t.Fatalf("AutoQuarantined = true after a terminal auth failure outside the rolling window, want false (streak should restart)")
	}

	mgr.evaluateAutoQuarantineLocked(auth, false, revokedOAuthTokenError(), outsideWindow.Add(time.Minute))
	if !auth.AutoQuarantined {
		t.Fatalf("AutoQuarantined = false after a second terminal auth failure within the restarted window, want true")
	}
}

// TestManagerUpdate_DoesNotRollBackAutoQuarantineFromStaleWriteback covers
// the low#2 concurrency finding: a caller that cloned an auth (e.g. via
// GetByID) before a concurrent MarkResult quarantined the live entry must
// not, by later calling Update with its now-stale clone for an unrelated
// field, silently wipe out the quarantine that was set in the meantime. The
// unrelated field change must still apply, proving this is a targeted guard
// and not a full write rejection.
func TestManagerUpdate_DoesNotRollBackAutoQuarantineFromStaleWriteback(t *testing.T) {
	mgr := NewManager(nil, nil, nil)
	ctx := WithSkipPersist(context.Background())
	// ProxyURL must be set: an auth missing proxy_url is force-marked
	// Status=StatusError/Unavailable=true by the scheduler's upsertAuthLocked
	// on every Register/Update (an unrelated gate, not part of this guard),
	// which would otherwise mask the assertions below.
	auth := &Auth{ID: "race-quarantine", Provider: "claude", Label: "original", ProxyURL: "http://test-proxy:8080"}
	if _, err := mgr.Register(ctx, auth); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	// Simulate a caller taking a clone before the quarantine happens (e.g. a
	// handler that read the auth, then got preempted before calling Update).
	staleClone, ok := mgr.GetByID("race-quarantine")
	if !ok || staleClone == nil {
		t.Fatalf("GetByID returned ok=%v auth=%v", ok, staleClone)
	}
	if staleClone.AutoQuarantined {
		t.Fatalf("precondition failed: stale clone already quarantined")
	}

	// Concurrently, two terminal auth failures quarantine the live entry.
	mgr.MarkResult(ctx, Result{AuthID: "race-quarantine", Provider: "claude", Success: false, Error: revokedOAuthTokenError()})
	mgr.MarkResult(ctx, Result{AuthID: "race-quarantine", Provider: "claude", Success: false, Error: revokedOAuthTokenError()})
	got, ok := mgr.GetByID("race-quarantine")
	if !ok || got == nil || !got.AutoQuarantined {
		t.Fatalf("precondition failed: live entry not quarantined before stale Update, got=%+v ok=%v", got, ok)
	}

	// The caller now writes back its stale clone with an unrelated field
	// change, unaware the live entry was quarantined in the meantime.
	staleClone.Label = "updated-by-stale-caller"
	if _, err := mgr.Update(ctx, staleClone); err != nil {
		t.Fatalf("Update returned error: %v", err)
	}

	got, ok = mgr.GetByID("race-quarantine")
	if !ok || got == nil {
		t.Fatalf("GetByID returned ok=%v auth=%v", ok, got)
	}
	if !got.AutoQuarantined {
		t.Fatalf("AutoQuarantined = false after a stale write-back, want true (quarantine must not be rolled back)")
	}
	if got.QuarantineReason != quarantineReasonTerminalAuthFailure {
		t.Fatalf("QuarantineReason = %q, want preserved %q", got.QuarantineReason, quarantineReasonTerminalAuthFailure)
	}
	if got.QuarantinedAt.IsZero() {
		t.Fatalf("QuarantinedAt is zero, want preserved non-zero")
	}
	if got.Status != StatusQuarantined {
		t.Fatalf("Status = %q, want preserved %q", got.Status, StatusQuarantined)
	}
	// The unrelated field change must still have applied: this guard is
	// targeted at quarantine fields only, not a full write rejection.
	if got.Label != "updated-by-stale-caller" {
		t.Fatalf("Label = %q, want %q (unrelated field write must still apply)", got.Label, "updated-by-stale-caller")
	}
}

// TestManagerUpdate_ExplicitClearAutoQuarantineStillClears asserts the other
// side of the low#2 guard's boundary: a record that itself went through
// Auth.ClearAutoQuarantine (the two sanctioned recovery paths: a completed
// reauth, or an explicit operator re-enable) must still successfully clear
// the lock through Manager.Update, even though its cleared end state is
// otherwise indistinguishable from an unaware stale clone.
func TestManagerUpdate_ExplicitClearAutoQuarantineStillClears(t *testing.T) {
	mgr := NewManager(nil, nil, nil)
	ctx := WithSkipPersist(context.Background())
	auth := &Auth{ID: "explicit-clear", Provider: "claude", ProxyURL: "http://test-proxy:8080"}
	if _, err := mgr.Register(ctx, auth); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	mgr.MarkResult(ctx, Result{AuthID: "explicit-clear", Provider: "claude", Success: false, Error: revokedOAuthTokenError()})
	mgr.MarkResult(ctx, Result{AuthID: "explicit-clear", Provider: "claude", Success: false, Error: revokedOAuthTokenError()})
	got, ok := mgr.GetByID("explicit-clear")
	if !ok || got == nil || !got.AutoQuarantined {
		t.Fatalf("precondition failed: auth not quarantined, got=%+v ok=%v", got, ok)
	}

	// Mirrors PatchAuthFileStatus / PatchAuthFileAccountSettings / saveTokenRecord:
	// fetch a clone, explicitly clear on that exact instance, then Update it.
	target, ok := mgr.GetByID("explicit-clear")
	if !ok || target == nil {
		t.Fatalf("GetByID returned ok=%v auth=%v", ok, target)
	}
	target.ClearAutoQuarantine()
	if _, err := mgr.Update(ctx, target); err != nil {
		t.Fatalf("Update returned error: %v", err)
	}

	got, ok = mgr.GetByID("explicit-clear")
	if !ok || got == nil {
		t.Fatalf("GetByID returned ok=%v auth=%v", ok, got)
	}
	if got.AutoQuarantined {
		t.Fatalf("AutoQuarantined = true after explicit ClearAutoQuarantine + Update, want false")
	}
	if got.QuarantineReason != "" {
		t.Fatalf("QuarantineReason = %q, want empty", got.QuarantineReason)
	}
	if !got.QuarantinedAt.IsZero() {
		t.Fatalf("QuarantinedAt = %v, want zero", got.QuarantinedAt)
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
