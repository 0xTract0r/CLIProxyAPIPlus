package management

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	fileauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"os"
	"path/filepath"
	"testing"
)

func TestCompleteMaterial327Boundaries(t *testing.T) {
	for _, mode := range []string{"access-claims-conflict", "id-claims-conflict", "siwc-claims-conflict", "email-claims-conflict", "prehook-claims-conflict", "missing-identity", "email-only", "cross-account", "provider", "alias-conflict", "prehook-strips-identity", "prehook-restores-old-refresh", "upload-still-requires-id"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			store := &recoveryCountingFileStore{FileTokenStore: fileauth.NewFileTokenStore()}
			store.SetBaseDir(t.TempDir())
			manager := coreauth.NewManager(store, nil, nil)
			old, err := manager.Register(coreauth.WithAuthCreationIntent(ctx), &coreauth.Auth{ID: "material.json", FileName: "material.json", Provider: "codex", AutoQuarantined: true, Status: coreauth.StatusQuarantined, Metadata: map[string]any{"type": "codex", "account_id": "account", "email": "same@example.invalid", "access_token": "old-access", "refresh_token": "old-refresh", "reauth_required": true, "refresh_disabled": true, "refresh_disabled_reason": "reauth_required", "proxy_url": "direct", "siwc": map[string]any{"access_token": "old-siwc", "refresh_token": "old-siwc-refresh"}}})
			if err != nil {
				t.Fatal(err)
			}
			path := old.Attributes[coreauth.AttributePath]
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: filepath.Dir(path)}, manager)
			h.tokenStore = store
			calls := 0
			h.SetPostAuthPersistHook(func(context.Context, *coreauth.Auth) error { calls++; return nil })
			incoming := &coreauth.Auth{ID: old.ID, FileName: old.FileName, Provider: "codex", Metadata: map[string]any{"type": "codex", "account_id": "account", "access_token": "fresh-access", "refresh_token": "fresh-refresh", "siwc": map[string]any{"access_token": "fresh-siwc", "refresh_token": "fresh-siwc-refresh"}}}
			jwt := func(account, email string) string {
				raw, _ := json.Marshal(map[string]any{"email": email, "https://api.openai.com/auth": map[string]any{"chatgpt_account_id": account}})
				return "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString(raw) + "."
			}
			switch mode {
			case "access-claims-conflict":
				incoming.Metadata["access_token"] = jwt("other", "")
			case "id-claims-conflict":
				incoming.Metadata["id_token"] = jwt("other", "")
			case "siwc-claims-conflict":
				incoming.Metadata["siwc"].(map[string]any)["access_token"] = jwt("other", "")
			case "email-claims-conflict":
				incoming.Metadata["email"] = "same@example.invalid"
				incoming.Metadata["access_token"] = jwt("account", "other@example.invalid")
			case "prehook-claims-conflict":
				h.postAuthHook = func(_ context.Context, a *coreauth.Auth) error {
					a.Metadata["access_token"] = jwt("other", "")
					return nil
				}

			case "missing-identity":
				delete(incoming.Metadata, "account_id")
			case "email-only":
				delete(incoming.Metadata, "account_id")
				incoming.Metadata["email"] = "same@example.invalid"
			case "cross-account":
				incoming.Metadata["account_id"] = "other"
			case "provider":
				incoming.Provider = "claude"
				incoming.Metadata["type"] = "claude"
			case "alias-conflict":
				incoming.Metadata["chatgpt_account_id"] = "other"
			case "prehook-strips-identity":
				h.postAuthHook = func(_ context.Context, a *coreauth.Auth) error { delete(a.Metadata, "account_id"); return nil }
			case "prehook-restores-old-refresh":
				h.postAuthHook = func(_ context.Context, a *coreauth.Auth) error {
					a.Metadata["refresh_token"] = "old-refresh"
					return nil
				}
			}
			store.saves = 0
			if mode == "upload-still-requires-id" {
				raw, _ := json.Marshal(incoming.Metadata)
				err = h.writeAuthFile(ctx, old.FileName, raw)
			} else {
				_, err = h.saveTokenRecord(ctx, incoming)
			}
			after, readErr := os.ReadFile(path)
			live, _ := manager.GetByID(old.ID)
			if err == nil || store.saves != 0 || calls != 0 || readErr != nil || string(after) != string(before) || live.RegistrationEpoch != old.RegistrationEpoch || live.CredentialVersion != old.CredentialVersion || !live.AutoQuarantined {
				t.Fatalf("unsafe admission: err=%v saves=%d hooks=%d epoch=%d/%d", err, store.saves, calls, old.RegistrationEpoch, live.RegistrationEpoch)
			}
		})
	}
}
