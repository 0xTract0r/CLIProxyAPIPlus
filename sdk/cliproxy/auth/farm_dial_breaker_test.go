package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// armDialBreaker arms the dead-proxy dial-failure breaker and disarms the sibling
// supply-atomicity gate (FARM_REQUIRE_PROVISIONED), which otherwise fail-closes an
// unbound farm-enrolled Claude account and would mask the breaker under test. Env
// is scoped to the test via t.Setenv, so these tests must not run in parallel.
func armDialBreaker(t *testing.T) {
	t.Helper()
	t.Setenv(FarmDialFailureBreakerEnabledEnvVar, "1")
	t.Setenv(FarmRequireProvisionedEnvVar, "0")
}

// dialFailureError builds a representative status-0 connectivity failure: a proxy
// CONNECT/dial that never completed, so no HTTP response was ever received.
func dialFailureError() *Error {
	return &Error{Message: "proxy dial tcp 10.0.0.1:8080: connect: connection refused"}
}

// farmEnrolledDialAuth builds an active, farm-enrolled Claude account with a LEGAL
// per-account proxy_url (so it is schedulable and never tripped by the empty/
// illegal proxy fail-closed guard — the breaker is orthogonal to that gate).
func farmEnrolledDialAuth(id string) *Auth {
	return &Auth{
		ID:       id,
		Provider: "claude",
		Status:   StatusActive,
		ProxyURL: "http://acc-proxy:8080",
		Metadata: map[string]any{FarmEnrolledMetadataKey: true},
	}
}

func dialAuthIDs(auths []*Auth) []string {
	ids := make([]string, 0, len(auths))
	for _, a := range auths {
		if a != nil {
			ids = append(ids, a.ID)
		}
	}
	return ids
}

// TestDialFailureBreaker_FlagOffIsNoop is the critical no-op guard: with the
// feature disarmed, no number of status-0 dial failures may set a breaker window
// or block the account, so behaviour is byte-identical to before this feature.
func TestDialFailureBreaker_FlagOffIsNoop(t *testing.T) {
	t.Setenv(FarmDialFailureBreakerEnabledEnvVar, "0")
	t.Setenv(FarmRequireProvisionedEnvVar, "0")

	mgr := NewManager(nil, nil, nil)
	ctx := WithSkipPersist(context.Background())
	if _, err := mgr.Register(ctx, farmEnrolledDialAuth("farm-off")); err != nil {
		t.Fatalf("Register: %v", err)
	}
	for i := 0; i < 10; i++ {
		mgr.MarkResult(ctx, Result{AuthID: "farm-off", Provider: "claude", Success: false, Error: dialFailureError()})
	}
	got, ok := mgr.GetByID("farm-off")
	if !ok || got == nil {
		t.Fatalf("GetByID ok=%v auth=%v", ok, got)
	}
	if !got.dialBreakerUntil.IsZero() {
		t.Fatalf("dialBreakerUntil set with feature off, want zero")
	}
	if blocked, reason, _ := isAuthBlockedForModel(got, "", time.Now()); blocked {
		t.Fatalf("blocked=%v reason=%v with feature off, want not blocked", blocked, reason)
	}
}

