package management

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	fileauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCompleteFieldsPatchTransactionalRecovery223(t *testing.T) {
	for _, name := range []string{"complete", "failed-save", "different-account", "missing-identity", "access-only", "outer-only", "operator-disabled"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			store := &recoveryCountingFileStore{FileTokenStore: fileauth.NewFileTokenStore()}
			store.SetBaseDir(dir)
			manager := coreauth.NewManager(store, nil, nil)
			old := &coreauth.Auth{ID: "patch223.json", FileName: "patch223.json", Provider: "codex", ProxyURL: "direct", CreatedAt: time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC), Status: coreauth.StatusQuarantined, AutoQuarantined: true, Unavailable: true, Success: 7, Failed: 9, Metadata: map[string]any{
				"type": "codex", "account_id": "account-a", "email": "synthetic@example.invalid", "access_token": "old-access", "refresh_token": "old-refresh", "id_token": "old-id", "proxy_url": "direct", "siwc": map[string]any{"access_token": "old-siwc-access", "refresh_token": "old-siwc-refresh"}, "auto_quarantined": true, "quarantine_reason": "terminal_auth_failure", "reauth_required": true, "refresh_disabled": true, "refresh_disabled_reason": "reauth_required", "note": "latest operator note", "priority": 23,
			}}
			// Enter terminal state through the real transition, including rejected
			// token ownership and quarantine freshness, rather than toggling booleans.
			old.Status, old.AutoQuarantined, old.Unavailable = coreauth.StatusActive, false, false
			for _, key := range []string{"auto_quarantined", "quarantine_reason", "reauth_required", "refresh_disabled", "refresh_disabled_reason"} {
				delete(old.Metadata, key)
			}
			if name == "operator-disabled" {
				old.Disabled = true
				old.Status = coreauth.StatusDisabled
				old.Metadata["disabled"] = true
			}
			before, err := manager.Register(coreauth.WithAuthCreationIntent(ctx), old)
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				current, _ := manager.GetByID(old.ID)
				manager.MarkResult(ctx, coreauth.Result{AuthID: old.ID, Provider: "codex", RegistrationEpoch: current.RegistrationEpoch, CredentialVersion: current.CredentialVersion, AccessToken: "old-access", Error: &coreauth.Error{HTTPStatus: 401, Code: "unauthorized", Message: "Encountered invalidated oauth token: synthetic"}})
			}
			before, _ = manager.GetByID(old.ID)
			if !before.AutoQuarantined || before.RejectedAccessToken != "old-access" {
				t.Fatal("real terminal state was not established")
			}
			path := filepath.Join(dir, old.FileName)
			original, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: dir}, manager)
			h.tokenStore = store
			hooks := 0
			h.postAuthPersistHook = func(ctx context.Context, a *coreauth.Auth) error {
				hooks++
				return h.upsertAuthRecord(coreauth.WithSkipPersist(ctx), a)
			}
			patch := map[string]any{"name": old.FileName, "account_id": "account-a", "email": "synthetic@example.invalid", "access_token": "new-access", "refresh_token": "new-refresh", "id_token": "new-id", "siwc": map[string]any{"access_token": "new-siwc-access", "refresh_token": "new-siwc-refresh"}}
			switch name {
			case "different-account":
				patch["account_id"] = "account-b"
			case "missing-identity":
				delete(patch, "account_id")
				delete(patch, "email")
			case "access-only":
				patch = map[string]any{"name": old.FileName, "access_token": "new-access"}
			case "outer-only":
				delete(patch, "siwc")
			}
			raw, _ := json.Marshal(patch)
			store.saves = 0
			store.fail = name == "failed-save"
			rec := httptest.NewRecorder()
			gc, _ := gin.CreateTestContext(rec)
			gc.Request = httptest.NewRequest(http.MethodPatch, "/v0/management/auth-files/fields", bytes.NewReader(raw))
			h.PatchAuthFileFields(gc)
			live, _ := manager.GetByID(old.ID)
			diskBytes, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var disk map[string]any
			if err = json.Unmarshal(diskBytes, &disk); err != nil {
				t.Fatal(err)
			}
			invalid := name == "different-account" || name == "missing-identity"
			partial := name == "access-only" || name == "outer-only"
			if invalid || store.fail || partial {
				t.Logf("state: epoch=%d/%d version=%d/%d quarantined=%v rejected=%v runtimeTokenOld=%v diskTokenOld=%v", live.RegistrationEpoch, before.RegistrationEpoch, live.CredentialVersion, before.CredentialVersion, live.AutoQuarantined, live.RejectedAccessToken != "", live.Metadata["access_token"] == "old-access", disk["access_token"] == "old-access")
				if live.RegistrationEpoch != before.RegistrationEpoch || live.CredentialVersion != before.CredentialVersion || !live.AutoQuarantined || live.Metadata["access_token"] != "old-access" || disk["access_token"] != "old-access" {
					t.Fatalf("incomplete/failed recovery mutated live or disk: status=%d", rec.Code)
				}
				if invalid || store.fail {
					if rec.Code < 400 || hooks != 0 || !bytes.Equal(original, diskBytes) {
						t.Fatalf("rejected recovery side effects: status=%d hooks=%d", rec.Code, hooks)
					}
				}
				if partial && rec.Code != 200 {
					t.Fatalf("ordinary partial status=%d %s", rec.Code, rec.Body.String())
				}
				return
			}
			if rec.Code != 200 {
				t.Fatalf("complete PATCH status=%d %s", rec.Code, rec.Body.String())
			}
			if store.saves != 1 || hooks != 1 {
				t.Fatalf("saves=%d hooks=%d", store.saves, hooks)
			}
			if live.AutoQuarantined || live.RegistrationEpoch <= before.RegistrationEpoch || live.CredentialVersion <= before.CredentialVersion || live.Index != before.Index || !live.CreatedAt.Equal(before.CreatedAt) || live.Success != before.Success || live.Failed != before.Failed || live.Disabled != before.Disabled {
				t.Fatal("recovery lost stable identity/history or epoch")
			}
			if disk["access_token"] != "new-access" || disk["refresh_token"] != "new-refresh" || live.Metadata["access_token"] != disk["access_token"] || live.Metadata["note"] != "latest operator note" {
				t.Fatal("complete recovery disk/runtime/operator mismatch")
			}
			manager.MarkResult(ctx, coreauth.Result{AuthID: old.ID, Provider: "codex", RegistrationEpoch: before.RegistrationEpoch, CredentialVersion: before.CredentialVersion, AccessToken: "old-access", Error: &coreauth.Error{HTTPStatus: 401, Message: "retired reply"}})
			after, _ := manager.GetByID(old.ID)
			if after.AutoQuarantined || after.Metadata["access_token"] != "new-access" {
				t.Fatal("retired callback poisoned recovered credential")
			}
		})
	}
}
