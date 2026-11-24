package auth

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestOrdinaryManagerQuotaResetPreservesPrivateRetryWindows(t *testing.T) {
	future := time.Now().Add(time.Hour)
	for _, fixture := range []*Auth{
		{ID: "credential-window", credentialRetryAfter: future},
		{ID: "shadow-window", ModelStates: map[string]*ModelState{"fixture": {credentialQuotaShadow: &ModelState{quotaForceCooldownUntil: future}}}},
	} {
		t.Run(fixture.ID, func(t *testing.T) {
			m := NewManager(nil, nil, nil)
			if _, err := m.Register(t.Context(), fixture); err != nil {
				t.Fatal(err)
			}
			before, _ := m.GetByID(fixture.ID)
			if !errors.Is(m.CheckQuotaReset(before), ErrQuotaResetProtected) {
				t.Fatal("admission accepted protected state")
			}
			updated, _, err := m.ResetQuota(t.Context(), fixture.ID)
			after, _ := m.GetByID(fixture.ID)
			if !errors.Is(err, ErrQuotaResetProtected) || updated != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("shared mutation cleared protected state")
			}
		})
	}
}

func TestOrdinaryManagerQuotaResetChecksSelectionAndNewLimitUnderMutation(t *testing.T) {
	for _, replacement := range []bool{false, true} {
		name := "limit recorded"
		if replacement {
			name = "same-id replacement"
		}
		t.Run(name, func(t *testing.T) {
			m := NewManager(nil, nil, nil)
			original := &Auth{ID: "fixture.json", FileName: "fixture.json", Provider: "fixture", Metadata: map[string]any{"access_token": "old"}}
			if _, err := m.Register(t.Context(), original); err != nil {
				t.Fatal(err)
			}
			selected, _ := m.GetByID(original.ID)
			var want = ErrQuotaResetProtected
			if replacement {
				next := original.Clone()
				next.Metadata["access_token"] = "replacement"
				if _, err := m.Register(t.Context(), next); err != nil {
					t.Fatal(err)
				}
				want = ErrQuotaResetSelectionChanged
			} else {
				wait := time.Hour
				m.MarkResult(t.Context(), Result{AuthID: original.ID, Provider: original.Provider, CredentialScope: true,
					RetryAfter: &wait, Error: &Error{HTTPStatus: 429}, Success: false})
			}
			before, _ := m.GetByID(original.ID)
			updated, _, err := m.ResetQuotaForSelection(t.Context(), selected)
			after, _ := m.GetByID(original.ID)
			if updated != nil || !errors.Is(err, want) || !reflect.DeepEqual(before, after) {
				t.Fatalf("stale reset admission mutated state, error=%v", err)
			}
		})
	}
}