// TestDialFailureBreaker_TripsAfterThresholdViaMarkResult covers the core trip
// behaviour through the full Manager.MarkResult path: N consecutive dial failures
// (with zero intervening successes) trip the breaker exactly at the threshold,
// after which the selector skips the account with blockReasonDialBreaker and a
// future retry time.
func TestDialFailureBreaker_TripsAfterThresholdViaMarkResult(t *testing.T) {
	armDialBreaker(t)
	mgr := NewManager(nil, nil, nil)
	ctx := WithSkipPersist(context.Background())
	if _, err := mgr.Register(ctx, farmEnrolledDialAuth("farm-dead")); err != nil {
		t.Fatalf("Register: %v", err)
	}
	threshold := dialFailureBreakerThreshold()

	for i := 0; i < threshold-1; i++ {
		mgr.MarkResult(ctx, Result{AuthID: "farm-dead", Provider: "claude", Success: false, Error: dialFailureError()})
	}
	got, _ := mgr.GetByID("farm-dead")
	if !got.dialBreakerUntil.IsZero() {
		t.Fatalf("breaker tripped after %d failures, want trip only at threshold %d", threshold-1, threshold)
	}
	if blocked, _, _ := isAuthBlockedForModel(got, "", time.Now()); blocked {
		t.Fatalf("blocked before threshold, want not blocked")
	}

	mgr.MarkResult(ctx, Result{AuthID: "farm-dead", Provider: "claude", Success: false, Error: dialFailureError()})
	got, _ = mgr.GetByID("farm-dead")
	if got.dialBreakerUntil.IsZero() {
		t.Fatalf("breaker not tripped after %d consecutive dial failures", threshold)
	}
	blocked, reason, next := isAuthBlockedForModel(got, "", time.Now())
	if !blocked || reason != blockReasonDialBreaker {
		t.Fatalf("isAuthBlockedForModel = (%v,%v), want (true, blockReasonDialBreaker=%v)", blocked, reason, blockReasonDialBreaker)
	}
	if next.IsZero() || !next.After(time.Now()) {
		t.Fatalf("next=%v, want a future breaker-until time", next)
	}
}

// TestDialFailureBreaker_RestoresWhenWindowExpires proves the auto-restore on
// backoff expiry: an elapsed window no longer blocks (the account rejoins the
// rotation and re-probes), while an active future window does block.
func TestDialFailureBreaker_RestoresWhenWindowExpires(t *testing.T) {
	armDialBreaker(t)
	now := time.Now()
	auth := farmEnrolledDialAuth("farm-expired")

	auth.dialBreakerUntil = now.Add(-time.Second) // already elapsed
	if forkDialFailureBreakerBlocked(auth, now) {
		t.Fatalf("breaker still blocking after window elapsed")
	}
	if blocked, _, _ := isAuthBlockedForModel(auth, "", now); blocked {
		t.Fatalf("isAuthBlockedForModel blocked after window elapsed, want restored")
	}

	auth.dialBreakerUntil = now.Add(time.Minute) // active
	if !forkDialFailureBreakerBlocked(auth, now) {
		t.Fatalf("breaker not blocking with an active future window")
	}
	if blocked, reason, _ := isAuthBlockedForModel(auth, "", now); !blocked || reason != blockReasonDialBreaker {
		t.Fatalf("isAuthBlockedForModel = (%v,%v) with active window, want (true, dialBreaker)", blocked, reason)
	}
}

// TestDialFailureBreaker_SuccessClearsBreaker proves the dial-recovery
// auto-restore: a single real success clears both the streak and the breaker
// window, so a recovered proxy is never left parked.
func TestDialFailureBreaker_SuccessClearsBreaker(t *testing.T) {
	armDialBreaker(t)
	mgr := NewManager(nil, nil, nil)
	ctx := WithSkipPersist(context.Background())
	if _, err := mgr.Register(ctx, farmEnrolledDialAuth("farm-recover")); err != nil {
		t.Fatalf("Register: %v", err)
	}
	for i := 0; i < dialFailureBreakerThreshold(); i++ {
		mgr.MarkResult(ctx, Result{AuthID: "farm-recover", Provider: "claude", Success: false, Error: dialFailureError()})
	}
	got, _ := mgr.GetByID("farm-recover")
	if got.dialBreakerUntil.IsZero() {
		t.Fatalf("precondition: breaker should be tripped after threshold failures")
	}

	mgr.MarkResult(ctx, Result{AuthID: "farm-recover", Provider: "claude", Success: true})
	got, _ = mgr.GetByID("farm-recover")
	if !got.dialBreakerUntil.IsZero() {
		t.Fatalf("dialBreakerUntil not cleared after a real success")
	}
	if got.dialFailureStreak != 0 {
		t.Fatalf("dialFailureStreak = %d after success, want 0", got.dialFailureStreak)
	}
	if blocked, _, _ := isAuthBlockedForModel(got, "", time.Now()); blocked {
		t.Fatalf("still blocked after recovery success")
	}
}

