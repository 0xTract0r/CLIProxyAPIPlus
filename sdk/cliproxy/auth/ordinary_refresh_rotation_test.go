package auth

import (
	"context"
	"testing"
	"time"
)

type ordinaryRotationStore struct{ record *Auth }

func (s *ordinaryRotationStore) List(context.Context) ([]*Auth, error) {
	return []*Auth{s.record.Clone()}, nil
}
func (s *ordinaryRotationStore) Save(_ context.Context, a *Auth) (string, error) {
	s.record = a.Clone()
	return "fixture.json", nil
}
func (*ordinaryRotationStore) Delete(context.Context, string) error { return nil }

type ordinaryRotationExecutor struct {
	ProviderExecutor
	refresh func(*Auth) (*Auth, error)
}

func (*ordinaryRotationExecutor) Identifier() string { return "synthetic" }
func (e *ordinaryRotationExecutor) Refresh(_ context.Context, a *Auth) (*Auth, error) {
	return e.refresh(a)
}

func TestOrdinaryRotationWithClockRollback(t *testing.T) {
	for _, replace := range []bool{false, true} {
		name := "same-grant"
		if replace {
			name = "replacement-grant"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			store := &ordinaryRotationStore{}
			manager := NewManager(store, nil, nil)
			const id = "fixture.json"
			future := time.Now().Add(24 * time.Hour)
			_, err := manager.Register(ctx, &Auth{ID: id, Provider: "synthetic", UpdatedAt: future, LastRefreshedAt: future,
				Metadata: map[string]any{"refresh_token": "fixture-old", "access_token": "fixture-old-access", "note": "keep"}})
			if err != nil {
				t.Fatal(err)
			}
			expected := "fixture-rotated"
			manager.RegisterExecutor(&ordinaryRotationExecutor{refresh: func(a *Auth) (*Auth, error) {
				if replace {
					current, _ := manager.GetByID(id)
					current.Metadata["refresh_token"] = "fixture-replacement"
					current.Metadata["access_token"] = "fixture-replacement-access"
					expected = "fixture-replacement"
					if _, err := manager.Update(ctx, current); err != nil {
						return nil, err
					}
				}
				a.Metadata["refresh_token"] = "fixture-rotated"
				a.Metadata["access_token"] = "fixture-rotated-access"
				a.UpdatedAt = time.Now()
				a.LastRefreshedAt = a.UpdatedAt
				return a, nil
			}})
			_, err = manager.ForceRefreshAuth(ctx, id)
			if err != nil && !replace {
				t.Fatal(err)
			}
			current, _ := manager.GetByID(id)
			reopened := NewManager(store, nil, nil)
			if err := reopened.Load(ctx); err != nil {
				t.Fatal(err)
			}
			cold, _ := reopened.GetByID(id)
			for _, record := range []*Auth{current, cold} {
				if record == nil || record.Metadata["refresh_token"] != expected || record.Metadata["note"] != "keep" {
					t.Fatal("refresh grant or concurrent replacement did not survive RAM and cold load")
				}
			}
		})
	}
}

func TestOrdinarySuccessfulRefreshRetryDelay(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		expiry string
		want   time.Duration
	}{
		{"missing", "", 15 * time.Minute},
		{"expired", now.Add(-time.Second).Format(time.RFC3339), refreshIneffectiveBackoff},
		{"short", now.Add(time.Second).Format(time.RFC3339), refreshIneffectiveBackoff},
		{"valid", now.Add(time.Hour).Format(time.RFC3339), 30 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			record := &Auth{Metadata: map[string]any{"expired": tc.expiry}}
			if got := successfulRefreshRetryDelay(record, now); got != tc.want {
				t.Fatalf("retry delay = %s, want %s", got, tc.want)
			}
		})
	}
}
