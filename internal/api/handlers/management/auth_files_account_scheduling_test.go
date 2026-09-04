package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// newAccountSchedulingTestHandler registers a single Claude auth record and
// returns a management handler wired to it, mirroring the account-settings test
// harness (memoryAuthStore + coreauth.NewManager + NewHandlerWithoutConfigFilePath).
func newAccountSchedulingTestHandler(t *testing.T, provider string) (*Handler, *coreauth.Manager) {
	t.Helper()
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	record := &coreauth.Auth{
		ID:       "acct.json",
		FileName: "acct.json",
		Provider: provider,
		Attributes: map[string]string{
			"path": "/tmp/acct.json",
		},
		Metadata: map[string]any{
			"type": provider,
			// A recognized rate_limit_tier so the projection's subscription_tier is
			// meaningful for the "clear -> auto" assertions.
			"quota_snapshot": map[string]any{
				"profile": map[string]any{
					"organization": map[string]any{"rate_limit_tier": "default_claude_max_20x"},
				},
			},
		},
	}
	if _, err := manager.Register(context.Background(), record); err != nil {
		t.Fatalf("failed to register auth record: %v", err)
	}
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	return h, manager
}

func patchAccountScheduling(t *testing.T, h *Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPatch, "/v0/management/auth-files/account-scheduling", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ctx.Request = req
	h.PatchAuthFileAccountScheduling(ctx)
	return rec
}

