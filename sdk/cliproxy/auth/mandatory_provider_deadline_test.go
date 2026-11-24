package auth

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestOrdinaryProvider429IgnoresDisableCoolingAndShorterHints(t *testing.T) {
	SetQuotaCooldownDisabled(true)
	t.Cleanup(func() { SetQuotaCooldownDisabled(false) })
	for _, model := range []string{"", "fixture-model"} {
		t.Run("model="+model, func(t *testing.T) {
			ctx := context.Background()
			m := NewManager(nil, nil, nil)
			m.SetConfigSnapshot(&internalconfig.Config{DisableCooling: true})
			const id = "fixture-auth"
			if _, err := m.Register(ctx, &Auth{ID: id, Provider: "synthetic", Metadata: map[string]any{"disable_cooling": true}}); err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			status := &Error{Code: "fixture_rate", Message: "synthetic quota rejection", HTTPStatus: http.StatusTooManyRequests}
			mark := func(d time.Duration) {
				m.MarkResult(ctx, Result{AuthID: id, Provider: "synthetic", Model: model, RetryAfter: &d, Error: status})
			}
			check := func() {
				t.Helper()
				stored, ok := m.GetByID(id)
				if !ok {
					t.Fatal("fixture disappeared")
				}
				deadline, last := stored.NextRetryAfter, stored.LastError
				if model != "" {
					state := stored.ModelStates[model]
					if state == nil {
						t.Fatal("model refusal was lost")
					}
					deadline, last = state.NextRetryAfter, state.LastError
				}
				if deadline.Before(start.Add(5*time.Minute)) || last == nil || last.HTTPStatus != status.HTTPStatus || last.Code != status.Code {
					t.Fatalf("provider deadline or original status lost: %v, %+v", deadline, last)
				}
			}
			mark(5 * time.Minute)
			check()
			mark(time.Second)
			check()
			m.SetConfigSnapshot(&internalconfig.Config{DisableCooling: true})
			check()
		})
	}
}

func TestOrdinaryAuthRejectionsPreserveOriginalStatus(t *testing.T) {
	now := time.Now()
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		state := &Auth{}
		status := &Error{Code: "provider-rejected", Message: "synthetic refusal", HTTPStatus: code}
		hint := 45 * time.Minute
		applyAuthFailureState(state, status, &hint, now, true)
		if state.LastError == nil || state.LastError.HTTPStatus != code || state.LastError.Code != status.Code || !state.NextRetryAfter.Equal(now.Add(hint)) {
			t.Fatalf("provider %d status or retry deadline not retained: %+v (%v)", code, state.LastError, state.NextRetryAfter)
		}
	}
}

func TestOrdinaryWebsocketConnectionPressure(t *testing.T) {
	for _, code := range []int{400, 429, 503} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			for _, model := range []string{"", "fixture-model"} {
				m := NewManager(nil, nil, nil)
				ctx := context.Background()
				m.SetConfigSnapshot(&internalconfig.Config{DisableCooling: true})
				if _, err := m.Register(ctx, &Auth{ID: "fixture", Provider: "synthetic"}); err != nil {
					t.Fatal(err)
				}
				e := ResultErrorFromError(&Error{HTTPStatus: code, Message: `{"error":{"code":"websocket_connection_limit_reached"}}`})
				if e.HTTPStatus != code || !isWebsocketConnectionLimitResultError(e) {
					t.Fatalf("status or explicit pressure classification lost: %+v", e)
				}
				start := time.Now()
				hint := 3 * time.Minute
				m.MarkResult(ctx, Result{AuthID: "fixture", Model: model, RetryAfter: &hint, Error: e})
				a, _ := m.GetByID("fixture")
				deadline, last, quota := a.NextRetryAfter, a.LastError, a.Quota.Exceeded
				if model != "" {
					st := a.ModelStates[model]
					if st == nil {
						t.Fatal("missing model pressure")
					}
					deadline, last, quota = st.NextRetryAfter, st.LastError, st.Quota.Exceeded
				}
				if a.Disabled || deadline.Before(start.Add(hint)) || last == nil || last.HTTPStatus != code || quota != (code == 429) {
					t.Fatalf("unexpected status %d model %q: disabled=%v deadline=%v last=%+v quota=%v", code, model, a.Disabled, deadline, last, quota)
				}
			}
		})
	}
	if isWebsocketConnectionLimitResultError(&Error{HTTPStatus: 400, Message: "websocket_connection_limit_reached"}) {
		t.Fatal("arbitrary error strings must not classify as WebSocket pressure")
	}
}
