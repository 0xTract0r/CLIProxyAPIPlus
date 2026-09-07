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

// newFirstProductionAtCurveHandler mirrors newAccountSchedulingTestHandler but
// wires the DEFAULT warm-up curve (config.DefaultAccountSchedulingConfig) instead
// of a zero-value scheduling config. That distinction matters here: with an empty
// curve AccountWarmupStageForAge resolves EVERY anchored account to "mature", so
// the warm-up projection would be trivially mature regardless of the anchor. The
// default curve is what makes "set a 90-day anchor -> mature" and "set a 3-day
// anchor -> w1, not mature" meaningful assertions that the operator-set anchor is
// actually flowing into the derived warm-up stage.
func newFirstProductionAtCurveHandler(t *testing.T) (*Handler, *coreauth.Manager) {
	t.Helper()
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	record := &coreauth.Auth{
		ID:         "acct.json",
		FileName:   "acct.json",
		Provider:   "claude",
		Attributes: map[string]string{"path": "/tmp/acct.json"},
		Metadata:   map[string]any{"type": "claude"},
	}
	if _, err := manager.Register(context.Background(), record); err != nil {
		t.Fatalf("failed to register auth record: %v", err)
	}
	h := NewHandlerWithoutConfigFilePath(&config.Config{
		AuthDir:           t.TempDir(),
		AccountScheduling: config.DefaultAccountSchedulingConfig(),
	}, manager)
	return h, manager
}

// namespacedFirstProductionAt pulls the persisted account_scheduling
// .first_production_at sub-key off a record, reporting whether it is present.
func namespacedFirstProductionAt(t *testing.T, auth *coreauth.Auth) (string, bool) {
	t.Helper()
	obj, ok := auth.Metadata[coreauth.AccountSchedulingMetadataKey].(map[string]any)
	if !ok {
		return "", false
	}
	raw, present := obj[coreauth.FirstProductionAtMetadataKey]
	if !present {
		return "", false
	}
	str, isStr := raw.(string)
	return str, isStr
}

