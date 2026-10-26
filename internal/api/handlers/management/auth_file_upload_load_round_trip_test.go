package management

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	fileauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// Hand-authored component fixtures; no old-writer migration evidence.
// Claude sends the complete record back; Codex sends credential-only recovery.
// The post-persist hook below is test assembly, not proof of full production wiring.
func TestAuthFileUploadLoadRoundTrip(t *testing.T) {
	for _, provider := range []string{"codex", "claude"} {
		for _, disabled := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s_disabled_%v", provider, disabled), func(t *testing.T) {
				ctx := context.Background()
				dir := t.TempDir()
				name := "roundtrip.json"
				path := filepath.Join(dir, name)
				original := map[string]any{
					"type": provider, "email": "fake858@example.invalid", "account_id": "11111111-1111-4111-8111-111111111111", "account_uuid": "22222222-2222-4222-8222-222222222222", "organization_uuid": "33333333-3333-4333-8333-333333333333",
					"access_token": "fake-old-access-858", "refresh_token": "fake-old-refresh-858", "proxy_url": "http://127.0.0.1:9", "owner": "fake-owner-858", "priority": 23, "note": "keep-note-858", "disabled": disabled,
					"refresh_disabled": true, "refresh_disabled_reason": "operator", "refresh_disabled_at": "2025-01-02T03:04:05Z", "account_settings": map[string]any{"schema_version": 1, "refresh_enabled": false},
					"claude_device_ids": []string{strings.Repeat("a", 64), strings.Repeat("b", 64)}, "claude_device_id": strings.Repeat("a", 64), "device_id": strings.Repeat("a", 64), "user_agent": "fake-user-agent-858",
				}
				if provider == "codex" {
					original["siwc"] = map[string]any{"access_token": "fake-old-siwc-access-858", "refresh_token": "fake-old-siwc-refresh-858"}
					original["reauth_required"] = true
					original["refresh_status"] = "reauth_required"
					original["auto_quarantined"] = true
					original["quarantine_reason"] = "terminal_auth_failure"
					original["quarantined_at"] = "2025-01-02T03:04:05Z"
				}
				raw, err := json.Marshal(original)
				if err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
				fixed := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
				if err = os.Chtimes(path, fixed, fixed); err != nil {
					t.Fatal(err)
				}
				store := &recoveryCountingFileStore{FileTokenStore: fileauth.NewFileTokenStore()}
				store.SetBaseDir(dir)
				manager := coreauth.NewManager(store, nil, nil)
				if err = manager.Load(ctx); err != nil {
					t.Fatal(err)
				}
				old, ok := manager.GetByID(name)
				if !ok {
					t.Fatal("Manager.Load did not load hand-authored file")
				}
				if old.Index == "" || old.ProxyURL != "http://127.0.0.1:9" || old.Disabled != disabled || !old.RefreshDisabled() {
					t.Fatalf("initial C load contract failed: created=%v index=%q proxy=%q disabled=%v refreshDisabled=%v", old.CreatedAt, old.Index, old.ProxyURL, old.Disabled, old.RefreshDisabled())
				}
				// First-load oracle is the original input, not a possibly already-lossy loaded record.
				for key, wantValue := range original {
					want, _ := json.Marshal(wantValue)
					got, _ := json.Marshal(old.Metadata[key])
					if !bytes.Equal(want, got) {
						t.Errorf("initial load metadata %s differs from original: want=%s got=%s", key, want, got)
					}
				}
				assertStable := func(stage string, a *coreauth.Auth) {
					t.Helper()
					if a.ID != old.ID || a.Index != old.Index || a.Disabled != disabled || a.ProxyURL != old.ProxyURL || !a.RefreshDisabled() {
						t.Errorf("%s identity/settings mismatch id=%q index=%q disabled=%v proxy=%q refreshDisabled=%v", stage, a.ID, a.Index, a.Disabled, a.ProxyURL, a.RefreshDisabled())
					}
					for _, key := range []string{"email", "account_id", "account_uuid", "organization_uuid", "owner", "priority", "note", "account_settings", "claude_device_ids", "claude_device_id", "device_id", "user_agent"} {
						want, _ := json.Marshal(original[key])
						got, _ := json.Marshal(a.Metadata[key])
						if !bytes.Equal(want, got) {
							t.Errorf("%s metadata %s changed: want=%s got=%s", stage, key, want, got)
						}
					}
				}
				h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: dir}, manager)
				h.tokenStore = store
				hookCalls := 0
				h.SetPostAuthPersistHook(func(c context.Context, a *coreauth.Auth) error {
					hookCalls++
					return h.upsertAuthRecord(coreauth.WithSkipPersist(c), a)
				})
				assertRejectedRuntime := func(stage string) {
					t.Helper()
					current, exists := manager.GetByID(name)
					if !exists || current.ID != old.ID || current.Index != old.Index || current.RegistrationEpoch != old.RegistrationEpoch || current.CredentialVersion != old.CredentialVersion || current.Disabled != old.Disabled || current.ProxyURL != old.ProxyURL || current.Status != old.Status || current.Unavailable != old.Unavailable || current.AutoQuarantined != old.AutoQuarantined || current.QuarantineReason != old.QuarantineReason || !reflect.DeepEqual(current.Metadata, old.Metadata) {
						t.Errorf("%s changed tested runtime identity/version/operator/quarantine fields", stage)
					}
					if hookCalls != 0 {
						t.Errorf("%s called post-persist hook %d times", stage, hookCalls)
					}
				}
				gin.SetMode(gin.TestMode)
				r := gin.New()
				r.POST("/v0/management/auth-files", h.UploadAuthFile)
				server := httptest.NewServer(r)
				defer server.Close()
				post := func(meta map[string]any) (int, []byte) {
					t.Helper()
					b, e := json.Marshal(meta)
					if e != nil {
						t.Fatal(e)
					}
					resp, e := server.Client().Post(server.URL+"/v0/management/auth-files?name="+name, "application/json", bytes.NewReader(b))
					if e != nil {
						t.Fatal(e)
					}
					defer resp.Body.Close()
					body, e := io.ReadAll(resp.Body)
					if e != nil {
						t.Fatal(e)
					}
					return resp.StatusCode, body
				}
				incoming := map[string]any{"type": provider, "email": original["email"], "account_id": original["account_id"], "access_token": "fake-new-access-858", "refresh_token": "fake-new-refresh-858"}
				if provider == "codex" {
					incoming["id_token"] = "fake-new-id-token-858"
					incoming["siwc"] = map[string]any{"access_token": "fake-new-siwc-access-858", "refresh_token": "fake-new-siwc-refresh-858"}
				} else {
					for k, v := range original {
						incoming[k] = v
					}
					incoming["access_token"] = "fake-new-access-858"
					incoming["refresh_token"] = "fake-new-refresh-858"
				}
				if provider == "codex" {
					t.Run("cross_account_without_claims_rejected", func(t *testing.T) {
						bad := map[string]any{}
						for k, v := range incoming {
							bad[k] = v
						}
						bad["account_id"] = "44444444-4444-4444-8444-444444444444"
						status, body := post(bad)
						if status != http.StatusBadRequest {
							t.Fatalf("status=%d body=%s", status, body)
						}
						disk, e := os.ReadFile(path)
						if e != nil || !bytes.Equal(disk, raw) {
							t.Fatal("rejected cross-account upload changed disk")
						}
						assertRejectedRuntime("cross-account rejection")
						t.Logf("HTTP %d, disk and tested runtime fields unchanged; post-persist hooks=0", status)
					})
					t.Run("persistence_failure_is_atomic", func(t *testing.T) {
						store.fail = true
						defer func() { store.fail = false }()
						status, body := post(incoming)
						if status != http.StatusInternalServerError {
							t.Fatalf("status=%d body=%s", status, body)
						}
						disk, e := os.ReadFile(path)
						if e != nil || !bytes.Equal(disk, raw) {
							t.Fatal("failed save changed disk")
						}
						assertRejectedRuntime("failed persistence")
						t.Logf("HTTP %d, injected store failure, disk and tested runtime fields unchanged; post-persist hooks=0", status)
					})
				}
				status, body := post(incoming)
				if status != http.StatusOK {
					t.Fatalf("upload status=%d body=%s", status, body)
				}
				live, ok := manager.GetByID(name)
				if !ok {
					t.Fatal("live auth vanished")
				}
				assertStable("after HTTP", live)
				if !live.CreatedAt.Equal(old.CreatedAt) {
					t.Errorf("HTTP changed runtime CreatedAt: %v -> %v", old.CreatedAt, live.CreatedAt)
				}
				if hookCalls != 1 {
					t.Errorf("successful upload post-persist hook calls=%d, want 1", hookCalls)
				}
				assertNewCredential := func(stage string, a *coreauth.Auth) {
					t.Helper()
					if a.Metadata["access_token"] != "fake-new-access-858" || a.Metadata["refresh_token"] != "fake-new-refresh-858" {
						t.Errorf("%s did not preserve new outer credential pair", stage)
					}
					if provider == "codex" {
						siwc, ok := a.Metadata["siwc"].(map[string]any)
						if !ok || siwc["access_token"] != "fake-new-siwc-access-858" || siwc["refresh_token"] != "fake-new-siwc-refresh-858" {
							t.Errorf("%s did not preserve new SIWC credential pair", stage)
						}
						if a.AutoQuarantined || coreauth.IsReauthRequiredMetadata(a.Metadata) {
							t.Errorf("%s retained terminal auth lock", stage)
						}
						if v, ok := a.Metadata["auto_quarantined"].(bool); ok && v {
							t.Errorf("%s retained metadata quarantine lock", stage)
						}
					}
				}
				assertNewCredential("after HTTP", live)
				if provider == "codex" {
					if live.AutoQuarantined || coreauth.IsReauthRequiredMetadata(live.Metadata) {
						t.Error("complete fake credential update did not recover lock")
					}
					if live.RegistrationEpoch <= old.RegistrationEpoch || live.CredentialVersion <= old.CredentialVersion {
						t.Errorf("recovery epoch/version did not advance %d/%d -> %d/%d", old.RegistrationEpoch, old.CredentialVersion, live.RegistrationEpoch, live.CredentialVersion)
					}
					t.Run("stale_epoch_writeback_rejected", func(t *testing.T) {
						before, e := os.ReadFile(path)
						if e != nil {
							t.Fatal(e)
						}
						stale := old.Clone()
						stale.Metadata["access_token"] = "fake-stale-access-858"
						if _, e = manager.UpdateRefreshedAuth(ctx, old, stale); e == nil {
							t.Fatal("stale epoch accepted")
						}
						after, e := os.ReadFile(path)
						if e != nil || !bytes.Equal(before, after) {
							t.Fatal("stale writeback changed disk")
						}
						current, _ := manager.GetByID(name)
						if current.Metadata["access_token"] != "fake-new-access-858" || current.RegistrationEpoch != live.RegistrationEpoch {
							t.Fatal("stale writeback changed manager")
						}
						t.Log("old credential epoch rejected; disk/runtime unchanged")
					})
					histories, e := filepath.Glob(filepath.Join(dir, ".history", name+".*.bak"))
					if e != nil || len(histories) == 0 {
						t.Error("no pre-save history snapshot")
					}
					found := false
					for _, hp := range histories {
						b, e := os.ReadFile(hp)
						if e == nil && bytes.Equal(b, raw) {
							found = true
						}
					}
					if !found {
						t.Error("history lacks exact original fake JSON")
					}
				}
				restartStore := fileauth.NewFileTokenStore()
				restartStore.SetBaseDir(dir)
				restarted := coreauth.NewManager(restartStore, nil, nil)
				if err = restarted.Load(ctx); err != nil {
					t.Fatal(err)
				}
				after, ok := restarted.GetByID(name)
				if !ok {
					t.Fatal("restarted manager missing auth")
				}
				assertStable("after restart", after)
				assertNewCredential("after restart", after)
				info, e := os.Stat(path)
				if e != nil {
					t.Fatal(e)
				}
				t.Logf("file mtime observation after rewrite: %s", info.ModTime().Format(time.RFC3339Nano))
				t.Logf("CreatedAt observation: initial=%s HTTP=%s restart=%s; restart uses rewritten file mtime; original birth time continuity NOT proved", old.CreatedAt.Format(time.RFC3339Nano), live.CreatedAt.Format(time.RFC3339Nano), after.CreatedAt.Format(time.RFC3339Nano))
				t.Logf("epoch/version observation: load=%d/%d HTTP=%d/%d fresh-manager=%d/%d; fresh manager epochs are process-local", old.RegistrationEpoch, old.CredentialVersion, live.RegistrationEpoch, live.CredentialVersion, after.RegistrationEpoch, after.CredentialVersion)
				t.Logf("PASS hand-authored JSON -> actual Manager.Load -> loopback HTTP upload -> fresh Manager.Load; owner/priority/proxy/disabled/refresh-disabled and fake identity/device METADATA stable; TLS/device runtime execution NOT covered")
			})
		}
	}
}