// decodeSchedulingResponse pulls the account_scheduling projection out of the
// PATCH response body.
func decodeSchedulingResponse(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var resp struct {
		Name              string         `json:"name"`
		AccountScheduling map[string]any `json:"account_scheduling"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v; body=%s", err, rec.Body.String())
	}
	if resp.AccountScheduling == nil {
		t.Fatalf("account_scheduling missing from response: %s", rec.Body.String())
	}
	return resp.AccountScheduling
}

func TestPatchAuthFileAccountScheduling_SetTierOverrideLegal(t *testing.T) {
	h, manager := newAccountSchedulingTestHandler(t, "claude")

	rec := patchAccountScheduling(t, h, `{"name":"acct.json","tier_override":"max_5x"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	view := decodeSchedulingResponse(t, rec)
	if view["tier_source"] != "override" {
		t.Fatalf("tier_source = %v, want override", view["tier_source"])
	}
	if view["subscription_tier"] != "max_5x" {
		t.Fatalf("subscription_tier = %v, want max_5x", view["subscription_tier"])
	}

	// Persisted metadata reflects the namespaced write.
	updated, ok := manager.GetByID("acct.json")
	if !ok || updated == nil {
		t.Fatalf("auth record missing after update")
	}
	obj, ok := updated.Metadata[coreauth.AccountSchedulingMetadataKey].(map[string]any)
	if !ok {
		t.Fatalf("account_scheduling object not persisted: %#v", updated.Metadata)
	}
	if obj[coreauth.TierOverrideMetadataKey] != "max_5x" {
		t.Fatalf("tier_override not persisted: %#v", obj)
	}
}

func TestPatchAuthFileAccountScheduling_SetTierOverrideIllegal(t *testing.T) {
	h, _ := newAccountSchedulingTestHandler(t, "claude")

	rec := patchAccountScheduling(t, h, `{"name":"acct.json","tier_override":"codex_pro"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "invalid tier_override") {
		t.Fatalf("body = %q, want invalid tier_override", rec.Body.String())
	}
	// The error should advertise the legal values for the account's provider.
	if !strings.Contains(rec.Body.String(), "max_5x") {
		t.Fatalf("body = %q, want legal_values listing max_5x", rec.Body.String())
	}
}

func TestPatchAuthFileAccountScheduling_SetRateScaleLegal(t *testing.T) {
	h, manager := newAccountSchedulingTestHandler(t, "claude")

	rec := patchAccountScheduling(t, h, `{"name":"acct.json","rate_scale":0.5}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	view := decodeSchedulingResponse(t, rec)
	// JSON numbers decode to float64.
	if got, ok := view["rate_scale"].(float64); !ok || got != 0.5 {
		t.Fatalf("rate_scale = %v (%T), want 0.5", view["rate_scale"], view["rate_scale"])
	}

	updated, _ := manager.GetByID("acct.json")
	if got := coreauth.AccountRateScale(updated, config.AccountSchedulingConfig{}); got != 0.5 {
		t.Fatalf("AccountRateScale after set = %v, want 0.5", got)
	}
}

func TestPatchAuthFileAccountScheduling_SetRateScaleNonPositive(t *testing.T) {
	h, _ := newAccountSchedulingTestHandler(t, "claude")

	rec := patchAccountScheduling(t, h, `{"name":"acct.json","rate_scale":0}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "greater than 0") {
		t.Fatalf("body = %q, want rate_scale must be greater than 0", rec.Body.String())
	}
}

func TestPatchAuthFileAccountScheduling_ClearTierOverrideEmptyString(t *testing.T) {
	h, manager := newAccountSchedulingTestHandler(t, "claude")

	// Set first, then clear with an explicit empty string.
	if rec := patchAccountScheduling(t, h, `{"name":"acct.json","tier_override":"max_5x"}`); rec.Code != http.StatusOK {
		t.Fatalf("setup set failed: %d %s", rec.Code, rec.Body.String())
	}
	rec := patchAccountScheduling(t, h, `{"name":"acct.json","tier_override":""}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	view := decodeSchedulingResponse(t, rec)
	if view["tier_source"] != "auto" {
		t.Fatalf("tier_source after clear = %v, want auto", view["tier_source"])
	}
	// With override cleared the projection falls back to the auto-detected tier.
	if view["subscription_tier"] != "max_20x" {
		t.Fatalf("subscription_tier after clear = %v, want auto-detected max_20x", view["subscription_tier"])
	}

	updated, _ := manager.GetByID("acct.json")
	if obj, ok := updated.Metadata[coreauth.AccountSchedulingMetadataKey].(map[string]any); ok {
		if _, present := obj[coreauth.TierOverrideMetadataKey]; present {
			t.Fatalf("tier_override still present after clear: %#v", obj)
		}
	}
}

func TestPatchAuthFileAccountScheduling_ClearRateScaleNull(t *testing.T) {
	h, manager := newAccountSchedulingTestHandler(t, "claude")

	if rec := patchAccountScheduling(t, h, `{"name":"acct.json","rate_scale":0.25}`); rec.Code != http.StatusOK {
		t.Fatalf("setup set failed: %d %s", rec.Code, rec.Body.String())
	}
	rec := patchAccountScheduling(t, h, `{"name":"acct.json","rate_scale":null}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	view := decodeSchedulingResponse(t, rec)
	if got, ok := view["rate_scale"].(float64); !ok || got != 1.0 {
		t.Fatalf("rate_scale after clear = %v (%T), want default 1.0", view["rate_scale"], view["rate_scale"])
	}

	updated, _ := manager.GetByID("acct.json")
	if got := coreauth.AccountRateScale(updated, config.AccountSchedulingConfig{}); got != 1.0 {
		t.Fatalf("AccountRateScale after clear = %v, want 1.0", got)
	}
}

func TestPatchAuthFileAccountScheduling_SetBothInOneRequest(t *testing.T) {
	h, manager := newAccountSchedulingTestHandler(t, "claude")

	rec := patchAccountScheduling(t, h, `{"name":"acct.json","tier_override":"pro","rate_scale":2}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	view := decodeSchedulingResponse(t, rec)
	if view["subscription_tier"] != "pro" {
		t.Fatalf("subscription_tier = %v, want pro", view["subscription_tier"])
	}
	if got, ok := view["rate_scale"].(float64); !ok || got != 2.0 {
		t.Fatalf("rate_scale = %v, want 2.0", view["rate_scale"])
	}

	updated, _ := manager.GetByID("acct.json")
	obj := updated.Metadata[coreauth.AccountSchedulingMetadataKey].(map[string]any)
	if obj[coreauth.TierOverrideMetadataKey] != "pro" {
		t.Fatalf("tier_override not persisted: %#v", obj)
	}
	if coreauth.AccountRateScale(updated, config.AccountSchedulingConfig{}) != 2.0 {
		t.Fatalf("rate_scale not persisted: %#v", obj)
	}
}

func TestPatchAuthFileAccountScheduling_MissingName(t *testing.T) {
	h, _ := newAccountSchedulingTestHandler(t, "claude")
	rec := patchAccountScheduling(t, h, `{"tier_override":"pro"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "name is required") {
		t.Fatalf("body = %q, want name is required", rec.Body.String())
	}
}

func TestPatchAuthFileAccountScheduling_NoFields(t *testing.T) {
	h, _ := newAccountSchedulingTestHandler(t, "claude")
	rec := patchAccountScheduling(t, h, `{"name":"acct.json"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "at least one of tier_override or rate_scale") {
		t.Fatalf("body = %q, want no-fields error", rec.Body.String())
	}
}

func TestPatchAuthFileAccountScheduling_NotFound(t *testing.T) {
	h, _ := newAccountSchedulingTestHandler(t, "claude")
	rec := patchAccountScheduling(t, h, `{"name":"missing.json","tier_override":"pro"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}
