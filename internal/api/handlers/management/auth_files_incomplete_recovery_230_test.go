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
	"reflect"
	"sync"
	"testing"
	"time"
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
				// 333/337恢复内部完整same-account保存缺id合同；外部upload仍要求id。
				completeWithoutID := entry == "save" && missing == "id"
				if completeWithoutID {
					old.CreatedAt = time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
					old.Success, old.Failed = 7, 9
					old.Metadata["note"] = "operator-note-358"
					old.Metadata["account_settings"] = map[string]any{"schema_version": 1, "refresh_enabled": false}
					old.Metadata["quota_refresh_status"] = "reauth_required"
					old.Metadata["quota_refresh_error"] = "old-quota-error"
					old.Metadata["quota_next_refresh_after"] = "2035-01-01T00:00:00Z"
				}
				// These two same-account save operations persist partial content;
				// they do not grant recovery. All upload/full-material negatives remain.
				ordinaryPartial := entry == "save" && (missing == "siwc-refresh" || missing == "outer-refresh")
				if ordinaryPartial {
					old.Metadata["quota_refresh_status"] = "reauth_required"
					old.Metadata["quota_refresh_error"] = "old-quota-error"
					old.Metadata["quota_next_refresh_after"] = "2035-01-01T00:00:00Z"
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
				if completeWithoutID {
					disabled, ok, e := manager.Modify(ctx, old.ID, before.RegistrationEpoch, func(a *coreauth.Auth) error {
						a.Disabled = true
						a.Status = coreauth.StatusDisabled
						a.Metadata["disabled"] = true
						return nil
					})
					if e != nil || !ok {
						t.Fatalf("operator disable: %v", e)
					}
					before = disabled
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
				// F 1228bf9 accepts this external full-file provider replacement;
				// the internal save/provider rejection below remains unchanged.
				if entry == "upload" && missing == "provider" {
					if err != nil || store.saves != 1 || hooks != 1 || live.Provider != "other" || live.RegistrationEpoch <= before.RegistrationEpoch || live.CredentialVersion <= before.CredentialVersion {
						t.Fatalf("generic upload replacement: %v", err)
					}
					if live.Runtime != nil || live.Storage != nil || live.CodexRefreshPatch != nil || live.AutoQuarantined || live.RejectedAccessToken != "" {
						t.Fatal("inherited old principal")
					}
					var disk map[string]any
					if err := json.Unmarshal(after, &disk); err != nil {
						t.Fatal(err)
					}
					for _, metadata := range []map[string]any{live.Metadata, disk} {
						if metadata["type"] != "other" || metadata["access_token"] != "new-access" || metadata["refresh_token"] != "new-refresh" || metadata["siwc"].(map[string]any)["access_token"] != "new-siwc-access" || metadata["siwc"].(map[string]any)["refresh_token"] != "new-siwc-refresh" {
							t.Fatal("submitted credential not persisted")
						}
						for _, key := range []string{"account_settings", "headers", "auto_quarantined"} {
							if _, ok := metadata[key]; ok {
								t.Fatalf("inherited old %s", key)
							}
						}
					}
					return
				}
				if completeWithoutID {
					if err != nil || store.saves != 1 || hooks != 1 {
						t.Fatalf("complete no-id save err=%v saves=%d hooks=%d", err, store.saves, hooks)
					}
					var disk map[string]any
					if e := json.Unmarshal(after, &disk); e != nil {
						t.Fatal(e)
					}
					for where, metadata := range map[string]map[string]any{"disk": disk, "runtime": live.Metadata} {
						if metadata["access_token"] != "new-access" || metadata["refresh_token"] != "new-refresh" {
							t.Fatalf("%s outer credentials not replaced", where)
						}
						part, ok := metadata["siwc"].(map[string]any)
						if !ok || part["access_token"] != "new-siwc-access" || part["refresh_token"] != "new-siwc-refresh" {
							t.Fatalf("%s SIWC credentials not replaced", where)
						}
						if coreauth.IsReauthRequiredMetadata(metadata) {
							t.Fatalf("%s retained recovery lock", where)
						}
						for _, key := range []string{"quota_refresh_status", "quota_refresh_error", "quota_next_refresh_after"} {
							if _, ok := metadata[key]; ok {
								t.Fatalf("%s stale quota %s survived", where, key)
							}
						}
						settings, ok := metadata["account_settings"].(map[string]any)
						if !ok || settings["refresh_enabled"] != false || metadata["disabled"] != true || metadata["note"] != "operator-note-358" {
							t.Fatalf("%s operator policy lost", where)
						}
					}
					if bytes.Equal(original, after) || live.RegistrationEpoch <= before.RegistrationEpoch || live.CredentialVersion <= before.CredentialVersion || live.Index != before.Index || !live.CreatedAt.Equal(before.CreatedAt) || live.Success != before.Success || live.Failed != before.Failed {
						t.Fatal("recovery registration/history/credentials boundary changed")
					}
					if !live.Disabled || !live.RefreshDisabled() || live.AutoQuarantined || live.Status == coreauth.StatusQuarantined || live.RejectedAccessToken != "" || live.RejectedSIWCAccessToken != "" || live.LastError != nil || !reflect.ValueOf(live.Quota).IsZero() {
						t.Fatalf("terminal cleanup or operator disable incorrect: %+v", live)
					}
					// 真实并发旧epoch refresh/result同时尝试，不能污染已提交新代。
					var wg sync.WaitGroup
					staleErr := make(chan error, 1)
					wg.Add(2)
					go func() {
						defer wg.Done()
						stale := before.Clone()
						stale.Metadata["access_token"] = "late-old-access"
						_, e := manager.UpdateRefreshedAuth(ctx, before, stale)
						staleErr <- e
					}()
					go func() {
						defer wg.Done()
						manager.MarkResult(ctx, coreauth.Result{AuthID: before.ID, Provider: "codex", RegistrationEpoch: before.RegistrationEpoch, CredentialVersion: before.CredentialVersion, AccessToken: "old-access", Success: true})
						manager.MarkResult(ctx, coreauth.Result{AuthID: before.ID, Provider: "codex", RegistrationEpoch: before.RegistrationEpoch, CredentialVersion: before.CredentialVersion, AccessToken: "old-access", Error: &coreauth.Error{HTTPStatus: 401, Message: "old terminal result"}})
					}()
					wg.Wait()
					if e := <-staleErr; e == nil {
						t.Fatal("old epoch refresh admitted")
					}
					fenced, _ := manager.GetByID(before.ID)
					fencedDisk, e := os.ReadFile(path)
					if e != nil || !bytes.Equal(after, fencedDisk) || !reflect.DeepEqual(fenced, live) || store.saves != 1 || hooks != 1 {
						t.Fatal("concurrent old epoch changed recovered state")
					}
					return
				}
				if ordinaryPartial {
					if err != nil || store.saves != 1 || hooks != 1 {
						t.Fatalf("partial save err=%v saves=%d hooks=%d", err, store.saves, hooks)
					}
					var disk map[string]any
					if e := json.Unmarshal(after, &disk); e != nil {
						t.Fatal(e)
					}
					if bytes.Equal(original, after) || disk["access_token"] != "new-access" || live.Metadata["access_token"] != "new-access" {
						t.Fatal("partial credentials not persisted")
					}
					if !coreauth.CredentialsChanged(before, live) || live.CredentialVersion != before.CredentialVersion+1 || live.RegistrationEpoch != before.RegistrationEpoch || live.Index != before.Index || !live.CreatedAt.Equal(before.CreatedAt) || live.Success != before.Success || live.Failed != before.Failed {
						t.Fatal("partial registration/history/version changed")
					}
					if live.AutoQuarantined != before.AutoQuarantined || live.Unavailable != before.Unavailable || live.Status != before.Status || live.StatusMessage != before.StatusMessage || live.Disabled != before.Disabled || live.RejectedAccessToken != before.RejectedAccessToken || live.RejectedSIWCAccessToken != before.RejectedSIWCAccessToken || !reflect.DeepEqual(live.LastError, before.LastError) || !reflect.DeepEqual(live.Quota, before.Quota) {
						t.Fatal("partial changed terminal state")
					}
					for _, k := range []string{"quota_refresh_status", "quota_refresh_error", "quota_next_refresh_after"} {
						if !reflect.DeepEqual(disk[k], before.Metadata[k]) || !reflect.DeepEqual(live.Metadata[k], before.Metadata[k]) {
							t.Fatalf("partial lost quota %s", k)
						}
					}
					if missing == "outer-refresh" {
						if disk["refresh_token"] != before.Metadata["refresh_token"] || live.Metadata["refresh_token"] != before.Metadata["refresh_token"] {
							t.Fatal("missing outer refresh lost")
						}
					} else {
						oldPart := before.Metadata["siwc"].(map[string]any)
						if disk["siwc"].(map[string]any)["refresh_token"] != oldPart["refresh_token"] || live.Metadata["siwc"].(map[string]any)["refresh_token"] != oldPart["refresh_token"] {
							t.Fatal("missing SIWC refresh lost")
						}
					}
					manager.MarkResult(ctx, coreauth.Result{AuthID: before.ID, Provider: "codex", RegistrationEpoch: before.RegistrationEpoch, CredentialVersion: before.CredentialVersion, AccessToken: "old-access", Success: true})
					manager.MarkResult(ctx, coreauth.Result{AuthID: before.ID, Provider: "codex", RegistrationEpoch: before.RegistrationEpoch, CredentialVersion: before.CredentialVersion, AccessToken: "old-access", Error: &coreauth.Error{HTTPStatus: 401, Message: "old credential result"}})
					fenced, _ := manager.GetByID(before.ID)
					fencedDisk, e := os.ReadFile(path)
					if e != nil || !bytes.Equal(after, fencedDisk) || fenced.Failed != live.Failed || fenced.Success != live.Success || fenced.AutoQuarantined != live.AutoQuarantined || fenced.CredentialVersion != live.CredentialVersion || fenced.Metadata["access_token"] != "new-access" {
						t.Fatal("old result changed partial credential")
					}
					return
				}
				if err == nil || store.saves != 0 || hooks != 0 || live.RegistrationEpoch != before.RegistrationEpoch || live.CredentialVersion != before.CredentialVersion || !live.AutoQuarantined || !bytes.Equal(original, after) || live.Metadata["access_token"] != "old-access" {
					t.Fatalf("incomplete replacement was not atomic rejection: err=%v saves=%d hooks=%d epoch=%d/%d version=%d/%d quarantine=%v diskEqual=%v", err, store.saves, hooks, live.RegistrationEpoch, before.RegistrationEpoch, live.CredentialVersion, before.CredentialVersion, live.AutoQuarantined, bytes.Equal(original, after))
				}
			})
		}
	}
}