// TestDialFailureBreaker_OrdinaryAccountUnaffected proves farm-scoping: a
// non-farm-enrolled account is never tripped or blocked by the breaker no matter
// how many dial failures it sees.
func TestDialFailureBreaker_OrdinaryAccountUnaffected(t *testing.T) {
	armDialBreaker(t)
	mgr := NewManager(nil, nil, nil)
	ctx := WithSkipPersist(context.Background())
	ordinary := &Auth{ID: "ordinary", Provider: "claude", Status: StatusActive, ProxyURL: "http://p:8080"}
	if _, err := mgr.Register(ctx, ordinary); err != nil {
		t.Fatalf("Register: %v", err)
	}
	for i := 0; i < dialFailureBreakerThreshold()+3; i++ {
		mgr.MarkResult(ctx, Result{AuthID: "ordinary", Provider: "claude", Success: false, Error: dialFailureError()})
	}
	got, _ := mgr.GetByID("ordinary")
	if !got.dialBreakerUntil.IsZero() {
		t.Fatalf("dialBreakerUntil set on a non-farm account, want zero")
	}
	if blocked, _, _ := isAuthBlockedForModel(got, "", time.Now()); blocked {
		t.Fatalf("ordinary account blocked by dial breaker, want unaffected")
	}
}

// TestDialFailureBreaker_NonDialFailureDoesNotTrip proves that failures which DID
// get an HTTP response (429/5xx) never trip the dial breaker — those keep their
// own dedicated cooldown path.
func TestDialFailureBreaker_NonDialFailureDoesNotTrip(t *testing.T) {
	armDialBreaker(t)
	mgr := NewManager(nil, nil, nil)
	ctx := WithSkipPersist(context.Background())
	if _, err := mgr.Register(ctx, farmEnrolledDialAuth("farm-http")); err != nil {
		t.Fatalf("Register: %v", err)
	}
	nonDial := []*Error{
		{HTTPStatus: http.StatusTooManyRequests, Message: "rate limited"},
		{HTTPStatus: http.StatusServiceUnavailable, Message: "overloaded"},
	}
	for i := 0; i < 4; i++ {
		for _, e := range nonDial {
			mgr.MarkResult(ctx, Result{AuthID: "farm-http", Provider: "claude", Success: false, Error: e})
		}
	}
	got, _ := mgr.GetByID("farm-http")
	if !got.dialBreakerUntil.IsZero() {
		t.Fatalf("dial breaker tripped on non-dial (HTTP) failures, want no trip")
	}
}

// TestDialFailureBreaker_NonDialFailureDoesNotResetStreak documents a deliberate
// design decision (mirroring the auto-quarantine streak): a non-dial failure
// interleaved between dial failures neither trips nor resets the in-progress dial
// streak, so dial failures keep accumulating across a transient HTTP blip.
func TestDialFailureBreaker_NonDialFailureDoesNotResetStreak(t *testing.T) {
	armDialBreaker(t)
	mgr := NewManager(nil, nil, nil)
	auth := farmEnrolledDialAuth("farm-interleave")
	base := time.Now()

	mgr.evaluateDialFailureBreakerLocked(auth, false, dialFailureError(), base)
	if auth.dialFailureStreak != 1 {
		t.Fatalf("streak = %d after 1 dial failure, want 1", auth.dialFailureStreak)
	}
	// A 429 is not a dial failure; it must not reset the streak.
	mgr.evaluateDialFailureBreakerLocked(auth, false, &Error{HTTPStatus: http.StatusTooManyRequests, Message: "x"}, base.Add(time.Second))
	if auth.dialFailureStreak != 1 {
		t.Fatalf("streak = %d after a non-dial failure, want 1 (preserved)", auth.dialFailureStreak)
	}
	// The next dial failure advances as if uninterrupted.
	mgr.evaluateDialFailureBreakerLocked(auth, false, dialFailureError(), base.Add(2*time.Second))
	if auth.dialFailureStreak != 2 {
		t.Fatalf("streak = %d, want 2 (dial failures accumulate across a non-dial blip)", auth.dialFailureStreak)
	}
}

