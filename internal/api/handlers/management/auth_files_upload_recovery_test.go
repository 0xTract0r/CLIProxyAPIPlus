package management

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	fileauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestUploadAuthFileCompleteReauthenticationPublishesAfterSave(t *testing.T) {
	for _, name := range []string{"complete", "failed save", "different account", "operator disabled", "missing identity", "empty claims", "claims same account", "claims different account", "claims conflict with metadata", "account alias", "known account requires account identity", "legacy email identity", "missing current identity"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			store := &recoveryCountingFileStore{FileTokenStore: fileauth.NewFileTokenStore()}
			dir := t.TempDir()
			store.SetBaseDir(dir)
			manager := coreauth.NewManager(store, nil, nil)
			old := &coreauth.Auth{ID: "upload-recovery.json", FileName: "upload-recovery.json", Provider: "codex", ProxyURL: "direct", Status: coreauth.StatusQuarantined, AutoQuarantined: true, Unavailable: true, Metadata: map[string]any{
				"type": "codex", "email": "synthetic@example.invalid", "account_id": "account-a", "access_token": "old-access", "refresh_token": "old-refresh", "id_token": "old-id", "proxy_url": "direct",
				"siwc":             map[string]any{"access_token": "old-siwc-access", "refresh_token": "old-siwc-refresh"},
				"auto_quarantined": true, "quarantine_reason": "terminal_auth_failure", "quarantined_at": time.Now().Format(time.RFC3339Nano), "refresh_disabled": true, "refresh_disabled_reason": "reauth_required", "reauth_required": true,
				"priority": 23, "note": "keep operator note", "account_settings": map[string]any{"schema_version": 1, "refresh_enabled": false},
			}}
			old.CreatedAt = time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
			old.Success = 7
			old.Failed = 9
			if name == "operator disabled" {
				old.Disabled = true
				old.Status = coreauth.StatusDisabled
				old.Metadata["disabled"] = true
			}
			if name == "legacy email identity" || name == "missing current identity" {
				delete(old.Metadata, "account_id")
			}
			if name == "missing current identity" {
				delete(old.Metadata, "email")
			}
			before, err := manager.Register(coreauth.WithAuthCreationIntent(ctx), old)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, old.FileName)
			beforeBytes, err := os.ReadFile(path)
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
			fresh := map[string]any{"type": "codex", "email": "synthetic@example.invalid", "account_id": "account-a", "access_token": "new-access", "refresh_token": "new-refresh", "id_token": "new-id", "proxy_url": "direct", "siwc": map[string]any{"access_token": "new-siwc-access", "refresh_token": "new-siwc-refresh"}}
			jwt := func(account, email string) string {
				claims, errMarshal := json.Marshal(map[string]any{"email": email, "https://api.openai.com/auth": map[string]any{"chatgpt_account_id": account}})
				if errMarshal != nil {
					t.Fatal(errMarshal)
				}
				return "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString(claims) + ".synthetic"
			}
			rejectIdentity := false
			switch name {
			case "different account":
				fresh["account_id"] = "account-b"
				rejectIdentity = true
			case "missing identity", "empty claims", "claims same account", "claims different account":
				delete(fresh, "account_id")
				delete(fresh, "email")
				switch name {
				case "missing identity":
					rejectIdentity = true
				case "empty claims":
					fresh["id_token"] = jwt("", "")
					rejectIdentity = true
				case "claims same account":
					fresh["id_token"] = jwt("account-a", "synthetic@example.invalid")
				case "claims different account":
					fresh["id_token"] = jwt("account-b", "synthetic@example.invalid")
					rejectIdentity = true
				}
			case "claims conflict with metadata":
				fresh["id_token"] = jwt("account-b", "synthetic@example.invalid")
				rejectIdentity = true
			case "account alias":
				delete(fresh, "account_id")
				fresh["chatgpt_account_id"] = "account-a"
			case "known account requires account identity":
				delete(fresh, "account_id")
				rejectIdentity = true
			case "legacy email identity":
				delete(fresh, "account_id")
				fresh["email"] = "SYNTHETIC@example.invalid"
			case "missing current identity":
				rejectIdentity = true
			}
			payload, err := json.Marshal(fresh)
			if err != nil {
				t.Fatal(err)
			}
			store.saves = 0
			store.fail = name == "failed save"
			err = h.writeAuthFile(ctx, old.FileName, payload)
			live, _ := manager.GetByID(old.ID)
			afterBytes, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if store.fail || rejectIdentity {
				if err == nil || hooks != 0 || live.RegistrationEpoch != before.RegistrationEpoch || live.CredentialVersion != before.CredentialVersion || !live.AutoQuarantined || !bytes.Equal(beforeBytes, afterBytes) {
					t.Fatalf("rejected upload changed disk/runtime: err=%v hooks=%d", err, hooks)
				}
				wantSaves := 0
				if store.fail {
					wantSaves = 1
				}
				if store.saves != wantSaves {
					t.Fatalf("save calls=%d want=%d", store.saves, wantSaves)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if store.saves != 1 || hooks != 1 {
				t.Fatalf("saves=%d hooks=%d, want exactly one each", store.saves, hooks)
			}
			if live.AutoQuarantined || live.RegistrationEpoch <= before.RegistrationEpoch || live.CredentialVersion <= before.CredentialVersion || live.Index != before.Index || !live.CreatedAt.Equal(before.CreatedAt) || live.Success != before.Success || live.Failed != before.Failed || live.Disabled != before.Disabled {
				t.Fatal("complete upload lost recovery or stable account state")
			}
			if live.Metadata["note"] != "keep operator note" || live.Attributes["priority"] != "23" || !live.RefreshDisabled() {
				t.Fatal("upload lost operator controls")
			}
			var disk map[string]any
			if err = json.Unmarshal(afterBytes, &disk); err != nil {
				t.Fatal(err)
			}
			if disk["access_token"] != "new-access" || disk["refresh_token"] != "new-refresh" || live.Metadata["access_token"] != disk["access_token"] {
				t.Fatal("complete upload disk/runtime mismatch")
			}
			manager.MarkResult(ctx, coreauth.Result{AuthID: old.ID, Provider: "codex", RegistrationEpoch: before.RegistrationEpoch, CredentialVersion: before.CredentialVersion, AccessToken: "old-access", Error: &coreauth.Error{HTTPStatus: 401, Message: "old callback"}})
			after, _ := manager.GetByID(old.ID)
			if after.AutoQuarantined || after.Failed != live.Failed || after.Metadata["access_token"] != "new-access" {
				t.Fatal("retired generation poisoned uploaded credential")
			}
		})
	}
}
