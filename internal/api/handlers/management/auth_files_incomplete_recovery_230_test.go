package management

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	fileauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"os"
	"path/filepath"
	"testing"
)

func TestSameIDIncompleteCodexRecovery230(t *testing.T) {
	for _, entry := range []string{"upload", "save"} {
		for _, missing := range []string{"siwc-access", "siwc-refresh", "outer-access", "outer-refresh", "id", "identity", "provider", "metadata-only"} {
			t.Run(entry+"/"+missing, func(t *testing.T) {
				ctx := context.Background()
				dir := t.TempDir()
				store := &recoveryCountingFileStore{FileTokenStore: fileauth.NewFileTokenStore()}
				store.SetBaseDir(dir)
				manager := coreauth.NewManager(store, nil, nil)
				old := &coreauth.Auth{ID: "incomplete230.json", FileName: "incomplete230.json", Provider: "codex", ProxyURL: "direct", Metadata: map[string]any{"type": "codex", "account_id": "account-a", "email": "synthetic@example.invalid", "access_token": "old-access", "refresh_token": "old-refresh", "id_token": "old-id", "proxy_url": "direct", "siwc": map[string]any{"access_token": "old-siwc-access", "refresh_token": "old-siwc-refresh"}}}
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
				h.postAuthPersistHook = func(context.Context, *coreauth.Auth) error { hooks++; return nil }
				fresh := map[string]any{"type": "codex", "account_id": "account-a", "email": "synthetic@example.invalid", "access_token": "new-access", "refresh_token": "new-refresh", "id_token": "new-id", "proxy_url": "direct", "siwc": map[string]any{"access_token": "new-siwc-access", "refresh_token": "new-siwc-refresh"}}
				switch missing {
				case "siwc-access":
					delete(fresh["siwc"].(map[string]any), "access_token")
				case "siwc-refresh":
					delete(fresh["siwc"].(map[string]any), "refresh_token")
				case "outer-access":
					delete(fresh, "access_token")
				case "outer-refresh":
					delete(fresh, "refresh_token")
				case "id":
					delete(fresh, "id_token")
				case "identity":
					delete(fresh, "account_id")
					delete(fresh, "email")
				case "provider":
					fresh["type"] = "other"
				case "metadata-only":
					fresh = map[string]any{"type": "codex", "note": "ordinary fields belong to PATCH"}
				}
				payload, _ := json.Marshal(fresh)
				store.saves = 0
				if entry == "upload" {
					err = h.writeAuthFile(ctx, old.FileName, payload)
				} else {
					record, buildErr := h.buildAuthFromFileData(path, payload)
					if buildErr != nil {
						t.Fatal(buildErr)
					}
					_, err = h.saveTokenRecord(ctx, record)
				}
				live, _ := manager.GetByID(old.ID)
				after, readErr := os.ReadFile(path)
				if readErr != nil {
					t.Fatal(readErr)
				}
				if err == nil || store.saves != 0 || hooks != 0 || live.RegistrationEpoch != before.RegistrationEpoch || live.CredentialVersion != before.CredentialVersion || !live.AutoQuarantined || !bytes.Equal(original, after) || live.Metadata["access_token"] != "old-access" {
					t.Fatalf("incomplete replacement was not atomic rejection: err=%v saves=%d hooks=%d epoch=%d/%d version=%d/%d quarantine=%v diskEqual=%v", err, store.saves, hooks, live.RegistrationEpoch, before.RegistrationEpoch, live.CredentialVersion, before.CredentialVersion, live.AutoQuarantined, bytes.Equal(original, after))
				}
			})
		}
	}
}