// TestDialFailureBreaker_WindowResetsStreak proves a dial failure that lands after
// the rolling window restarts the streak, so an occasional isolated blip never
// accumulates toward a trip.
func TestDialFailureBreaker_WindowResetsStreak(t *testing.T) {
	armDialBreaker(t)
	mgr := NewManager(nil, nil, nil)
	auth := farmEnrolledDialAuth("farm-window")
	window := dialFailureBreakerWindow()
	base := time.Now()

	// Two consecutive dial failures (threshold defaults to 3, so no trip yet).
	mgr.evaluateDialFailureBreakerLocked(auth, false, dialFailureError(), base)
	mgr.evaluateDialFailureBreakerLocked(auth, false, dialFailureError(), base.Add(time.Second))
	if auth.dialFailureStreak != 2 {
		t.Fatalf("streak = %d after 2 failures, want 2", auth.dialFailureStreak)
	}

	// A dial failure after the window restarts the streak at 1.
	mgr.evaluateDialFailureBreakerLocked(auth, false, dialFailureError(), base.Add(window+time.Minute))
	if auth.dialFailureStreak != 1 {
		t.Fatalf("streak = %d after a gap > window, want 1 (restart)", auth.dialFailureStreak)
	}
	if !auth.dialBreakerUntil.IsZero() {
		t.Fatalf("breaker tripped after a window-reset restart, want not tripped")
	}
}

// TestDialFailureBreaker_NonStarvation_LegacyPool proves the legacy selection pool
// never collapses to "no auth available" when every candidate is blocked solely
// by the dial breaker: it falls back to a last-resort pick (which doubles as a
// recovery probe).
func TestDialFailureBreaker_NonStarvation_LegacyPool(t *testing.T) {
	armDialBreaker(t)
	now := time.Now()
	a := farmEnrolledDialAuth("a")
	b := farmEnrolledDialAuth("b")
	a.dialBreakerUntil = now.Add(time.Minute)
	b.dialBreakerUntil = now.Add(time.Minute)

	got, err := getAvailableAuths([]*Auth{a, b}, "claude", "", now)
	if err != nil {
		t.Fatalf("getAvailableAuths errored when all breaker-blocked: %v (must not starve)", err)
	}
	if len(got) == 0 {
		t.Fatalf("getAvailableAuths returned empty fallback pool, want a last-resort candidate")
	}
}

// TestDialFailureBreaker_HealthyPreferredOverBreaker proves the breaker is a
// PREFER-TO-SKIP, not a hard exclude: a healthy account is always chosen over a
// breaker-blocked one when both exist.
func TestDialFailureBreaker_HealthyPreferredOverBreaker(t *testing.T) {
	armDialBreaker(t)
	now := time.Now()
	dead := farmEnrolledDialAuth("dead")
	dead.dialBreakerUntil = now.Add(time.Minute)
	healthy := farmEnrolledDialAuth("healthy")

	got, err := getAvailableAuths([]*Auth{dead, healthy}, "claude", "", now)
	if err != nil {
		t.Fatalf("getAvailableAuths: %v", err)
	}
	if len(got) != 1 || got[0].ID != "healthy" {
		t.Fatalf("available = %v, want only [healthy] (breaker-blocked skipped while a healthy one exists)", dialAuthIDs(got))
	}
}