func TestPatchAuthFileAccountScheduling_SetFirstProductionAtPastMature(t *testing.T) {
	h, manager := newFirstProductionAtCurveHandler(t)

	anchor := time.Now().Add(-90 * 24 * time.Hour).UTC().Format(time.RFC3339)
	rec := patchAccountScheduling(t, h, `{"name":"acct.json","first_production_at":"`+anchor+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	view := decodeSchedulingResponse(t, rec)

	// Projection reflects the operator-set anchor verbatim (UTC RFC3339).
	if got, ok := view["first_production_at"].(string); !ok || got != anchor {
		t.Fatalf("first_production_at = %#v, want %q", view["first_production_at"], anchor)
	}

	// A 90-day anchor is past the default curve's last stage (w7-8 ends at 60) ->
	// mature. This is driven by the anchor we just set, not the empty-curve
	// trivial-mature shortcut (see newFirstProductionAtCurveHandler). Values are
	// read back through the JSON round-trip, so nested objects are map[string]any
	// and numbers decode to float64.
	warmup, ok := view["warmup"].(map[string]any)
	if !ok {
		t.Fatalf("warmup = %#v, want map[string]any", view["warmup"])
	}
	if got := warmup["stage"]; got != "mature" {
		t.Fatalf("warmup.stage = %#v, want %q for a 90-day-old account", got, "mature")
	}
	if mature, ok := warmup["mature"].(bool); !ok || !mature {
		t.Fatalf("warmup.mature = %#v, want true for a 90-day-old account", warmup["mature"])
	}
	if age, ok := warmup["age_days"].(float64); !ok || age != 90 {
		t.Fatalf("warmup.age_days = %#v, want 90", warmup["age_days"])
	}

	// Persisted only to the namespaced location, matching the auto-mint shape.
	updated, ok := manager.GetByID("acct.json")
	if !ok || updated == nil {
		t.Fatalf("auth record missing after update")
	}
	if got, present := namespacedFirstProductionAt(t, updated); !present || got != anchor {
		t.Fatalf("namespaced first_production_at = %q present=%v, want %q", got, present, anchor)
	}
}

func TestPatchAuthFileAccountScheduling_SetFirstProductionAtRecentWarmupReflectsAnchor(t *testing.T) {
	h, _ := newFirstProductionAtCurveHandler(t)

	// A 3-day anchor lands inside the default curve's first stage (w1: [0,7)) and
	// is NOT mature. This proves the operator-set anchor drives the derived
	// warm-up stage rather than every anchored account collapsing to "mature".
	anchor := time.Now().Add(-3 * 24 * time.Hour).UTC().Format(time.RFC3339)
	rec := patchAccountScheduling(t, h, `{"name":"acct.json","first_production_at":"`+anchor+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	view := decodeSchedulingResponse(t, rec)

	warmup, ok := view["warmup"].(map[string]any)
	if !ok {
		t.Fatalf("warmup = %#v, want map[string]any", view["warmup"])
	}
	if got := warmup["stage"]; got != "w1" {
		t.Fatalf("warmup.stage = %#v, want %q for a 3-day-old account on the default curve", got, "w1")
	}
	if mature, ok := warmup["mature"].(bool); !ok || mature {
		t.Fatalf("warmup.mature = %#v, want false for a 3-day-old account", warmup["mature"])
	}
	if age, ok := warmup["age_days"].(float64); !ok || age != 3 {
		t.Fatalf("warmup.age_days = %#v, want 3", warmup["age_days"])
	}
}

func TestPatchAuthFileAccountScheduling_SetFirstProductionAtFutureRejected(t *testing.T) {
	h, manager := newAccountSchedulingTestHandler(t, "claude")

	future := time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339)
	rec := patchAccountScheduling(t, h, `{"name":"acct.json","first_production_at":"`+future+`"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "must not be in the future") {
		t.Fatalf("body = %q, want future-rejected error", rec.Body.String())
	}
	// Nothing was persisted.
	updated, _ := manager.GetByID("acct.json")
	if _, present := namespacedFirstProductionAt(t, updated); present {
		t.Fatalf("first_production_at should not be persisted after a rejected future value")
	}
}

func TestPatchAuthFileAccountScheduling_SetFirstProductionAtInvalidFormatRejected(t *testing.T) {
	h, _ := newAccountSchedulingTestHandler(t, "claude")

	rec := patchAccountScheduling(t, h, `{"name":"acct.json","first_production_at":"not-a-timestamp"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "valid RFC3339 timestamp") {
		t.Fatalf("body = %q, want RFC3339-format error", rec.Body.String())
	}
}

func TestPatchAuthFileAccountScheduling_SetFirstProductionAtNonStringRejected(t *testing.T) {
	h, _ := newAccountSchedulingTestHandler(t, "claude")

	// A JSON number is neither a clear intent (null/empty) nor a string timestamp.
	rec := patchAccountScheduling(t, h, `{"name":"acct.json","first_production_at":1700000000}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "RFC3339 timestamp string") {
		t.Fatalf("body = %q, want non-string error", rec.Body.String())
	}
}

func TestPatchAuthFileAccountScheduling_ClearFirstProductionAtReopensAutoMint(t *testing.T) {
	h, manager := newAccountSchedulingTestHandler(t, "claude")

	// Set an explicit anchor first.
	past := time.Now().Add(-30 * 24 * time.Hour).UTC().Format(time.RFC3339)
	if rec := patchAccountScheduling(t, h, `{"name":"acct.json","first_production_at":"`+past+`"}`); rec.Code != http.StatusOK {
		t.Fatalf("setup set failed: %d %s", rec.Code, rec.Body.String())
	}

	// Clear via explicit empty string.
	rec := patchAccountScheduling(t, h, `{"name":"acct.json","first_production_at":""}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	view := decodeSchedulingResponse(t, rec)
	if fp, present := view["first_production_at"]; !present {
		t.Fatalf("first_production_at key absent from projection, want present-and-null")
	} else if fp != nil {
		t.Fatalf("first_production_at = %#v after clear, want nil", fp)
	}

	updated, _ := manager.GetByID("acct.json")
	if _, ok := coreauth.AuthFirstProductionAt(updated); ok {
		t.Fatalf("anchor still readable after clear")
	}
	if _, present := namespacedFirstProductionAt(t, updated); present {
		t.Fatalf("namespaced first_production_at still present after clear")
	}

	// The append-only auto-mint path is re-opened: the next serving success can
	// mint a fresh anchor (minted=true), proving clear handed control back to it.
	now := time.Now()
	anchor, minted := coreauth.EnsureAuthFirstProductionAt(updated, now)
	if !minted {
		t.Fatalf("EnsureAuthFirstProductionAt minted=false after clear, want true (auto-mint re-opened)")
	}
	if anchor.Before(now.Add(-time.Minute)) {
		t.Fatalf("re-minted anchor %v is not fresh (want ~now)", anchor)
	}
}

func TestPatchAuthFileAccountScheduling_ClearFirstProductionAtViaNull(t *testing.T) {
	h, manager := newAccountSchedulingTestHandler(t, "claude")

	past := time.Now().Add(-30 * 24 * time.Hour).UTC().Format(time.RFC3339)
	if rec := patchAccountScheduling(t, h, `{"name":"acct.json","first_production_at":"`+past+`"}`); rec.Code != http.StatusOK {
		t.Fatalf("setup set failed: %d %s", rec.Code, rec.Body.String())
	}
	rec := patchAccountScheduling(t, h, `{"name":"acct.json","first_production_at":null}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	updated, _ := manager.GetByID("acct.json")
	if _, ok := coreauth.AuthFirstProductionAt(updated); ok {
		t.Fatalf("anchor still readable after null clear")
	}
}

func TestPatchAuthFileAccountScheduling_ExplicitSetHonoredByAutoMint(t *testing.T) {
	h, manager := newAccountSchedulingTestHandler(t, "claude")

	// Operator-set explicit anchor.
	past := time.Now().Add(-45 * 24 * time.Hour).UTC()
	pastStr := past.Format(time.RFC3339)
	if rec := patchAccountScheduling(t, h, `{"name":"acct.json","first_production_at":"`+pastStr+`"}`); rec.Code != http.StatusOK {
		t.Fatalf("set failed: %d %s", rec.Code, rec.Body.String())
	}

	updated, _ := manager.GetByID("acct.json")

	// The append-only auto-mint must NOT clobber an operator-set anchor: it sees a
	// present, parseable value and returns it unchanged with minted=false. This is
	// the proof that the explicit-set and auto-mint paths do not fight each other.
	anchor, minted := coreauth.EnsureAuthFirstProductionAt(updated, time.Now())
	if minted {
		t.Fatalf("EnsureAuthFirstProductionAt minted=true over an explicit anchor, want false")
	}
	if got := anchor.UTC().Format(time.RFC3339); got != pastStr {
		t.Fatalf("auto-mint returned %q, want the operator-set %q", got, pastStr)
	}
}

func TestPatchAuthFileAccountScheduling_FirstProductionAtAloneSatisfiesPresence(t *testing.T) {
	h, _ := newAccountSchedulingTestHandler(t, "claude")

	// first_production_at alone (no tier_override / rate_scale) must satisfy the
	// "at least one field" presence gate rather than tripping the no-fields 400.
	past := time.Now().Add(-10 * 24 * time.Hour).UTC().Format(time.RFC3339)
	rec := patchAccountScheduling(t, h, `{"name":"acct.json","first_production_at":"`+past+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

func TestPatchAuthFileAccountScheduling_SetFirstProductionAtWithTierOverride(t *testing.T) {
	h, manager := newAccountSchedulingTestHandler(t, "claude")

	past := time.Now().Add(-20 * 24 * time.Hour).UTC().Format(time.RFC3339)
	rec := patchAccountScheduling(t, h, `{"name":"acct.json","tier_override":"max_5x","first_production_at":"`+past+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	view := decodeSchedulingResponse(t, rec)
	if view["tier_source"] != "override" {
		t.Fatalf("tier_source = %v, want override", view["tier_source"])
	}
	if got, ok := view["first_production_at"].(string); !ok || got != past {
		t.Fatalf("first_production_at = %#v, want %q", view["first_production_at"], past)
	}

	// Both sub-keys coexist under the one namespaced object.
	updated, _ := manager.GetByID("acct.json")
	obj, ok := updated.Metadata[coreauth.AccountSchedulingMetadataKey].(map[string]any)
	if !ok {
		t.Fatalf("account_scheduling object not persisted: %#v", updated.Metadata)
	}
	if obj[coreauth.TierOverrideMetadataKey] != "max_5x" {
		t.Fatalf("tier_override not persisted alongside first_production_at: %#v", obj)
	}
	if obj[coreauth.FirstProductionAtMetadataKey] != past {
		t.Fatalf("first_production_at not persisted alongside tier_override: %#v", obj)
	}
}

func TestPatchAuthFileAccountScheduling_FirstProductionAtRequiresAuth(t *testing.T) {
	// Unlike the sibling tests above (which invoke the handler directly, past the
	// auth middleware), this one exercises the real admin-auth gate the route is
	// registered under. envSecret must be set BEFORE NewHandler reads
	// MANAGEMENT_PASSWORD.
	t.Setenv("MANAGEMENT_PASSWORD", "test-secret")
	gin.SetMode(gin.TestMode)

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	record := &coreauth.Auth{
		ID:         "acct.json",
		FileName:   "acct.json",
		Provider:   "claude",
		Attributes: map[string]string{"path": "/tmp/acct.json"},
		Metadata:   map[string]any{"type": "claude"},
	}
	if _, err := manager.Register(context.Background(), record); err != nil {
		t.Fatalf("failed to register auth record: %v", err)
	}
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)

	engine := gin.New()
	engine.PATCH("/v0/management/auth-files/account-scheduling", h.Middleware(), h.PatchAuthFileAccountScheduling)

	body := `{"name":"acct.json","first_production_at":"2020-01-01T00:00:00Z"}`

	t.Run("missing key is rejected", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPatch, "/v0/management/auth-files/account-scheduling", strings.NewReader(body))
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("Content-Type", "application/json")
		engine.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401; body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("valid key reaches the handler", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPatch, "/v0/management/auth-files/account-scheduling", strings.NewReader(body))
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Management-Key", "test-secret")
		engine.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 with valid key; body=%s", rec.Code, rec.Body.String())
		}
	})
}
