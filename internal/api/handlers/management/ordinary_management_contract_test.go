package management

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/claude"
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
	var settings struct {
		AccountSettings ordinaryAccountSettingsView `json:"account_settings"`
	}
	if res.Code != http.StatusOK || json.Unmarshal(res.Body.Bytes(), &settings) != nil ||
		!slices.Equal(settings.AccountSettings.EditableFields, []string{"note", "disabled", "refresh_enabled"}) {
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
	const source = `{"type":"claude","email":"fixture@example.invalid","account_uuid":"fixture-subject","reauth_url":"https://foreign.example.invalid/synthetic-private-url","reauth_required":true,"auto_quarantined":true,"quarantine_reason":"synthetic-private-provider-body","quarantined_at":"2026-07-01T10:12:15Z"}`
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
	for _, want := range []string{`"reauth_required":true`, `"reauth_url":"/v0/management/anthropic-auth-url?auth_name=legacy-warning.json"`, `"auto_quarantined":true`, `"quarantined_at":"2026-07-01T10:12:15Z"`} {
		if res.Code != 200 || !strings.Contains(body, want) {
			t.Fatalf("saved warning %s missing in status %d", want, res.Code)
		}
	}
	for _, forbidden := range []string{"synthetic-private-provider-body", "synthetic-private-url"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("unexpected private or obsolete projection %s", forbidden)
		}
	}
}

const (
	fixtureClaudeAccountUUID = "ab12cdef-1234-4abc-8def-abcdefabcdef"
	fixtureClaudeOrgUUID     = "ed34abcd-1234-4def-8abc-abcdefabcdef"
	fixtureOtherUUID         = "0012cdef-1234-4abc-8def-abcdefabcdef"
)

type ordinaryClaudeReauthFixture struct{}

func (ordinaryClaudeReauthFixture) GenerateAuthURL(state string, _ *claude.PKCECodes) (string, string, error) {
	return "https://oauth.example.invalid/authorize", state, nil
}

func (ordinaryClaudeReauthFixture) ExchangeCodeForTokens(_ context.Context, code, _ string, _ *claude.PKCECodes) (*claude.ClaudeAuthBundle, error) {
	return &claude.ClaudeAuthBundle{APIKey: code}, nil
}

func (ordinaryClaudeReauthFixture) CreateTokenStorage(bundle *claude.ClaudeAuthBundle) *claude.ClaudeTokenStorage {
	account, organization := fixtureClaudeAccountUUID, fixtureClaudeOrgUUID
	switch bundle.APIKey {
	case "different-account":
		account = strings.ToUpper(fixtureOtherUUID)
	case "different-organization":
		organization = strings.ToUpper(fixtureOtherUUID)
	case "same-uuid-uppercase":
		account, organization = strings.ToUpper(account), strings.ToUpper(organization)
	case "opaque-account-case":
		account, organization = "FIXTURE-SUBJECT", "fixture-org"
	case "opaque-organization-case":
		account, organization = "fixture-subject", "FIXTURE-ORG"
	}
	return &claude.ClaudeTokenStorage{AccessToken: "new-fake-access", RefreshToken: "new-fake-refresh",
		Email: "fixture@example.invalid", AccountUUID: account, OrganizationUUID: organization,
		Expire: "2099-03-27T14:00:00Z"}
}

