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
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	sdkauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestOrdinaryAccountSettingsPreservesOtherFields(t *testing.T) {
	dir := t.TempDir()
	name := "ordinary-local.json"
	path := filepath.Join(dir, name)
	initial := []byte(`{"type":"codex","email":"fixture@example.invalid","note":"old","priority":4,"legacy":{"count":9007199254740993}}`)
	if err := os.WriteFile(path, initial, 0600); err != nil {
		t.Fatal(err)
	}
	store := sdkauth.NewFileTokenStore()
	store.SetBaseDir(dir)
	manager := coreauth.NewManager(store, nil, nil)
	if err := manager.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: dir}, manager)
	h.tokenStore = store
	router := gin.New()
	router.GET("/auth-files/account-settings", h.GetOrdinaryAuthFileAccountSettings)
	router.PATCH("/auth-files/account-settings", h.PatchOrdinaryAuthFileAccountSettings)

	call := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		return w
	}

	res := call(http.MethodGet, "/auth-files/account-settings?name="+name, "")
	if res.Code != 200 || !strings.Contains(res.Body.String(), `"editable_fields":["note","disabled"]`) {
		t.Fatalf("settings GET %d: %s", res.Code, res.Body.String())
	}
	out := call(http.MethodPatch, "/auth-files/account-settings", `{"name":"`+name+`","note":"must not publish","unknown_setting":true}`)
	afterRejected, err := os.ReadFile(path)
	if out.Code != 400 || err != nil || string(afterRejected) != string(initial) {
		t.Fatalf("invalid edit changed source: %d %v", out.Code, err)
	}
	resp := call(http.MethodPatch, "/auth-files/account-settings", `{"name":"`+name+`","note":"updated","disabled":true}`)
	if resp.Code != 200 {
		t.Fatalf("ordinary update %d: %s", resp.Code, resp.Body.String())
	}
	raw, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(raw), "9007199254740993") {
		t.Fatalf("ordinary update changed unrelated data: %v", err)
	}
	var saved struct {
		Disabled bool
		Note     string
		Priority int
	}
	if err := json.Unmarshal(raw, &saved); err != nil || !saved.Disabled || saved.Note != "updated" || saved.Priority != 4 {
		t.Fatalf("unexpected saved values: %+v, error=%v", saved, err)
	}
}

func TestOrdinaryReadOnlyQuotaSnapshotFieldsAndDirectoryIsolation(t *testing.T) {
	makeHandler := func(name string, disabled bool, meta map[string]any) *Handler {
		t.Helper()
		m := coreauth.NewManager(nil, nil, nil)
		if _, err := m.Register(coreauth.WithSkipPersist(context.Background()), &coreauth.Auth{
			ID: name, FileName: name, Provider: "codex", Disabled: disabled, Metadata: meta,
		}); err != nil {
			t.Fatal(err)
		}
		return NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, m)
	}
	first := makeHandler("first.json", true, map[string]any{
		"refresh_disabled":        true,
		"quota_last_refreshed_at": "0001-01-01T00:00:00Z",
		"quota_refresh_error":     "synthetic-private-error",
		"quota_snapshot": map[string]any{
			"usage": map[string]any{
				"plan_type":                "plus",
				"internal_provider_marker": "synthetic-private-marker",
				"rate_limit": map[string]any{"primary_window": map[string]any{
					"used_percent": 22, "private_field": "synthetic-private-value",
				}},
			},
		},
	})
	second := makeHandler("second.json", false, map[string]any{"quota_refresh_status": "reauth_required", "quota_refresh_error": "synthetic-private-error"})

	for _, fixture := range []struct {
		h                   *Handler
		own, foreign, state string
	}{
		{first, "first.json", "second.json", "refresh_disabled"},
		{second, "second.json", "first.json", "reauth_required"},
	} {
		rec := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(rec)
		ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/quota/snapshots", nil)
		fixture.h.GetStoredQuotaSnapshots(ctx)
		var payload struct{ Entries []storedQuotaEntry }
		if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil || rec.Code != 200 || len(payload.Entries) != 1 ||
			payload.Entries[0].AuthID != fixture.own || payload.Entries[0].Status != fixture.state {
			t.Fatalf("read-only snapshot %d: %s, err=%v", rec.Code, rec.Body.String(), err)
		}
		raw := rec.Body.String()
		for _, secret := range []string{fixture.foreign, "synthetic-private", "0001-01-01", "last_refreshed_at", "next_refresh_at"} {
			if strings.Contains(raw, secret) {
				t.Fatalf("snapshot exposes %q", secret)
			}
		}
		if fixture.h == first && (!payload.Entries[0].Disabled || !strings.Contains(raw, `"used_percent":22`)) {
			t.Fatal("read-only snapshot lost disabled status or the display value")
		}
	}
}

func TestPatchAuthFileStatusEnablesOperatorSettingWithoutClearingServerWait(t *testing.T) {
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
	if !updated.Unavailable {
		t.Fatal("operator status update erased the existing unavailable signal")
	}
}

func TestOrdinaryCredentialListProjectsStoredWarningsWithoutPrivateData(t *testing.T) {
	dir := t.TempDir()
	const filename = "legacy-warning.json"
	const source = `{"type":"claude","email":"fixture@example.invalid","reauth_required":true,"auto_quarantined":true,"quarantine_reason":"synthetic-private-provider-body","quarantined_at":"2026-07-01T10:12:15Z"}`
	if err := os.WriteFile(filepath.Join(dir, filename), []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	store := sdkauth.NewFileTokenStore()
	store.SetBaseDir(dir)
	manager := coreauth.NewManager(store, nil, nil)
	if err := manager.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: dir}, manager)
	res := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(res)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/auth-files?name="+filename, nil)
	h.ListAuthFiles(ctx)
	body := res.Body.String()
	for _, want := range []string{`"reauth_required":true`, `"auto_quarantined":true`, `"quarantined_at":"2026-07-01T10:12:15Z"`} {
		if res.Code != 200 || !strings.Contains(body, want) {
			t.Fatalf("saved warning %s missing in status %d", want, res.Code)
		}
	}
	for _, forbidden := range []string{"synthetic-private-provider-body", "reauth_url"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("unexpected private or obsolete projection %s", forbidden)
		}
	}
}
