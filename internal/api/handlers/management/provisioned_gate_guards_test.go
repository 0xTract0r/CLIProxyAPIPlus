package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// enrolledUnprovisionedClaudeAuth builds a farm-enrolled Claude account with no
// real claude_device_id binding: the population the supply-atomicity gate
// fail-closes when FARM_REQUIRE_PROVISIONED is armed.
func enrolledUnprovisionedClaudeAuth(id string) *coreauth.Auth {
	return &coreauth.Auth{
		ID:       id,
		Provider: "claude",
		ProxyURL: "http://test-proxy:8080",
		Metadata: map[string]any{coreauth.FarmEnrolledMetadataKey: true},
	}
}

// TestRefreshDueQuotaSnapshots_ProvisionedGate covers the R5-3e quota-poller
// fail-closed gate. With the flag armed, an enrolled-but-unprovisioned Claude
// account is skipped (no api.anthropic.com /oauth/profile+usage probe), while an
// unenrolled "old" account in the same manager is still probed (immune). With the
// flag off, both are probed — byte-identical to today.
func TestRefreshDueQuotaSnapshots_ProvisionedGate(t *testing.T) {
	gin.SetMode(gin.TestMode)

	build := func(t *testing.T) (*Handler, *quotaSnapshotTestExecutor) {
		manager := coreauth.NewManager(nil, nil, nil)
		exec := &quotaSnapshotTestExecutor{provider: "claude"}
		manager.RegisterExecutor(exec)
		past := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
		enrolled := enrolledUnprovisionedClaudeAuth("claude-enrolled-unprov")
		enrolled.Metadata[quotaNextRefreshMetadataKey] = past
		old := &coreauth.Auth{
			ID:       "claude-old",
			Provider: "claude",
			ProxyURL: "http://test-proxy:8080",
			Metadata: map[string]any{quotaNextRefreshMetadataKey: past},
		}
		for _, a := range []*coreauth.Auth{enrolled, old} {
			if _, err := manager.Register(context.Background(), a); err != nil {
				t.Fatalf("register %s: %v", a.ID, err)
			}
		}
		return NewHandlerWithoutConfigFilePath(nil, manager), exec
	}

	t.Run("flag on: enrolled+unprovisioned skipped, old account probed", func(t *testing.T) {
		t.Setenv(coreauth.FarmRequireProvisionedEnvVar, "1")
		h, exec := build(t)
		h.refreshDueQuotaSnapshots(context.Background(), defaultQuotaSnapshotTestPolicy(), false)
		if got := exec.CallsForAuth("claude-enrolled-unprov"); got != 0 {
			t.Fatalf("enrolled+unprovisioned probed %d times, want 0 (fail-closed skip)", got)
		}
		if got := exec.CallsForAuth("claude-old"); got == 0 {
			t.Fatalf("old/unenrolled account was skipped; want probed (immune)")
		}
	})

	t.Run("flag off: both probed (byte-identical no-op)", func(t *testing.T) {
		t.Setenv(coreauth.FarmRequireProvisionedEnvVar, "")
		h, exec := build(t)
		h.refreshDueQuotaSnapshots(context.Background(), defaultQuotaSnapshotTestPolicy(), false)
		if got := exec.CallsForAuth("claude-enrolled-unprov"); got == 0 {
			t.Fatalf("flag off: enrolled+unprovisioned was skipped; want probed (no-op)")
		}
		if got := exec.CallsForAuth("claude-old"); got == 0 {
			t.Fatalf("flag off: old account was skipped; want probed")
		}
	})
}

