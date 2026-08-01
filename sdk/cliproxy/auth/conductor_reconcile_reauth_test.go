package auth

import (
	"context"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
)

// reauthLockMetadata mirrors the persisted keys markRefreshReauthRequiredWithReason
// writes for a terminally dead refresh token.
func reauthLockMetadata() map[string]any {
	return map[string]any{
		"type":                    "claude",
		"reauth_required":         true,
		"refresh_status":          "reauth_required",
		"refresh_error_code":      "invalid_grant",
		"refresh_disabled_reason": "reauth_required",
		"refresh_disabled":        true,
	}
}

// TestReconcileRegistryModelStates_PreservesReauthRequiredView guards against
// the periodic per-model reconciliation pass (triggered by registry model
// re-registration) silently flipping a terminal reauth_required auth's Status
// back to StatusActive/Unavailable=false while its reauth lock metadata is
// still set -- the false-green regression this fix closes. The selector still
// excludes the credential via isReauthRequiredMetadata, but the status readers
// (logs, management frontends) would otherwise misreport it as healthy.
func TestReconcileRegistryModelStates_PreservesReauthRequiredView(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	ctx := context.Background()
	authID := "reconcile-reauth-auth"
	model := "reconcile-reauth-model"

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(authID, "claude", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() {
		reg.UnregisterClient(authID)
	})

	if _, err := manager.Register(ctx, &Auth{
		ID:            authID,
		Provider:      "claude",
		ProxyURL:      "http://test-proxy:8080",
		Status:        StatusError,
		StatusMessage: "reauth_required",
		Unavailable:   true,
		Metadata:      reauthLockMetadata(),
		LastError:     &Error{Code: "reauth_required", Message: "refresh token is no longer valid", HTTPStatus: 401},
		ModelStates: map[string]*ModelState{
			// A stale, non-clean per-model state makes the reconciliation pass
			// observe changed=true and actually run its auth-level status sync.
			model: {
				Status:      StatusError,
				Unavailable: true,
				LastError:   &Error{Code: "unauthorized", Message: "authentication_error", HTTPStatus: 401},
			},
		},
	}); err != nil {
		t.Fatalf("register reauth auth: %v", err)
	}

	manager.ReconcileRegistryModelStates(ctx, authID)

	updated, ok := manager.GetByID(authID)
	if !ok || updated == nil {
		t.Fatalf("GetByID(%q) not found after reconcile", authID)
	}
	if updated.Status != StatusError {
		t.Fatalf("Status = %q, want %q (reconcile must not flip a reauth auth back to active)", updated.Status, StatusError)
	}
	if updated.StatusMessage != "reauth_required" {
		t.Fatalf("StatusMessage = %q, want preserved %q", updated.StatusMessage, "reauth_required")
	}
	if !updated.Unavailable {
		t.Fatalf("Unavailable = false, want true for a reauth auth")
	}
	if !updated.NextRetryAfter.IsZero() {
		t.Fatalf("NextRetryAfter = %v, want zero (reauth implies no scheduled retry)", updated.NextRetryAfter)
	}
}

// TestReconcileRegistryModelStates_DoesNotBlockRecoveryAfterReauthCleared
// verifies the guard is keyed off the live metadata lock and does not block the
// legitimate recovery path: once the reauth lock keys are removed (completed
// re-auth) and written back through Manager.Update, a subsequent reconciliation
// must still normalize the auth back to Status=active/Unavailable=false.
func TestReconcileRegistryModelStates_DoesNotBlockRecoveryAfterReauthCleared(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	ctx := context.Background()
	authID := "reconcile-reauth-recovered-auth"
	model := "reconcile-reauth-recovered-model"

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(authID, "claude", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() {
		reg.UnregisterClient(authID)
	})

	if _, err := manager.Register(ctx, &Auth{
		ID:            authID,
		Provider:      "claude",
		ProxyURL:      "http://test-proxy:8080",
		Status:        StatusError,
		StatusMessage: "reauth_required",
		Unavailable:   true,
		Metadata:      reauthLockMetadata(),
		ModelStates: map[string]*ModelState{
			model: {
				Status:      StatusError,
				Unavailable: true,
				LastError:   &Error{Code: "unauthorized", Message: "authentication_error", HTTPStatus: 401},
			},
		},
	}); err != nil {
		t.Fatalf("register reauth auth: %v", err)
	}

	// Simulate the sanctioned recovery path: fetch the current auth, drop the
	// reauth lock keys and reset the terminal status fields, and write it back
	// through Manager.Update (mirrors clearAuthReauthRequiredLock + save).
	recovered, ok := manager.GetByID(authID)
	if !ok || recovered == nil {
		t.Fatalf("GetByID(%q) not found before recovery", authID)
	}
	for _, k := range []string{"reauth_required", "refresh_status", "refresh_error_code", "refresh_disabled_reason", "refresh_disabled"} {
		delete(recovered.Metadata, k)
	}
	recovered.Metadata["access_token"] = "fresh-token"
	recovered.Status = StatusActive
	recovered.StatusMessage = ""
	recovered.Unavailable = false
	recovered.LastError = nil
	if _, err := manager.Update(ctx, recovered); err != nil {
		t.Fatalf("update cleared auth: %v", err)
	}

	manager.ReconcileRegistryModelStates(ctx, authID)

	updated, ok := manager.GetByID(authID)
	if !ok || updated == nil {
		t.Fatalf("GetByID(%q) not found after reconcile", authID)
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
}