// TestDialFailureBreaker_NonStarvation_Scheduler proves the built-in scheduler
// path also refuses to starve: an all-breaker-blocked farm still yields a
// last-resort pick instead of erroring.
func TestDialFailureBreaker_NonStarvation_Scheduler(t *testing.T) {
	armDialBreaker(t)
	now := time.Now()
	a := farmEnrolledDialAuth("sa")
	b := farmEnrolledDialAuth("sb")
	a.dialBreakerUntil = now.Add(time.Minute)
	b.dialBreakerUntil = now.Add(time.Minute)

	scheduler := newSchedulerForTest(&RoundRobinSelector{}, a, b)
	got, err := scheduler.pickSingle(context.Background(), "claude", "", cliproxyexecutor.Options{}, nil)
	if err != nil {
		t.Fatalf("pickSingle errored when all breaker-blocked: %v (must not starve)", err)
	}
	if got == nil {
		t.Fatalf("pickSingle returned nil fallback, want a last-resort candidate")
	}
}

// TestDialFailureBreaker_Scheduler_HealthyPreferred proves the built-in scheduler
// skips a breaker-blocked account while a healthy one exists.
func TestDialFailureBreaker_Scheduler_HealthyPreferred(t *testing.T) {
	armDialBreaker(t)
	now := time.Now()
	dead := farmEnrolledDialAuth("s-dead")
	dead.dialBreakerUntil = now.Add(time.Minute)
	healthy := farmEnrolledDialAuth("s-healthy")

	scheduler := newSchedulerForTest(&RoundRobinSelector{}, dead, healthy)
	for i := 0; i < 5; i++ {
		got, err := scheduler.pickSingle(context.Background(), "claude", "", cliproxyexecutor.Options{}, nil)
		if err != nil {
			t.Fatalf("pickSingle #%d: %v", i, err)
		}
		if got == nil || got.ID != "s-healthy" {
			t.Fatalf("pickSingle #%d = %v, want s-healthy (breaker-blocked skipped while healthy exists)", i, got)
		}
	}
}

// TestIsDialFailureResultError pins the connectivity-failure classifier.
func TestIsDialFailureResultError(t *testing.T) {
	cases := []struct {
		name string
		err  *Error
		want bool
	}{
		{"nil", nil, false},
		{"status0-with-message", &Error{Message: "dial tcp: connection refused"}, true},
		{"status0-with-code", &Error{Code: "proxy_dial_failed"}, true},
		{"status0-empty", &Error{}, false},
		{"401", &Error{HTTPStatus: http.StatusUnauthorized, Message: "unauthorized"}, false},
		{"429", &Error{HTTPStatus: http.StatusTooManyRequests, Message: "rate limited"}, false},
		{"503", &Error{HTTPStatus: http.StatusServiceUnavailable, Message: "overloaded"}, false},
		{"request-scoped", &Error{Code: requestScopedErrorCode, Message: "store=false item miss"}, false},
	}
	for _, c := range cases {
		if got := isDialFailureResultError(c.err); got != c.want {
			t.Errorf("%s: isDialFailureResultError = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestDialFailureBreaker_EscalatingBackoff pins the escalating, capped backoff
// ladder: base at the first trip, doubling per extra consecutive failure, capped
// at the max, and never overflowing on a deep streak.
func TestDialFailureBreaker_EscalatingBackoff(t *testing.T) {
	armDialBreaker(t)
	base := dialFailureBreakerBaseBackoff()
	max := dialFailureBreakerMaxBackoff()
	threshold := dialFailureBreakerThreshold()

	if got := dialFailureBreakerBackoffForStreak(threshold); got != base {
		t.Fatalf("first-trip backoff = %v, want base %v", got, base)
	}
	if got := dialFailureBreakerBackoffForStreak(threshold + 1); got != base*2 {
		t.Fatalf("second-trip backoff = %v, want 2*base %v", got, base*2)
	}
	if got := dialFailureBreakerBackoffForStreak(threshold + 1000); got != max {
		t.Fatalf("deep-streak backoff = %v, want capped at max %v", got, max)
	}
	if got := dialFailureBreakerBackoffForStreak(threshold - 5); got != base {
		t.Fatalf("below-threshold backoff = %v, want base %v", got, base)
	}
}
