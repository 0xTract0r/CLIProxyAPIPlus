package auth

import (
	"context"
	"net/http"
	"testing"
	"time"
)

func TestRequestScoped401PreservesQuarantineBoundary(t *testing.T) {
	for _, tc := range []struct {
		name, code           string
		quarantine, cooldown bool
	}{
		{"credential", "", true, true},
		{"request", ErrorCodeRequestScoped, false, false},
		{"limited cooldown", ErrorCodeForceCooldown, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager := NewManager(nil, nil, nil)
			auth, err := manager.Register(context.Background(), &Auth{ID: tc.name, Provider: "codex", ProxyURL: "direct", Metadata: map[string]any{"access_token": "fixture-token"}})
			if err != nil {
				t.Fatal(err)
			}
			result := Result{AuthID: auth.ID, Provider: auth.Provider, Model: "fixture-model", Error: &Error{Code: tc.code, HTTPStatus: http.StatusUnauthorized, Message: "fixture 401"}}
			if got := resultFeedsFailureCluster(result); got != tc.quarantine {
				t.Fatalf("failure cluster=%v, want %v", got, tc.quarantine)
			}
			for i := 0; i < authAutoQuarantineFailureThreshold; i++ {
				manager.MarkResult(context.Background(), result)
			}
			current, _ := manager.GetByID(auth.ID)
			if current.AutoQuarantined != tc.quarantine {
				t.Fatalf("quarantine=%v, want %v", current.AutoQuarantined, tc.quarantine)
			}
			if !tc.quarantine && current.RejectedAccessToken != "" {
				t.Fatal("request policy rejected credential token")
			}
			state := current.ModelStates["fixture-model"]
			if tc.code == ErrorCodeForceCooldown {
				if state == nil || state.NextRetryAfter.IsZero() {
					t.Fatal("force cooldown did not establish a bounded window")
				}
				if blocked, _, _ := isAuthBlockedForModel(current, "fixture-model", time.Now()); !blocked {
					t.Fatal("force cooldown not active")
				}
				if blocked, _, _ := isAuthBlockedForModel(current, "fixture-model", state.NextRetryAfter.Add(time.Second)); blocked {
					t.Fatal("force cooldown became terminal")
				}
			} else if !tc.quarantine {
				if blocked, _, _ := isAuthBlockedForModel(current, "fixture-model", time.Now()); blocked {
					t.Fatal("request-scoped 401 blocked credential")
				}
			} else if current.RejectedAccessToken != "fixture-token" {
				t.Fatal("real credential 401 no longer rejects the token")
			}
		})
	}
}
