package management

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	fileauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type recoveryCountingFileStore struct {
	*fileauth.FileTokenStore
	saves int
	fail  bool
}

func (s *recoveryCountingFileStore) Save(ctx context.Context, a *coreauth.Auth) (string, error) {
	s.saves++
	if s.fail {
		return "", errors.New("synthetic persistence failure")
	}
	return s.FileTokenStore.Save(ctx, a)
}

func TestSaveTokenRecordCredentialRecoveryPersistsAndFences(t *testing.T) {
	for _, tc := range []struct {
		name, provider, outer, siwc   string
		oldSIWC, recover, operatorOff bool
	}{
		{"codex full reauthentication", "codex", "new-outer", "new-siwc", true, true, false},
		{"codex outer only stays locked", "codex", "new-outer", "old-siwc", true, false, false},
		{"codex SIWC only stays locked", "codex", "old-outer", "new-siwc", true, false, false},
		{"codex access-only stays locked", "codex", "old-outer", "old-siwc", true, false, false},
		{"codex empty refresh stays locked", "codex", "", "", true, false, false},
		{"codex missing SIWC stays locked", "codex", "new-outer", "", true, false, false},
		{"codex outer partition replacement", "codex", "new-outer", "", false, true, false},
		{"claude reauthentication", "claude", "new-outer", "", false, true, false},
		{"operator refresh disabled survives", "codex", "new-outer", "new-siwc", true, true, true},
		{"operator account disabled survives", "codex", "new-outer", "new-siwc", true, true, false},
		{"config-only stays locked", "codex", "old-outer", "old-siwc", true, false, false},
		{"failed persistence never registers", "codex", "new-outer", "new-siwc", true, true, false},
		{"cross account filename rejected", "codex", "new-outer", "new-siwc", true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := &recoveryCountingFileStore{FileTokenStore: fileauth.NewFileTokenStore()}
			store.SetBaseDir(t.TempDir())
			manager := coreauth.NewManager(store, nil, nil)
			previous := &coreauth.Auth{ID: "reauth.json", FileName: "reauth.json", Provider: tc.provider, ProxyURL: "direct", Status: coreauth.StatusQuarantined, Unavailable: true, AutoQuarantined: true, QuarantineReason: "terminal_auth_failure", QuarantinedAt: time.Now(), Metadata: map[string]any{
				"type": tc.provider, "email": "synthetic@example.invalid", "account_id": "synthetic-account", "access_token": "old-access", "refresh_token": "old-outer", "proxy_url": "direct", "note": "keep-operator-note", "auto_quarantined": true, "quarantine_reason": "terminal_auth_failure", "quarantined_at": time.Now().Format(time.RFC3339Nano),
				"reauth_required": true, "refresh_status": "reauth_required", "refresh_error_code": "invalid_grant", "refresh_disabled": true, "refresh_disabled_reason": "reauth_required", "refresh_disabled_at": "2026-01-01T00:00:00Z", "last_refresh_error": "synthetic invalid_grant",
			}}
			previous.CreatedAt = time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
			previous.Success, previous.Failed = 7, 9
			previous.Metadata["priority"] = 23
			previous.Metadata["owner"] = "synthetic-owner"
			previous.Attributes = map[string]string{"priority": "23", "owner": "synthetic-owner"}
			if tc.name == "operator account disabled survives" {
				previous.Disabled = true
				previous.Metadata["disabled"] = true
				previous.Status = coreauth.StatusDisabled
			}
			if tc.oldSIWC {
				previous.Metadata["siwc"] = map[string]any{"access_token": "old-siwc-access", "refresh_token": "old-siwc"}
			}
			if tc.operatorOff {
				previous.Metadata["account_settings"] = map[string]any{"schema_version": 1, "refresh_enabled": false}
			}
			old, err := manager.Register(coreauth.WithAuthCreationIntent(ctx), previous)
			if err != nil {
				t.Fatal(err)
			}
			path := old.Attributes[coreauth.AttributePath]
			h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: filepath.Dir(path)}, manager)
			h.tokenStore = store
			// Exercise the actual persistence callback's manager update path, including
			// synthesis from bytes written to disk, rather than editing lock metadata.
			hookCalls := 0
			h.SetPostAuthPersistHook(func(ctx context.Context, a *coreauth.Auth) error {
				hookCalls++
				// The production SDK callback uses the same SkipPersist boundary.
				return h.upsertAuthRecord(coreauth.WithSkipPersist(ctx), a)
			})
			incoming := &coreauth.Auth{ID: old.ID, FileName: old.FileName, Provider: tc.provider, Metadata: map[string]any{"type": tc.provider, "email": "synthetic@example.invalid", "account_id": "synthetic-account", "access_token": "new-access", "refresh_token": tc.outer}}
			if tc.siwc != "" {
				incoming.Metadata["siwc"] = map[string]any{"access_token": "new-siwc-access", "refresh_token": tc.siwc}
			}
			if tc.name == "config-only stays locked" {
				incoming.Metadata["access_token"] = "old-access"
				incoming.Metadata["siwc"].(map[string]any)["access_token"] = "old-siwc-access"
				incoming.Metadata["note"] = "config-note"
			}
			beforeRaw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			store.saves = 0
			store.fail = tc.name == "failed persistence never registers"
			if tc.name == "cross account filename rejected" {
				incoming.Metadata["account_id"] = "another-account"
			}
			saved, err := h.saveTokenRecord(ctx, incoming)
			if tc.name == "cross account filename rejected" {
				if err == nil || store.saves != 0 || hookCalls != 0 {
					t.Fatalf("identity guard err=%v saves=%d hooks=%d", err, store.saves, hookCalls)
				}
				live, _ := manager.GetByID(old.ID)
				if live.RegistrationEpoch != old.RegistrationEpoch || live.Success != old.Success {
					t.Fatal("reused filename inherited or changed registration")
				}
				afterRaw, readErr := os.ReadFile(path)
				if readErr != nil || string(afterRaw) != string(beforeRaw) {
					t.Fatal("reused filename changed disk")
				}
				return
			}
			if store.fail {
				if err == nil || store.saves != 1 || hookCalls != 0 {
					t.Fatalf("failed persist side effects: err=%v saves=%d hooks=%d", err, store.saves, hookCalls)
				}
				live, _ := manager.GetByID(old.ID)
				if live.RegistrationEpoch != old.RegistrationEpoch || live.CredentialVersion != old.CredentialVersion || !live.AutoQuarantined {
					t.Fatal("failed persist changed runtime registration")
				}
				afterRaw, readErr := os.ReadFile(path)
				if readErr != nil || string(afterRaw) != string(beforeRaw) {
					t.Fatal("failed persist changed disk")
				}
				return
			}
			if store.saves != 1 || hookCalls != 1 {
				t.Fatalf("expected one disk write and one runtime hook, saves=%d hooks=%d", store.saves, hookCalls)
			}
			if err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(saved)
			if err != nil {
				t.Fatal(err)
			}
			disk := map[string]any{}
			if err = json.Unmarshal(raw, &disk); err != nil {
				t.Fatal(err)
			}
			live, ok := manager.GetByID(old.ID)
			if !ok {
				t.Fatal("saved credential missing from manager")
			}
			if coreauth.IsReauthRequiredMetadata(disk) == tc.recover || coreauth.IsReauthRequiredMetadata(live.Metadata) == tc.recover {
				t.Fatalf("recovery=%v diskLock=%v liveLock=%v", tc.recover, coreauth.IsReauthRequiredMetadata(disk), coreauth.IsReauthRequiredMetadata(live.Metadata))
			}
			if live.AutoQuarantined == tc.recover {
				t.Fatalf("live quarantine=%v recovery=%v", live.AutoQuarantined, tc.recover)
			}
			wantNote := "keep-operator-note"
			if tc.name == "config-only stays locked" {
				wantNote = "config-note"
			}
			if live.ID != old.ID || live.Index != old.Index || !live.CreatedAt.Equal(old.CreatedAt) || live.Success != old.Success || live.Failed != old.Failed || live.Disabled != old.Disabled {
				t.Fatalf("stable account identity/history changed: old=%s/%s/%v/%d/%d/%v live=%s/%s/%v/%d/%d/%v", old.ID, old.Index, old.CreatedAt, old.Success, old.Failed, old.Disabled, live.ID, live.Index, live.CreatedAt, live.Success, live.Failed, live.Disabled)
			}
			if disk["owner"] != "synthetic-owner" || live.Attributes["owner"] != "synthetic-owner" || live.Attributes["priority"] != "23" {
				t.Fatal("owner/priority lost")
			}
			if disk["note"] != wantNote || live.ProxyURL != "direct" {
				t.Fatal("operator fields lost")
			}
			if live.RefreshDisabled() != (!tc.recover || tc.operatorOff) {
				t.Fatalf("refresh disabled=%v", live.RefreshDisabled())
			}
			if tc.recover {
				if live.RegistrationEpoch <= old.RegistrationEpoch || live.CredentialVersion <= old.CredentialVersion {
					t.Fatalf("new credential did not advance epoch/version: old=%d/%d live=%d/%d", old.RegistrationEpoch, old.CredentialVersion, live.RegistrationEpoch, live.CredentialVersion)
				}
				if (!live.Disabled && live.Unavailable) || live.Status == coreauth.StatusQuarantined {
					t.Fatalf("recovered credential remains unavailable: status=%s", live.Status)
				}
				failed := live.Failed
				manager.MarkResult(ctx, coreauth.Result{AuthID: old.ID, Provider: old.Provider, RegistrationEpoch: old.RegistrationEpoch, CredentialVersion: old.CredentialVersion, AccessToken: "old-access", Error: &coreauth.Error{HTTPStatus: 401, Message: "synthetic old credential failure"}})
				manager.MarkResult(ctx, coreauth.Result{AuthID: old.ID, Provider: old.Provider, RegistrationEpoch: old.RegistrationEpoch, CredentialVersion: old.CredentialVersion, AccessToken: "old-access", Success: true})
				stale := old.Clone()
				stale.Metadata["access_token"] = "stale-refresh-access"
				if _, err := manager.UpdateRefreshedAuth(ctx, old, stale); err == nil {
					t.Fatal("old epoch refresh was accepted")
				}
				after, _ := manager.GetByID(old.ID)
				if after.AutoQuarantined || (!after.Disabled && after.Unavailable) || after.Failed != failed || after.Metadata["access_token"] != "new-access" {
					t.Fatal("old result polluted new credential")
				}
				afterRaw, err := os.ReadFile(saved)
				if err != nil {
					t.Fatal(err)
				}
				if string(afterRaw) != string(raw) {
					t.Fatal("old result changed persisted credential")
				}
			} else if live.RegistrationEpoch != old.RegistrationEpoch {
				t.Fatal("incomplete replacement opened a new registration")
			}
		})
	}
}
