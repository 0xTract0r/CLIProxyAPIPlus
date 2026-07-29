package auth

import (
	"context"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
)

// TestReconcileRegistryModelStates_PreservesAutoQuarantineView guards against a
// regression where the periodic per-model reconciliation pass (triggered by
// registry model re-registration) silently flipped an auto_quarantined auth's
// Status back to StatusActive and Unavailable back to false, while leaving the
// AutoQuarantined/QuarantineReason/QuarantinedAt trio untouched. That produced
// a self-contradictory persisted state (AutoQuarantined=true but
// Status=active/Unavailable=false) that every status reader -- logs, other
// APIs, and both management frontends -- would misread as a false "healthy"
// signal, even though the selector still correctly excluded the credential
// via AutoQuarantined (see selector.go isAuthBlockedForModel).
func TestReconcileRegistryModelStates_PreservesAutoQuarantineView(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	ctx := context.Background()
	authID := "reconcile-quarantine-auth"
	model := "reconcile-quarantine-model"

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(authID, "claude", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() {
		reg.UnregisterClient(authID)
	})

	quarantinedAt := time.Now().Add(-time.Minute)
	quarantineMessage := "auto_quarantined: repeated authentication failures, credential needs re-authentication"
	if _, err := manager.Register(ctx, &Auth{
		ID:               authID,
		Provider:         "claude",
		ProxyURL:         "http://test-proxy:8080",
		Status:           StatusQuarantined,
		StatusMessage:    quarantineMessage,
		Unavailable:      true,
		AutoQuarantined:  true,
		QuarantineReason: quarantineReasonTerminalAuthFailure,
		QuarantinedAt:    quarantinedAt,
		LastError:        &Error{Code: "unauthorized", Message: "authentication_error", HTTPStatus: 401},
		ModelStates: map[string]*ModelState{
			// A stale, non-clean per-model state is required to make the
			// reconciliation pass observe `changed=true` and actually run its
			// auth-level status/availability sync; this reproduces the exact
			// shape a real auto_quarantined auth carries right after
			// markAutoQuarantine.
			model: {
				Status:      StatusError,
				Unavailable: true,
				LastError:   &Error{Code: "unauthorized", Message: "authentication_error", HTTPStatus: 401},
			},
		},
	}); err != nil {
		t.Fatalf("register quarantined auth: %v", err)
	}

	manager.ReconcileRegistryModelStates(ctx, authID)

	updated, ok := manager.GetByID(authID)
	if !ok || updated == nil {
		t.Fatalf("GetByID(%q) not found after reconcile", authID)
	}
	if updated.Status != StatusQuarantined {
		t.Fatalf("Status = %q, want %q (reconcile must not flip a quarantined auth back to active)", updated.Status, StatusQuarantined)
	}
	if updated.StatusMessage != quarantineMessage {
		t.Fatalf("StatusMessage = %q, want preserved %q", updated.StatusMessage, quarantineMessage)
	}
	if !updated.Unavailable {
		t.Fatalf("Unavailable = false, want true for a quarantined auth")
	}
	if !updated.NextRetryAfter.IsZero() {
		t.Fatalf("NextRetryAfter = %v, want zero (quarantine implies no scheduled retry)", updated.NextRetryAfter)
	}
	if updated.LastError == nil {
		t.Fatalf("LastError cleared, want preserved for a quarantined auth")
	}
	if !updated.AutoQuarantined {
		t.Fatalf("AutoQuarantined = false, want true (unchanged)")
	}
	if updated.QuarantineReason != quarantineReasonTerminalAuthFailure {
		t.Fatalf("QuarantineReason = %q, want %q (unchanged)", updated.QuarantineReason, quarantineReasonTerminalAuthFailure)
	}
	if !updated.QuarantinedAt.Equal(quarantinedAt) {
		t.Fatalf("QuarantinedAt = %v, want unchanged %v", updated.QuarantinedAt, quarantinedAt)
	}
}