func TestOrdinaryLegacyClaudeReauthUsesSelectedFileAndAdmin(t *testing.T) {
	const name = "selected + original.json"
	const old = `{"type":"claude","email":"fixture@example.invalid","account_uuid":"` + fixtureClaudeAccountUUID + `","organization_uuid":"` + fixtureClaudeOrgUUID + `","access_token":"old-fake-access","refresh_token":"old-fake-refresh","note":"preserve me","legacy":9007199254740993,"disabled":true,"reauth_required":true,"refresh_status":"reauth_required","refresh_disabled":true,"account_settings":{"refresh_enabled":false},"auto_quarantined":true,"quarantine_reason":"terminal_auth_failure","reauth_url":"https://foreign.example.invalid/synthetic-private-url"}`
	const mgmt = "/v0/management"
	const key = "test-only-admin"
	dir := t.TempDir()
	targetPath := filepath.Join(dir, name)
	siblingPath := filepath.Join(dir, "sibling.json")
	emailOnlyPath := filepath.Join(dir, "legacy-email-only.json")
	const opaqueName = "legacy-opaque.json"
	const opaque = `{"type":"claude","email":"fixture@example.invalid","account_uuid":"fixture-subject","organization_uuid":"fixture-org","access_token":"opaque-fake-access"}`
	opaquePath := filepath.Join(dir, opaqueName)
	const emailOnly = `{"type":"claude","email":"fixture@example.invalid","reauth_required":true,"access_token":"legacy-fake-access"}`
	for path, content := range map[string]string{
		emailOnlyPath:                        emailOnly,
		opaquePath:                           opaque,
		targetPath:                           old,
		siblingPath:                          strings.Replace(old, fixtureClaudeAccountUUID, fixtureOtherUUID, 1),
		filepath.Join(dir, "ownerless.json"): `{"type":"claude","reauth_required":true}`,
		filepath.Join(dir, "other-provider.json"): `{"type":"codex","reauth_required":true}`,
	} {
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	store := sdkauth.NewFileTokenStore()
	store.SetBaseDir(dir)
	manager := coreauth.NewManager(store, nil, nil)
	if err := manager.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: dir}, manager)
	h.tokenStore = store
	h.envSecret = key
	prev := newClaudeOAuthService
	started := make(chan struct{}, 10)
	newClaudeOAuthService = func(*config.Config, string) claudeOAuthService {
		started <- struct{}{}
		return ordinaryClaudeReauthFixture{}
	}
	t.Cleanup(func() { newClaudeOAuthService = prev })

	router := gin.New()
	if err := router.SetTrustedProxies(nil); err != nil {
		t.Fatal(err)
	}
	group := router.Group(mgmt, h.Middleware())
	group.GET("/auth-files", h.ListAuthFiles)
	group.GET("/anthropic-auth-url", h.RequestAnthropicTokenV0)
	group.POST("/oauth-callback", h.PostOAuthCallback)
	group.GET("/get-auth-status", h.GetAuthStatus)
	httpServer := httptest.NewServer(router)
	t.Cleanup(httpServer.Close)
	call := func(method, endpoint, body, managementKey string) (int, string) {
		t.Helper()
		req, err := http.NewRequest(method, httpServer.URL+endpoint, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if managementKey != "" {
			req.Header.Set("Authorization", "Bearer "+managementKey)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := httpServer.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, string(raw)
	}
	link := mgmt + "/anthropic-auth-url?auth_name=" + url.QueryEscape(name)
	var listing struct {
		Files []struct {
			Name, ID string
			Link     string `json:"reauth_url"`
		}
	}
	code, body := call(http.MethodGet, mgmt+"/auth-files", "", key)
	if code != http.StatusOK || json.Unmarshal([]byte(body), &listing) != nil {
		t.Fatalf("list status %d", code)
	}
	var selectedID string
	for _, row := range listing.Files {
		switch row.Name {
		case name:
			if row.Link != link {
				t.Fatalf("selected link = %q", row.Link)
			}
			selectedID = row.ID
		case "ownerless.json", "legacy-email-only.json", "other-provider.json":
			if row.Link != "" {
				t.Fatalf("unexpected link for %s", row.Name)
			}
		}
	}
	if selectedID == "" {
		t.Fatal("selected file missing from list")
	}
	for _, test := range []struct {
		endpoint, password string
		expected           int
	}{
		{link, "", http.StatusUnauthorized},
		{link, "wrong-admin-key", http.StatusUnauthorized},
		{link + "&auth_name=sibling.json", key, http.StatusBadRequest},
		{mgmt + "/anthropic-auth-url?auth_name=..%2Fsibling.json", key, http.StatusBadRequest},
		{mgmt + "/anthropic-auth-url?auth_name=%GG", key, http.StatusBadRequest},
		{mgmt + "/anthropic-auth-url?auth_name=missing.json", key, http.StatusNotFound},
		{mgmt + "/anthropic-auth-url?auth_name=ownerless.json", key, http.StatusConflict},
		{mgmt + "/anthropic-auth-url?auth_name=legacy-email-only.json", key, http.StatusConflict},
		{mgmt + "/anthropic-auth-url?auth_name=other-provider.json", key, http.StatusBadRequest},
	} {
		code, body := call(http.MethodGet, test.endpoint, "", test.password)
		if code != test.expected || strings.Contains(body, "state") {
			t.Fatalf("rejected start %s: got %d, want %d", test.endpoint, code, test.expected)
		}
	}
	if len(started) != 0 {
		t.Fatal("rejected requests started an authorization")
	}
	startSelected := func(authName, authID string) string {
		t.Helper()
		code, body := call(http.MethodGet, mgmt+"/anthropic-auth-url?auth_name="+url.QueryEscape(authName), "", key)
		var start struct {
			State, URL string
			AuthID     string `json:"target_auth_id"`
		}
		if code != http.StatusOK || json.Unmarshal([]byte(body), &start) != nil || start.AuthID != authID || start.State == "" || start.URL != "https://oauth.example.invalid/authorize" {
			t.Fatalf("selected start did not return the bound session, status %d", code)
		}
		t.Cleanup(func() { CancelOAuthSession(start.State) })
		return start.State
	}
	start := func() string { return startSelected(name, selectedID) }
	complete := func(state, grant string, expectAdopted ...bool) string {
		t.Helper()
		payload, _ := json.Marshal(map[string]string{"provider": "anthropic", "redirect_url": "http://localhost/callback?state=" + url.QueryEscape(state) + "&code=" + url.QueryEscape(grant)})
		code, _ := call(http.MethodPost, mgmt+"/oauth-callback", string(payload), key)
		if code != http.StatusOK {
			t.Fatalf("callback status %d", code)
		}
		for deadline := time.Now().Add(4 * time.Second); time.Now().Before(deadline); {
			code, body := call(http.MethodGet, mgmt+"/get-auth-status?state="+url.QueryEscape(state), "", key)
			var response struct {
				Status         string
				AuthID         string `json:"auth_id"`
				Saved, Adopted bool
			}
			if code != http.StatusOK || json.Unmarshal([]byte(body), &response) != nil {
				t.Fatalf("poll status %d", code)
			}
			wantAdopted := len(expectAdopted) == 0 || expectAdopted[0]
			if response.Status == "ok" && (response.AuthID != selectedID || !response.Saved || response.Adopted != wantAdopted) {
				t.Fatalf("completed grant for selected credential: saved=%v, adopted=%v, want adopted=%v", response.Saved, response.Adopted, wantAdopted)
			}
			if response.Status == "ok" && !wantAdopted && !strings.Contains(body, "selected runtime scope changed") {
				t.Fatal("saved-only grant omitted the runtime scope message")
			}
			if response.Status != "wait" {
				return response.Status
			}
			time.Sleep(15 * time.Millisecond)
		}
		t.Fatal("timed out waiting for the selected reauthentication")
		return ""
	}
	checkBytes := func(path, want string) {
		t.Helper()
		raw, err := os.ReadFile(path)
		if err != nil || string(raw) != want {
			t.Fatalf("unexpected file update: %v", err)
		}
	}
	checkBytes(emailOnlyPath, emailOnly)
	for _, grant := range []string{"different-account", "different-organization"} {
		if got := complete(start(), grant); got != "error" {
			t.Fatalf("%s outcome = %q", grant, got)
		}
		checkBytes(targetPath, old)
	}

	// A listing hint comes from the already loaded row. The authenticated
	// click still consults disk and refuses a file whose subject has changed.
	stale := strings.Replace(old, fixtureClaudeAccountUUID, fixtureOtherUUID, 1)
	if err := os.WriteFile(targetPath, []byte(stale), 0600); err != nil {
		t.Fatal(err)
	}
	if code, body := call(http.MethodGet, mgmt+"/auth-files", "", key); code != http.StatusOK || !strings.Contains(body, link) {
		t.Fatal("listing revalidated the loaded row against the changed file")
	}
	if code, _ := call(http.MethodGet, link, "", key); code != http.StatusConflict {
		t.Fatalf("stale selected file status = %d", code)
	}
	checkBytes(targetPath, stale)
	if err := os.WriteFile(targetPath, []byte(old), 0600); err != nil {
		t.Fatal(err)
	}
	state := start()
	moved := filepath.Join(dir, "renamed.json")
	if err := os.Rename(targetPath, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(targetPath, []byte(old), 0600); err != nil {
		t.Fatal(err)
	}
	if got := complete(state, "selected-account"); got != "error" {
		t.Fatalf("replacement inode outcome = %q", got)
	}
	checkBytes(moved, old)
	checkBytes(targetPath, old)

	// The hook runs after checking the selected session and before its write.
	// Both roots contain valid files, so only the directory generation can
	// stop an otherwise valid CAS in the now-inactive root.
	newRoot := t.TempDir()
	newRootPath := filepath.Join(newRoot, name)
	if err := os.WriteFile(newRootPath, []byte(old), 0600); err != nil {
		t.Fatal(err)
	}
	h.SetPostAuthHook(func(context.Context, *coreauth.Auth) error {
		h.SetConfig(&config.Config{AuthDir: newRoot})
		return nil
	})
	rootState := start()
	if got := complete(rootState, "selected-account", false); got != "error" {
		t.Fatalf("new root could inspect old status: %s", got)
	}
	code, body = call(http.MethodGet, mgmt+"/get-auth-status?state="+url.QueryEscape(rootState), "", key)
	if code != http.StatusOK || !strings.Contains(body, "unknown or expired state") || strings.Contains(body, `"saved"`) {
		t.Fatal("reloaded handler disclosed an outcome from the previous root")
	}
	finished := false
	for until := time.Now().Add(5 * time.Second); time.Now().Before(until); {
		session, exists := oauthSessions.Get(rootState)
		if exists && (session.Completed || session.Status != "") {
			if session.Saved || !strings.Contains(session.Status, "Selected credential directory changed") {
				t.Fatal("old-root session recorded an unexpected file commit")
			}
			finished = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !finished {
		t.Fatal("old-root callback worker did not finish")
	}
	checkBytes(newRootPath, old)
	checkBytes(targetPath, old)
	active, ok := manager.GetByID(selectedID)
	if !ok || active.Metadata["access_token"] != "old-fake-access" {
		t.Fatal("root switch altered the selected runtime record")
	}
	h.SetConfig(&config.Config{AuthDir: dir})
	h.SetPostAuthHook(nil)

	for _, change := range []string{"manager", "registration"} {
		current, ok := manager.GetByID(selectedID)
		if !ok || current == nil {
			t.Fatal("missing active selected record")
		}
		replacement := current.Clone()
		replacement.Metadata["access_token"] = "in-memory-replacement-" + change
		replacement.RegistrationEpoch = 0
		nextManager := manager
		if change == "manager" {
			nextManager = coreauth.NewManager(nil, nil, nil)
		}
		h.SetPostAuthHook(func(ctx context.Context, _ *coreauth.Auth) error {
			ctx = coreauth.WithSkipPersist(ctx)
			if change == "registration" {
				nextManager.Remove(ctx, selectedID)
			}
			if _, err := nextManager.Register(ctx, replacement); err != nil {
				return err
			}
			if change == "manager" {
				h.SetAuthManager(nextManager)
			}
			return nil
		})
		if got := complete(start(), "selected-account", false); got != "ok" {
			t.Fatalf("durable grant must be visible as saved after changing %s; outcome = %q", change, got)
		}
		active, ok := nextManager.GetByID(selectedID)
		if !ok || active.Metadata["access_token"] != "in-memory-replacement-"+change {
			t.Fatalf("saved grant replaced the active %s", change)
		}
		if change == "manager" {
			previous, ok := manager.GetByID(selectedID)
			if !ok || previous.Metadata["access_token"] != "old-fake-access" {
				t.Fatal("saved grant was injected into the detached manager")
			}
		}
		persisted, err := os.ReadFile(targetPath)
		if err != nil || !strings.Contains(string(persisted), "new-fake-access") {
			t.Fatalf("file CAS did not preserve the durable grant: %v", err)
		}
		h.SetAuthManager(manager)
		h.SetPostAuthHook(nil)
		if err := os.WriteFile(targetPath, []byte(old), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if got := complete(start(), "same-uuid-uppercase"); got != "ok" {
		t.Fatalf("same UUID with different hexadecimal case: selected completion outcome = %q", got)
	}
	raw, err := os.ReadFile(targetPath)
	var saved map[string]json.RawMessage
	if err != nil || json.Unmarshal(raw, &saved) != nil ||
		string(saved["access_token"]) != `"new-fake-access"` || string(saved["refresh_token"]) != `"new-fake-refresh"` ||
		string(saved["note"]) != `"preserve me"` || string(saved["legacy"]) != "9007199254740993" ||
		string(saved["disabled"]) != "true" || string(saved["account_settings"]) != `{"refresh_enabled":false}` ||
		len(saved["auto_quarantined"]) != 0 || len(saved["reauth_required"]) != 0 || len(saved["refresh_disabled"]) != 0 {
		t.Fatal("selected replacement did not preserve operator fields and clear the terminal credential warning")
	}
	checkBytes(siblingPath, strings.Replace(old, fixtureClaudeAccountUUID, fixtureOtherUUID, 1))
	checkBytes(moved, old)
	cold := coreauth.NewManager(store, nil, nil)
	if err := cold.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	reopened, ok := cold.GetByID(selectedID)
	if !ok || reopened.Metadata["access_token"] != "new-fake-access" || !reopened.Disabled ||
		reopened.AutoQuarantined || coreauth.IsReauthRequiredMetadata(reopened.Metadata) {
		t.Fatal("cold load did not reflect the selected completed grant")
	}
	if info, err := os.Stat(targetPath); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("credential permissions changed: %v", err)
	}

	saveHook := make(chan struct{}, 2)
	h.SetPostAuthHook(func(context.Context, *coreauth.Auth) error {
		saveHook <- struct{}{}
		return nil
	})
	defer h.SetPostAuthHook(nil)
	for _, grant := range []string{"opaque-account-case", "opaque-organization-case"} {
		if got := complete(startSelected(opaqueName, opaqueName), grant); got != "error" {
			t.Fatalf("%s outcome = %q", grant, got)
		}
		checkBytes(opaquePath, opaque)
	}
	if len(saveHook) != 0 {
		t.Fatal("a different opaque identity reached the save hook")
	}
}
