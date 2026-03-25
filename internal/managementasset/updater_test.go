package managementasset

import (
	"errors"
	"net/http"
	"testing"
	"time"
)

func TestReleaseCooldownFromErrorPrefersRateLimitReset(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	reset := now.Add(10 * time.Minute)

	until, ok := releaseCooldownFromError(now, &releaseRequestError{
		StatusCode:     http.StatusForbidden,
		Body:           "API rate limit exceeded",
		RateLimitReset: reset,
	})
	if !ok {
		t.Fatalf("expected rate-limit cooldown")
	}

	want := reset.Add(5 * time.Second)
	if !until.Equal(want) {
		t.Fatalf("expected cooldown until %v, got %v", want, until)
	}
}

func TestReleaseCooldownFromErrorPrefersRetryAfter(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)

	until, ok := releaseCooldownFromError(now, &releaseRequestError{
		StatusCode: http.StatusTooManyRequests,
		Body:       "too many requests",
		RetryAfter: 2 * time.Minute,
	})
	if !ok {
		t.Fatalf("expected rate-limit cooldown")
	}

	want := now.Add(2 * time.Minute)
	if !until.Equal(want) {
		t.Fatalf("expected cooldown until %v, got %v", want, until)
	}
}

func TestReleaseCooldownFromErrorFallsBackToDefault(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)

	until, ok := releaseCooldownFromError(now, &releaseRequestError{
		StatusCode: http.StatusForbidden,
		Body:       "API rate limit exceeded",
	})
	if !ok {
		t.Fatalf("expected rate-limit cooldown")
	}

	want := now.Add(releaseRateLimitCooldown)
	if !until.Equal(want) {
		t.Fatalf("expected cooldown until %v, got %v", want, until)
	}
}

func TestReleaseCooldownFromErrorIgnoresNonRateLimitErrors(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)

	if _, ok := releaseCooldownFromError(now, errors.New("boom")); ok {
		t.Fatalf("unexpected cooldown for generic error")
	}
	if _, ok := releaseCooldownFromError(now, &releaseRequestError{
		StatusCode: http.StatusInternalServerError,
		Body:       "server exploded",
	}); ok {
		t.Fatalf("unexpected cooldown for non-rate-limit status")
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)

	if got := parseRetryAfter("120", now); got != 120*time.Second {
		t.Fatalf("expected 120s, got %v", got)
	}

	httpDate := now.Add(90 * time.Second).UTC().Format(http.TimeFormat)
	if got := parseRetryAfter(httpDate, now); got != 90*time.Second {
		t.Fatalf("expected 90s from HTTP date, got %v", got)
	}
}

func TestParseRateLimitReset(t *testing.T) {
	got := parseRateLimitReset("1700000600")
	want := time.Unix(1_700_000_600, 0)
	if !got.Equal(want) {
		t.Fatalf("expected %v, got %v", want, got)
	}
}
