package auth

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestOrdinaryStickySelectorKeepsProviderRefusal(t *testing.T) {
	s := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{RebindOn: "never"})
	t.Cleanup(s.Stop)
	ctx := context.Background()
	headers := http.Header{}
	headers.Set("X-Session-ID", "synthetic-sticky-session")
	opts := cliproxyexecutor.Options{Headers: headers}
	a := &Auth{ID: "fixture", Provider: "fixture"}
	if got, err := s.Pick(ctx, "fixture", "model", opts, []*Auth{a}); got == nil || got.ID != a.ID || err != nil {
		t.Fatalf("initial binding=%v err=%v", got, err)
	}
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests} {
		blocked := a.Clone()
		blocked.Unavailable = true
		blocked.NextRetryAfter = time.Now().Add(time.Minute)
		blocked.Quota = QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: blocked.NextRetryAfter}
		blocked.LastError = &Error{Code: "fixture_rate", Message: "synthetic quota refusal", HTTPStatus: status}
		if got, err := s.Pick(ctx, "fixture", "model", opts, []*Auth{blocked}); got != nil || statusCodeFromError(err) != status || SafeResponseHeaders(err).Get("Retry-After") == "" || !errors.Is(err, blocked.LastError) {
			t.Fatalf("binding returned different refusal: got=%v err=%v headers=%v", got, err, SafeResponseHeaders(err))
		}
	}
	if got, err := s.Pick(ctx, "fixture", "model", opts, []*Auth{a}); got == nil || got.ID != a.ID || err != nil {
		t.Fatalf("binding lost after refusal: got=%v err=%v", got, err)
	}
}

func TestOrdinarySelectorRejectsPolicyCancelAndUnentitledModel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a := &Auth{ID: "fixture", Provider: "codex", Metadata: map[string]any{"plan_type": "plus"}}
	s := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{RebindOn: "quota"})
	t.Cleanup(s.Stop)
	if _, err := s.Pick(context.Background(), "codex", "standard", cliproxyexecutor.Options{}, []*Auth{a}); statusCodeFromError(err) != 400 {
		t.Fatalf("unsupported rebinding unexpectedly admitted: %v", err)
	}
	for _, pick := range []Selector{&RoundRobinSelector{}, &WeightedRoundRobinSelector{}, &FillFirstSelector{}} {
		if got, err := pick.Pick(ctx, "codex", "standard", cliproxyexecutor.Options{}, []*Auth{a}); got != nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled selector admitted: got=%v err=%v", got, err)
		}
		if got, err := pick.Pick(context.Background(), "codex", "gpt-5.3-codex-spark", cliproxyexecutor.Options{}, []*Auth{a}); got != nil || err == nil {
			t.Fatalf("unentitled model admitted: got=%v err=%v", got, err)
		}
	}
}
