package auth

import (
	"context"
	"errors"
	"testing"
)

func TestNonRecovery284IdentityBackfill(t *testing.T) {
	for _, name := range []string{"account", "email-only", "delete-account", "typed-account", "missing-email", "case-email"} {
		t.Run(name, func(t *testing.T) {
			m, _, h, before := setup274(t)
			if name == "missing-email" {
				m.mu.Lock()
				delete(m.auths[before.ID].Metadata, "email")
				m.mu.Unlock()
			}
			saves := 0
			got, err := m.CommitNonRecoveryUpdate(context.Background(), before.ID, before.RegistrationEpoch, before.CredentialVersion, func(a *Auth) (*Auth, error) { a.Metadata["note"] = "accepted"; return a, nil }, func(_ context.Context, a *Auth) error {
				saves++
				switch name {
				case "account":
					a.Metadata["account_id"] = "foreign"
				case "email-only":
					a.Metadata["email"] = "foreign@example.invalid"
				case "delete-account":
					delete(a.Metadata, "account_id")
				case "typed-account":
					a.Metadata["account_id"] = 7
				case "missing-email":
					a.Metadata["email"] = "account274@example.invalid"
				case "case-email":
					a.Metadata["email"] = "ACCOUNT274@example.invalid"
				}
				return nil
			})
			live, _ := m.GetByID(before.ID)
			allowed := name == "missing-email" || name == "case-email"
			if saves != 1 {
				t.Fatalf("saves=%d", saves)
			}
			if allowed {
				if err != nil || got == nil || h.calls != 1 || live.Metadata["note"] != "accepted" {
					t.Fatalf("legitimate backfill rejected: %v", err)
				}
			} else if err == nil || got != nil || h.calls != 0 || live.Metadata["note"] != "old-note" || live.Metadata["account_id"] != "account274" || live.Metadata["email"] != before.Metadata["email"] || live.Generation != before.Generation {
				t.Fatal("foreign identity published or rejection mutated runtime")
			}
		})
	}
}

func TestNonRecovery284ErrorOwnership(t *testing.T) {
	for _, phase := range []string{"prepare-failure", "prepare-success", "save-success", "after-return"} {
		t.Run(phase, func(t *testing.T) {
			m, _, h, before := setup274(t)
			m.mu.Lock()
			m.auths[before.ID].LastError = &Error{Message: "terminal-original", HTTPStatus: 401}
			m.mu.Unlock()
			sentinel := errors.New("prepare failed")
			var prepared, saved, hook *Auth
			h.fn = func(_ context.Context, a *Auth) { hook = a; a.LastError.Message = "hook-mutated" }
			got, err := m.CommitNonRecoveryUpdate(context.Background(), before.ID, before.RegistrationEpoch, before.CredentialVersion, func(a *Auth) (*Auth, error) {
				prepared = a
				a.LastError.Message = "prepare-mutated"
				a.Metadata["note"] = "accepted"
				if phase == "prepare-failure" {
					return nil, sentinel
				}
				return a, nil
			}, func(_ context.Context, a *Auth) error {
				saved = a
				if a.LastError.Message != "terminal-original" {
					t.Error("prepare corrupted persistence terminal state")
				}
				a.LastError.Message = "save-mutated"
				return nil
			})
			if phase == "prepare-failure" {
				if !errors.Is(err, sentinel) || h.calls != 0 {
					t.Fatal("prepare error lost")
				}
			} else {
				if err != nil || got.LastError.Message != "terminal-original" {
					t.Fatalf("saved/hook error polluted returned terminal: %v", err)
				}
				prepared.LastError.Message = "retained-prepare"
				saved.LastError.Message = "retained-save"
				hook.LastError.Message = "retained-hook"
				if phase == "after-return" {
					got.LastError.Message = "caller-mutated"
				}
			}
			live, _ := m.GetByID(before.ID)
			if live.LastError.Message != "terminal-original" {
				t.Fatal("callback or returned pointer aliases manager error")
			}
		})
	}
}
