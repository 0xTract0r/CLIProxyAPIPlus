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

// TestValidateFarmAliveAtPatch covers the write-side guard for the container
// liveness heartbeat: valid RFC3339 and empty (clear) are accepted; malformed
// values and non-strings are rejected so a bad heartbeat can never be persisted.
func TestValidateFarmAliveAtPatch(t *testing.T) {
	ok := []any{"", "  ", "2026-08-22T12:00:00Z", "2026-08-22T12:00:00+08:00"}
	for _, v := range ok {
		if err := validateFarmAliveAtPatch(v); err != nil {
			t.Fatalf("validateFarmAliveAtPatch(%#v) = %v, want nil", v, err)
		}
	}
	bad := []any{"not-a-timestamp", "2026-08-22", "12:00:00", 12345, true, nil}
	for _, v := range bad {
		if err := validateFarmAliveAtPatch(v); err == nil {
			t.Fatalf("validateFarmAliveAtPatch(%#v) = nil, want error", v)
		}
	}
}

// TestSyncAuthFileContainerAliveAtAttribute_SetAndClear guards the immediate
// Metadata->Attributes mirror helper directly (independent of Manager.Update
// hydration): a valid persisted value is mirrored; a missing/empty/non-string
// value clears any stale mirror.
func TestSyncAuthFileContainerAliveAtAttribute_SetAndClear(t *testing.T) {
	// Set from a valid persisted metadata value.
	auth := &coreauth.Auth{
		Metadata: map[string]any{coreauth.FarmContainerAliveAtMetadataKey: " 2026-08-22T12:00:00Z "},
	}
	syncAuthFileContainerAliveAtAttribute(auth)
	if got := auth.Attributes[coreauth.FarmContainerAliveAtAttributeKey]; got != "2026-08-22T12:00:00Z" {
		t.Fatalf("attribute = %q, want trimmed heartbeat", got)
	}

	// Cleared metadata removes the stale mirror.
	auth.Metadata[coreauth.FarmContainerAliveAtMetadataKey] = ""
	syncAuthFileContainerAliveAtAttribute(auth)
	if _, ok := auth.Attributes[coreauth.FarmContainerAliveAtAttributeKey]; ok {
		t.Fatalf("expected attribute cleared for empty metadata, got %#v", auth.Attributes)
	}

	// Missing metadata key removes the stale mirror.
	auth2 := &coreauth.Auth{
		Metadata:   map[string]any{},
		Attributes: map[string]string{coreauth.FarmContainerAliveAtAttributeKey: "2026-08-22T12:00:00Z"},
	}
	syncAuthFileContainerAliveAtAttribute(auth2)
	if _, ok := auth2.Attributes[coreauth.FarmContainerAliveAtAttributeKey]; ok {
		t.Fatalf("expected attribute cleared for missing metadata, got %#v", auth2.Attributes)
	}

	// Non-string metadata removes the stale mirror.
	auth3 := &coreauth.Auth{
		Metadata:   map[string]any{coreauth.FarmContainerAliveAtMetadataKey: 12345},
		Attributes: map[string]string{coreauth.FarmContainerAliveAtAttributeKey: "2026-08-22T12:00:00Z"},
	}
	syncAuthFileContainerAliveAtAttribute(auth3)
	if _, ok := auth3.Attributes[coreauth.FarmContainerAliveAtAttributeKey]; ok {
		t.Fatalf("expected attribute cleared for non-string metadata, got %#v", auth3.Attributes)
	}
}

// TestPatchAuthFileFields_FarmAliveAtSetsAndClears is the end-to-end management
// PATCH path: writing farm_container_alive_at persists it to Metadata and makes
// it visible in the live Attributes mirror the gate reads; PATCHing an empty
// string clears both.
func TestPatchAuthFileFields_FarmAliveAtSetsAndClears(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	record := &coreauth.Auth{
		ID:       "farm-alive.json",
		FileName: "farm-alive.json",
		Provider: "claude",
		Attributes: map[string]string{
			"path": "/tmp/farm-alive.json",
		},
		Metadata: map[string]any{
			"type":                           "claude",
			coreauth.FarmEnrolledMetadataKey: true,
		},
	}
	if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
		t.Fatalf("failed to register auth record: %v", errRegister)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)

	heartbeat := time.Now().UTC().Format(time.RFC3339)
	patch(t, h, `{"name":"farm-alive.json","farm_container_alive_at":"`+heartbeat+`"}`, http.StatusOK)

	updated, ok := manager.GetByID("farm-alive.json")
	if !ok || updated == nil {
		t.Fatalf("expected auth record to exist after patch")
	}
	if got, _ := updated.Metadata[coreauth.FarmContainerAliveAtMetadataKey].(string); got != heartbeat {
		t.Fatalf("metadata.farm_container_alive_at = %q, want %q", got, heartbeat)
	}
	if got := updated.Attributes[coreauth.FarmContainerAliveAtAttributeKey]; got != heartbeat {
		t.Fatalf("attributes.farm_container_alive_at = %q, want %q (must be mirrored immediately for the gate)", got, heartbeat)
	}

	// Clearing the heartbeat removes the live attribute.
	patch(t, h, `{"name":"farm-alive.json","farm_container_alive_at":""}`, http.StatusOK)
	cleared, _ := manager.GetByID("farm-alive.json")
	if _, ok := cleared.Attributes[coreauth.FarmContainerAliveAtAttributeKey]; ok {
		t.Fatalf("expected attributes.farm_container_alive_at cleared, got %#v", cleared.Attributes[coreauth.FarmContainerAliveAtAttributeKey])
	}
}

// TestPatchAuthFileFields_FarmAliveAtRejectsInvalid confirms a malformed
// heartbeat is rejected with 400 before it is ever persisted.
func TestPatchAuthFileFields_FarmAliveAtRejectsInvalid(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	record := &coreauth.Auth{
		ID:         "farm-alive-bad.json",
		FileName:   "farm-alive-bad.json",
		Provider:   "claude",
		Attributes: map[string]string{"path": "/tmp/farm-alive-bad.json"},
		Metadata:   map[string]any{"type": "claude"},
	}
	if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
		t.Fatalf("failed to register auth record: %v", errRegister)
	}
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)

	patch(t, h, `{"name":"farm-alive-bad.json","farm_container_alive_at":"not-a-timestamp"}`, http.StatusBadRequest)

	updated, _ := manager.GetByID("farm-alive-bad.json")
	if _, ok := updated.Metadata[coreauth.FarmContainerAliveAtMetadataKey]; ok {
		t.Fatalf("expected invalid heartbeat NOT persisted, got %#v", updated.Metadata[coreauth.FarmContainerAliveAtMetadataKey])
	}
}

// patch drives PatchAuthFileFields with the given JSON body and asserts the
// resulting HTTP status.
func patch(t *testing.T, h *Handler, body string, wantStatus int) {
	t.Helper()
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPatch, "/v0/management/auth-files/fields", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ctx.Request = req
	h.PatchAuthFileFields(ctx)
	if rec.Code != wantStatus {
		t.Fatalf("PatchAuthFileFields status = %d, want %d (body: %s)", rec.Code, wantStatus, rec.Body.String())
	}
}