// TestRefreshQuotaSnapshots_ProvisionedGate covers the R5-3e explicit-refresh
// chokepoint fix: the operator/frontend-triggerable POST /quota/refresh endpoint
// (RefreshQuotaSnapshots -> quotaRefreshTargets -> refreshQuotaSnapshotResult ->
// refreshQuotaSnapshot -> fetchProviderQuotaSnapshot) previously had NO gate, so
// with the flag armed it still issued GET /api/oauth/profile+usage to
// api.anthropic.com for an enrolled-but-unprovisioned Claude account. This asserts
// the endpoint no longer egresses for that population (both the global and the
// explicit by-id trigger modes) while every immune population (old/unenrolled,
// non-Claude, provisioned) and the flag-off no-op stay byte-identical.
func TestRefreshQuotaSnapshots_ProvisionedGate(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// validProvisionedDeviceID mirrors sdk/cliproxy/auth's provisioned-binding
	// fixture: a well-formed 64-hex claude_device_id override marking a real
	// container binding, which the gate must treat as servable (immune).
	validProvisionedDeviceID := strings.Repeat("a", 64)

	newManager := func(t *testing.T, provider string) (*coreauth.Manager, *quotaSnapshotTestExecutor) {
		t.Helper()
		manager := coreauth.NewManager(nil, nil, nil)
		exec := &quotaSnapshotTestExecutor{provider: provider}
		manager.RegisterExecutor(exec)
		return manager, exec
	}

	register := func(t *testing.T, manager *coreauth.Manager, auths ...*coreauth.Auth) {
		t.Helper()
		for _, a := range auths {
			if _, err := manager.Register(context.Background(), a); err != nil {
				t.Fatalf("register %s: %v", a.ID, err)
			}
		}
	}

	// postRefresh drives the real HTTP endpoint so the full quotaRefreshTargets ->
	// refreshQuotaSnapshotResult -> refreshQuotaSnapshot path is exercised. body ==
	// "" triggers the global/provider-wide refresh; a JSON object targets by id.
	postRefresh := func(t *testing.T, manager *coreauth.Manager, body string) *httptest.ResponseRecorder {
		t.Helper()
		h := NewHandlerWithoutConfigFilePath(nil, manager)
		router := gin.New()
		router.POST("/v0/management/quota/refresh", h.RefreshQuotaSnapshots)
		payload := body
		if payload == "" {
			payload = "{}"
		}
		req := httptest.NewRequest(http.MethodPost, "/v0/management/quota/refresh", strings.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	type refreshResultView struct {
		AuthID     string `json:"auth_id"`
		Status     string `json:"status"`
		ErrorClass string `json:"error_class"`
		Refreshed  bool   `json:"refreshed"`
	}
	resultFor := func(t *testing.T, rec *httptest.ResponseRecorder, authID string) (refreshResultView, bool) {
		t.Helper()
		var payload struct {
			RefreshResults []refreshResultView `json:"refresh_results"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
			t.Fatalf("decode refresh payload: %v (body=%s)", err, rec.Body.String())
		}
		for _, r := range payload.RefreshResults {
			if r.AuthID == authID {
				return r, true
			}
		}
		return refreshResultView{}, false
	}

	t.Run("flag on: global refresh skips enrolled+unprovisioned, probes old account", func(t *testing.T) {
		t.Setenv(coreauth.FarmRequireProvisionedEnvVar, "1")
		manager, exec := newManager(t, "claude")
		enrolled := enrolledUnprovisionedClaudeAuth("claude-enrolled-unprov")
		old := &coreauth.Auth{ID: "claude-old", Provider: "claude", ProxyURL: "http://test-proxy:8080"}
		register(t, manager, enrolled, old)

		rec := postRefresh(t, manager, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
		}
		if got := exec.CallsForAuth("claude-enrolled-unprov"); got != 0 {
			t.Fatalf("enrolled+unprovisioned probed %d times via explicit endpoint, want 0 (fail-closed)", got)
		}
		if got := exec.CallsForAuth("claude-old"); got == 0 {
			t.Fatalf("old/unenrolled account was skipped; want probed (immune)")
		}
		// The blocked account is reported as a deliberate skip, not a provider error.
		if r, ok := resultFor(t, rec, "claude-enrolled-unprov"); ok {
			if r.Refreshed {
				t.Fatalf("blocked account refreshed=true, want false")
			}
			if r.ErrorClass != "provisioning_blocked" {
				t.Fatalf("blocked account error_class=%q, want provisioning_blocked", r.ErrorClass)
			}
		}
	})

	t.Run("flag on: explicit by-id refresh skips enrolled+unprovisioned", func(t *testing.T) {
		t.Setenv(coreauth.FarmRequireProvisionedEnvVar, "1")
		manager, exec := newManager(t, "claude")
		register(t, manager, enrolledUnprovisionedClaudeAuth("claude-enrolled-unprov"))

		rec := postRefresh(t, manager, `{"auth_id":"claude-enrolled-unprov"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
		}
		if got := exec.CallsForAuth("claude-enrolled-unprov"); got != 0 {
			t.Fatalf("by-id enrolled+unprovisioned probed %d times, want 0 (fail-closed)", got)
		}
		if r, ok := resultFor(t, rec, "claude-enrolled-unprov"); !ok {
			t.Fatalf("by-id refresh returned no result for the target account")
		} else if r.Refreshed || r.ErrorClass != "provisioning_blocked" {
			t.Fatalf("by-id blocked result = %+v, want refreshed=false error_class=provisioning_blocked", r)
		}
	})

	t.Run("flag on: enrolled+provisioned Claude is probed (immune)", func(t *testing.T) {
		t.Setenv(coreauth.FarmRequireProvisionedEnvVar, "1")
		manager, exec := newManager(t, "claude")
		provisioned := enrolledUnprovisionedClaudeAuth("claude-enrolled-prov")
		// A real container-synced binding is persisted in Metadata; Manager
		// hydration (applyClaudeDeviceIDFromMetadata) mirrors it into Attributes,
		// which is the exact field the gate predicate reads. Setting Attributes
		// directly would be wiped by that same hydration.
		provisioned.Metadata[coreauth.ClaudeDeviceIDMetadataKey] = validProvisionedDeviceID
		register(t, manager, provisioned)

		postRefresh(t, manager, `{"auth_id":"claude-enrolled-prov"}`)
		if got := exec.CallsForAuth("claude-enrolled-prov"); got == 0 {
			t.Fatalf("enrolled+provisioned account was skipped; want probed (a real device binding is immune)")
		}
	})

	t.Run("flag on: non-Claude enrolled account is probed (immune)", func(t *testing.T) {
		t.Setenv(coreauth.FarmRequireProvisionedEnvVar, "1")
		manager, exec := newManager(t, "codex")
		codex := &coreauth.Auth{ID: "codex-enrolled", Provider: "codex", ProxyURL: "http://test-proxy:8080",
			Metadata: map[string]any{coreauth.FarmEnrolledMetadataKey: true}}
		register(t, manager, codex)

		postRefresh(t, manager, `{"auth_id":"codex-enrolled"}`)
		if got := exec.CallsForAuth("codex-enrolled"); got == 0 {
			t.Fatalf("non-Claude account was skipped; want probed (gate is Claude-scoped)")
		}
	})

	t.Run("flag off: enrolled+unprovisioned is probed (byte-identical no-op)", func(t *testing.T) {
		t.Setenv(coreauth.FarmRequireProvisionedEnvVar, "")
		manager, exec := newManager(t, "claude")
		register(t, manager, enrolledUnprovisionedClaudeAuth("claude-enrolled-unprov"))

		postRefresh(t, manager, `{"auth_id":"claude-enrolled-unprov"}`)
		if got := exec.CallsForAuth("claude-enrolled-unprov"); got == 0 {
			t.Fatalf("flag off: enrolled+unprovisioned was skipped; want probed (no-op)")
		}
	})
}

// TestAPICall_ProvisionedGate covers the R5-3b api-call precheck. With the flag
// armed, an api-call targeting an enrolled-but-unprovisioned Claude account is
// rejected with a fail-closed 400 before any outbound request is built. Every
// immune case (flag off, unenrolled old account, non-Claude) must NOT receive
// that fail-closed 400.
func TestAPICall_ProvisionedGate(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// A live local server so non-gated cases fail on transport (not on the gate),
	// keeping the negative assertions deterministic and network-free of real hosts.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	doAPICall := func(t *testing.T, auth *coreauth.Auth) *httptest.ResponseRecorder {
		t.Helper()
		manager := coreauth.NewManager(nil, nil, nil)
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("register auth: %v", err)
		}
		idx := auth.EnsureIndex()
		h := &Handler{authManager: manager, cfg: &config.Config{}}
		router := gin.New()
		router.POST("/v0/management/api-call", h.APICall)
		body, err := json.Marshal(map[string]any{"method": "GET", "url": upstream.URL, "auth_index": idx})
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		req := httptest.NewRequest(http.MethodPost, "/v0/management/api-call", strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	isFailClosed := func(rec *httptest.ResponseRecorder) bool {
		return rec.Code == http.StatusBadRequest && strings.Contains(rec.Body.String(), "not provisioned")
	}

	t.Run("flag on: enrolled+unprovisioned Claude is rejected 400 (fail-closed)", func(t *testing.T) {
		t.Setenv(coreauth.FarmRequireProvisionedEnvVar, "1")
		rec := doAPICall(t, enrolledUnprovisionedClaudeAuth("claude-enrolled-unprov"))
		if !isFailClosed(rec) {
			t.Fatalf("want fail-closed 400 with 'not provisioned'; got code=%d body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("flag off: enrolled+unprovisioned Claude not fail-closed (no-op)", func(t *testing.T) {
		t.Setenv(coreauth.FarmRequireProvisionedEnvVar, "")
		rec := doAPICall(t, enrolledUnprovisionedClaudeAuth("claude-enrolled-unprov"))
		if isFailClosed(rec) {
			t.Fatalf("flag off must be a no-op; got fail-closed 400 body=%s", rec.Body.String())
		}
	})

	t.Run("flag on: unenrolled old Claude account is immune", func(t *testing.T) {
		t.Setenv(coreauth.FarmRequireProvisionedEnvVar, "1")
		old := &coreauth.Auth{ID: "claude-old", Provider: "claude", ProxyURL: "http://test-proxy:8080"}
		rec := doAPICall(t, old)
		if isFailClosed(rec) {
			t.Fatalf("old/unenrolled account must be immune; got fail-closed 400 body=%s", rec.Body.String())
		}
	})

	t.Run("flag on: non-Claude account is immune", func(t *testing.T) {
		t.Setenv(coreauth.FarmRequireProvisionedEnvVar, "1")
		codex := &coreauth.Auth{ID: "codex-acct", Provider: "codex", ProxyURL: "http://test-proxy:8080",
			Metadata: map[string]any{coreauth.FarmEnrolledMetadataKey: true}}
		rec := doAPICall(t, codex)
		if isFailClosed(rec) {
			t.Fatalf("non-Claude account must be immune; got fail-closed 400 body=%s", rec.Body.String())
		}
	})
}
