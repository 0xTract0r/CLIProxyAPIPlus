package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func newFarmAutoEnrollRecord(email string) *coreauth.Auth {
	id := "claude-" + email + ".json"
	return &coreauth.Auth{
		ID:       id,
		FileName: id,
		Provider: "claude",
		Metadata: map[string]any{
			"type":          "claude",
			"email":         email,
			"access_token":  "NEW_TOKEN",
			"refresh_token": "NEW_REFRESH",
		},
	}
}

// TestSaveTokenRecord_SkipsFarmAutoEnrollWhenSwitchDisabled asserts the H4 gate:
// with the global farm-auto-enroll switch off, a brand-new account's first
// authentication (previous == nil) must NOT be enrolled, leaving farm_enrolled
// absent so enrollment defaults to false (manual opt-in only).
func TestSaveTokenRecord_SkipsFarmAutoEnrollWhenSwitchDisabled(t *testing.T) {
	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	disabled := false
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir(), FarmAutoEnroll: &disabled}, manager)
	h.tokenStore = store

	record := newFarmAutoEnrollRecord("new-when-disabled@example.com")
	if _, errSave := h.saveTokenRecord(context.Background(), record); errSave != nil {
		t.Fatalf("saveTokenRecord returned error: %v", errSave)
	}

	if _, ok := record.Metadata[coreauth.FarmEnrolledMetadataKey]; ok {
		t.Fatalf("metadata[farm_enrolled] = %#v with farm-auto-enroll disabled, want key absent", record.Metadata[coreauth.FarmEnrolledMetadataKey])
	}
	if coreauth.AuthFarmEnrolled(record) {
		t.Fatal("AuthFarmEnrolled(record) = true with farm-auto-enroll disabled, want false")
	}
}

// TestSaveTokenRecord_AutoEnrollsFarmWhenSwitchExplicitlyEnabled asserts the
// enabled side of the H4 gate: an explicit farm-auto-enroll: true keeps the
// pre-toggle behavior (brand-new account enrolled on first auth). The nil
// (default-true) case is covered by TestSaveTokenRecord_AutoEnrollsFarmOnFirstAuth.
func TestSaveTokenRecord_AutoEnrollsFarmWhenSwitchExplicitlyEnabled(t *testing.T) {
	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	enabled := true
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir(), FarmAutoEnroll: &enabled}, manager)
	h.tokenStore = store

	record := newFarmAutoEnrollRecord("new-when-enabled@example.com")
	if _, errSave := h.saveTokenRecord(context.Background(), record); errSave != nil {
		t.Fatalf("saveTokenRecord returned error: %v", errSave)
	}
	if !coreauth.AuthFarmEnrolled(record) {
		t.Fatal("AuthFarmEnrolled(record) = false with farm-auto-enroll enabled, want true")
	}
}

// putFarmAutoEnroll drives the PUT endpoint with a raw JSON body and returns the
// gin status code so callers can assert both success and contract-violation paths.
func putFarmAutoEnroll(t *testing.T, h *Handler, body string) int {
	t.Helper()
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPut, "/v0/management/farm-auto-enroll", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ctx.Request = req
	h.PutFarmAutoEnroll(ctx)
	return rec.Code
}

// getFarmAutoEnroll drives the GET endpoint and returns the decoded {"value": bool}.
func getFarmAutoEnroll(t *testing.T, h *Handler) bool {
	t.Helper()
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/farm-auto-enroll", nil)
	h.GetFarmAutoEnroll(ctx)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, body %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Value bool `json:"value"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode GET response: %v", err)
	}
	return resp.Value
}

// TestFarmAutoEnrollEndpoint_GetPutRoundtripPersists asserts the fixed
// {"value": bool} GET/PUT contract and that PUT persists to config.yaml so the
// switch survives a fresh load from disk.
func TestFarmAutoEnrollEndpoint_GetPutRoundtripPersists(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("port: 8317\n"), 0o600); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	h := NewHandler(cfg, configPath, manager)

	// Default: the unset pointer normalizes to enabled.
	if !getFarmAutoEnroll(t, h) {
		t.Fatal("GET default = false, want true (unset switch is enabled)")
	}

	// PUT false disables it and persists to disk.
	if code := putFarmAutoEnroll(t, h, `{"value":false}`); code != http.StatusOK {
		t.Fatalf("PUT false status = %d, want 200", code)
	}
	if getFarmAutoEnroll(t, h) {
		t.Fatal("GET after PUT false = true, want false")
	}
	if reloaded, errLoad := config.LoadConfig(configPath); errLoad != nil {
		t.Fatalf("reload after PUT false: %v", errLoad)
	} else if config.FarmAutoEnrollEnabled(reloaded) {
		t.Fatal("persisted config still enabled after PUT false")
	}

	// PUT true re-enables it and persists to disk.
	if code := putFarmAutoEnroll(t, h, `{"value":true}`); code != http.StatusOK {
		t.Fatalf("PUT true status = %d, want 200", code)
	}
	if !getFarmAutoEnroll(t, h) {
		t.Fatal("GET after PUT true = false, want true")
	}
	if reloaded, errLoad := config.LoadConfig(configPath); errLoad != nil {
		t.Fatalf("reload after PUT true: %v", errLoad)
	} else if !config.FarmAutoEnrollEnabled(reloaded) {
		t.Fatal("persisted config not enabled after PUT true")
	}

	// A body missing the value field violates the {"value": bool} contract.
	if code := putFarmAutoEnroll(t, h, `{}`); code != http.StatusBadRequest {
		t.Fatalf("PUT with missing value status = %d, want %d", code, http.StatusBadRequest)
	}
}

// TestSaveTokenRecord_FarmAutoEnrollGateObservesHotReloadedConfigPointer is the
// open-question guard: after PUT flips the switch and the hot-reload path swaps
// h.cfg to a brand-new *config.Config parsed from disk (exactly what SetConfig
// does inside the reload hook), the saveTokenRecord gate must read the new
// (disabled) value rather than a stale enabled pointer.
func TestSaveTokenRecord_FarmAutoEnrollGateObservesHotReloadedConfigPointer(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("port: 8317\n"), 0o600); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	h := NewHandler(cfg, configPath, manager)
	h.tokenStore = store

	// PUT false: mutates the live h.cfg in place and persists to disk.
	if code := putFarmAutoEnroll(t, h, `{"value":false}`); code != http.StatusOK {
		t.Fatalf("PUT false status = %d, want 200", code)
	}

	// Simulate hot reload swapping h.cfg to a fresh pointer parsed from disk.
	reloaded, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("reload config from disk: %v", err)
	}
	if reloaded == cfg {
		t.Fatal("reloaded config shares the pointer with cfg; the swap is not exercised")
	}
	if config.FarmAutoEnrollEnabled(reloaded) {
		t.Fatal("reloaded-from-disk config is enabled; PUT did not persist the disable")
	}
	h.SetConfig(reloaded)

	record := newFarmAutoEnrollRecord("after-reload@example.com")
	if _, errSave := h.saveTokenRecord(context.Background(), record); errSave != nil {
		t.Fatalf("saveTokenRecord returned error: %v", errSave)
	}
	if coreauth.AuthFarmEnrolled(record) {
		t.Fatal("brand-new account enrolled after farm-auto-enroll disabled + hot reload, want not enrolled")
	}
}
