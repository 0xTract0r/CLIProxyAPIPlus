package management

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// TestPatchAuthFileStatus_ReEnableClearsModelStatesCooldown verifies that
// re-enabling an auth via the management API clears any in-memory cooldown
// (including per-model ModelStates) so the auth becomes immediately
// selectable again instead of being stuck behind a stale 429 cooldown.
func TestPatchAuthFileStatus_ReEnableClearsModelStatesCooldown(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)

	future := time.Now().Add(30 * time.Minute)
	record := &coreauth.Auth{ProxyURL: "http://test-proxy:8080",
		ID:             "stuck.json",
		FileName:       "stuck.json",
		Provider:       "claude",
		Disabled:       true,
		Status:         coreauth.StatusDisabled,
		StatusMessage:  "disabled via management API",
		Unavailable:    true,
		NextRetryAfter: future,
		Quota: coreauth.QuotaState{
			Exceeded:      true,
			Reason:        "quota",
			NextRecoverAt: future,
		},
		LastError: &coreauth.Error{HTTPStatus: http.StatusTooManyRequests, Message: "quota"},
		ModelStates: map[string]*coreauth.ModelState{
			"foo": {
				Status:         coreauth.StatusError,
				Unavailable:    true,
				NextRetryAfter: future,
				Quota: coreauth.QuotaState{
					Exceeded:      true,
					Reason:        "quota",
					NextRecoverAt: future,
				},
				LastError: &coreauth.Error{HTTPStatus: http.StatusTooManyRequests, Message: "quota"},
			},
		},
	}
	if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
		t.Fatalf("failed to register auth record: %v", errRegister)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)

	body := `{"name":"stuck.json","disabled":false}`
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPatch, "/v0/management/auth-files/status", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ctx.Request = req
	h.PatchAuthFileStatus(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}

	updated, ok := manager.GetByID("stuck.json")
	if !ok || updated == nil {
		t.Fatalf("expected auth record to exist after patch")
	}

	if updated.Disabled {
		t.Fatalf("Disabled = true, want false")
	}
	if updated.Status != coreauth.StatusActive {
		t.Fatalf("Status = %q, want %q", updated.Status, coreauth.StatusActive)
	}
	if updated.Unavailable {
		t.Fatalf("Unavailable = true, want false")
	}
	if !updated.NextRetryAfter.IsZero() {
		t.Fatalf("NextRetryAfter = %v, want zero", updated.NextRetryAfter)
	}
	if updated.Quota.Exceeded {
		t.Fatalf("Quota.Exceeded = true, want false")
	}
	if updated.LastError != nil {
		t.Fatalf("LastError = %v, want nil", updated.LastError)
	}

	state, exists := updated.ModelStates["foo"]
	if !exists || state == nil {
		t.Fatalf("ModelStates[foo] = (%v, %v), want state", state, exists)
	}
	if state.Quota.Exceeded {
		t.Fatalf("ModelStates[foo].Quota.Exceeded = true, want false")
	}
	if !state.NextRetryAfter.IsZero() {
		t.Fatalf("ModelStates[foo].NextRetryAfter = %v, want zero", state.NextRetryAfter)
	}
	if state.Unavailable {
		t.Fatalf("ModelStates[foo].Unavailable = true, want false")
	}
	if state.LastError != nil {
		t.Fatalf("ModelStates[foo].LastError = %v, want nil", state.LastError)
	}
	if state.Status != coreauth.StatusActive {
		t.Fatalf("ModelStates[foo].Status = %q, want %q", state.Status, coreauth.StatusActive)
	}
}

// TestPatchAuthFileStatus_ReEnableClearsAutoQuarantine covers T3's second
// sanctioned recovery path: an explicit operator "not disabled" via
// PatchAuthFileStatus must lift the automatic terminal-auth quarantine lock
// (AutoQuarantined) exactly like a completed reauth does, and the account
// must become immediately selectable again.
func TestPatchAuthFileStatus_ReEnableClearsAutoQuarantine(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	record := &coreauth.Auth{ProxyURL: "http://test-proxy:8080",
		ID:       "quarantined.json",
		FileName: "quarantined.json",
		Provider: "claude",
	}
	if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
		t.Fatalf("failed to register auth record: %v", errRegister)
	}
	terminalAuthErr := &coreauth.Error{HTTPStatus: http.StatusUnauthorized, Message: `{"type":"error","error":{"type":"authentication_error","message":"OAuth access token has been revoked."}}`}
	manager.MarkResult(context.Background(), coreauth.Result{AuthID: "quarantined.json", Provider: "claude", Success: false, Error: terminalAuthErr})
	manager.MarkResult(context.Background(), coreauth.Result{AuthID: "quarantined.json", Provider: "claude", Success: false, Error: terminalAuthErr})
	quarantined, ok := manager.GetByID("quarantined.json")
	if !ok || quarantined == nil || !quarantined.AutoQuarantined {
		t.Fatalf("precondition failed: auth not quarantined, got=%+v ok=%v", quarantined, ok)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)

	body := `{"name":"quarantined.json","disabled":false}`
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPatch, "/v0/management/auth-files/status", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ctx.Request = req
	h.PatchAuthFileStatus(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}

	updated, ok := manager.GetByID("quarantined.json")
	if !ok || updated == nil {
		t.Fatalf("expected auth record to exist after patch")
	}
	if updated.AutoQuarantined {
		t.Fatalf("AutoQuarantined = true after explicit re-enable, want false")
	}
	if updated.QuarantineReason != "" {
		t.Fatalf("QuarantineReason = %q, want empty", updated.QuarantineReason)
	}
	if !updated.QuarantinedAt.IsZero() {
		t.Fatalf("QuarantinedAt = %v, want zero", updated.QuarantinedAt)
	}
	if updated.Status != coreauth.StatusActive {
		t.Fatalf("Status = %q, want %q", updated.Status, coreauth.StatusActive)
	}
}
