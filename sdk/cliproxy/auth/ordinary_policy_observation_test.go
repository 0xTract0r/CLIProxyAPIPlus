package auth

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
)

type ordinaryAuditStore struct {
	best    *Auth
	saveErr error
}

func (s *ordinaryAuditStore) List(context.Context) ([]*Auth, error) { return nil, nil }
func (s *ordinaryAuditStore) Delete(context.Context, string) error  { return nil }
func (s *ordinaryAuditStore) Save(_ context.Context, a *Auth) (string, error) {
	if s.saveErr != nil {
		return "", s.saveErr
	}
	s.best = a.Clone()
	return a.ID, nil
}

func TestOrdinaryRefusalAuditKeepsConcurrentObservations(t *testing.T) {
	ctx := context.Background()
	store := &ordinaryAuditStore{}
	m := NewManager(store, nil, nil)
	const id = "local-policy-fixture"
	if _, err := m.Register(ctx, &Auth{ID: id, Provider: "codex", Metadata: map[string]any{"note": "keep"}}); err != nil {
		t.Fatal(err)
	}
	const total = 16
	var writers sync.WaitGroup
	for range total {
		writers.Go(func() {
			if _, ts, err := m.IncrementCyberPolicyCount(ctx, id); err != nil || ts.IsZero() {
				t.Errorf("record: %v, %v", ts, err)
			}
		})
	}
	writers.Wait()
	current, ok := m.GetByID(id)
	if !ok || current.CyberPolicyFlagCount != total || store.best == nil || store.best.CyberPolicyFlagCount != total || store.best.Metadata["note"] != "keep" {
		t.Fatal("concurrent observations or unrelated metadata were lost")
	}
	store.saveErr = errors.New("synthetic write error")
	n, _, err := m.IncrementCyberPolicyCount(ctx, id)
	if !errors.Is(err, store.saveErr) || n != total+1 || store.best.CyberPolicyFlagCount != total {
		t.Fatal("audit persistence error contract changed")
	}
	if n, at, err := m.IncrementCyberPolicyCount(ctx, "missing"); n != 0 || !at.IsZero() || err != nil {
		t.Fatal("absent credential was mutated")
	}
}

func TestOrdinaryReadOnlyCreditHeaders(t *testing.T) {
	h := make(http.Header)
	h.Set("X-Codex-Credits-Has-Credits", "true")
	for _, c := range []struct {
		balance string
		want    bool
	}{{"0", false}, {"nan", false}, {"+Inf", false}, {"1.5", true}} {
		h.Set("X-Codex-Credits-Balance", c.balance)
		if CodexCreditsUsable(h) != c.want {
			t.Errorf("unexpected credit status for %q", c.balance)
		}
	}
	h.Set("X-Codex-Credits-Has-Credits", "false")
	if CodexCreditsUsable(h) {
		t.Fatal("credit status requires an affirmative provider signal")
	}
}