// TestReconcileRegistryModelStates_DoesNotBlockRecoveryAfterQuarantineCleared
// verifies the new guard is keyed off the live AutoQuarantined flag and does
// not get in the way of the legitimate recovery path: once
// Auth.ClearAutoQuarantine() has run (e.g. via a completed re-auth or an
// explicit operator re-enable) and the clear has been written back through
// Manager.Update, a subsequent reconciliation pass must still normalize the
// auth back to Status=active/Unavailable=false.
func TestReconcileRegistryModelStates_DoesNotBlockRecoveryAfterQuarantineCleared(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	ctx := context.Background()
	authID := "reconcile-recovered-auth"
	model := "reconcile-recovered-model"

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(authID, "claude", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() {
		reg.UnregisterClient(authID)
	})

	if _, err := manager.Register(ctx, &Auth{
		ID:               authID,
		Provider:         "claude",
		ProxyURL:         "http://test-proxy:8080",
		Status:           StatusQuarantined,
		StatusMessage:    "auto_quarantined: repeated authentication failures, credential needs re-authentication",
		Unavailable:      true,
		AutoQuarantined:  true,
		QuarantineReason: quarantineReasonTerminalAuthFailure,
		QuarantinedAt:    time.Now().Add(-time.Minute),
		ModelStates: map[string]*ModelState{
			model: {
				Status:      StatusError,
				Unavailable: true,
				LastError:   &Error{Code: "unauthorized", Message: "authentication_error", HTTPStatus: 401},
			},
		},
	}); err != nil {
		t.Fatalf("register quarantined auth: %v", err)
	}

	// Simulate the sanctioned recovery path: a caller (re-auth save or
	// operator re-enable) fetches the current auth, clears the quarantine
	// lock, and writes it back through Manager.Update.
	recovered, ok := manager.GetByID(authID)
	if !ok || recovered == nil {
		t.Fatalf("GetByID(%q) not found before recovery", authID)
	}
	recovered.ClearAutoQuarantine()
	if _, err := manager.Update(ctx, recovered); err != nil {
		t.Fatalf("update cleared auth: %v", err)
	}

	manager.ReconcileRegistryModelStates(ctx, authID)

	updated, ok := manager.GetByID(authID)
	if !ok || updated == nil {
		t.Fatalf("GetByID(%q) not found after reconcile", authID)
	}
	if updated.AutoQuarantined {
		t.Fatalf("AutoQuarantined = true, want false after ClearAutoQuarantine")
	}
	if updated.Status != StatusActive {
		t.Fatalf("Status = %q, want %q (reconcile must still normalize a recovered auth)", updated.Status, StatusActive)
	}
	if updated.StatusMessage != "" {
		t.Fatalf("StatusMessage = %q, want cleared for a recovered auth", updated.StatusMessage)
	}
	if updated.Unavailable {
		t.Fatalf("Unavailable = true, want false for a recovered auth")
	}
	if updated.LastError != nil {
		t.Fatalf("LastError = %+v, want cleared for a recovered auth", updated.LastError)
	}
}

// TestReconcileRegistryModelStates_PreservesManuallyDisabledStatus guards the
// same class of drift for an operator-disabled auth: the reconciliation pass
// must not silently promote Status back to StatusActive just because the
// per-model error states happened to reset clean.
func TestReconcileRegistryModelStates_PreservesManuallyDisabledStatus(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	ctx := context.Background()
	authID := "reconcile-disabled-auth"
	model := "reconcile-disabled-model"

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(authID, "claude", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() {
		reg.UnregisterClient(authID)
	})

	if _, err := manager.Register(ctx, &Auth{
		ID:            authID,
		Provider:      "claude",
		ProxyURL:      "http://test-proxy:8080",
		Status:        StatusDisabled,
		StatusMessage: "disabled by operator",
		ModelStates: map[string]*ModelState{
			model: {
				Status:      StatusError,
				Unavailable: true,
				LastError:   &Error{Code: "unauthorized", Message: "authentication_error", HTTPStatus: 401},
			},
		},
	}); err != nil {
		t.Fatalf("register disabled auth: %v", err)
	}

	manager.ReconcileRegistryModelStates(ctx, authID)

	updated, ok := manager.GetByID(authID)
	if !ok || updated == nil {
		t.Fatalf("GetByID(%q) not found after reconcile", authID)
	}
	if updated.Status != StatusDisabled {
		t.Fatalf("Status = %q, want %q (reconcile must not flip a disabled auth back to active)", updated.Status, StatusDisabled)
	}
	if updated.StatusMessage != "disabled by operator" {
		t.Fatalf("StatusMessage = %q, want preserved %q", updated.StatusMessage, "disabled by operator")
	}
}
