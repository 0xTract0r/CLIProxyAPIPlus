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

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	sdkauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestOrdinaryAccountSettingsKeepsOnlyRefreshBlock(t *testing.T) {
	const name = "ordinary-locked.json"
	const endpoint = "/auth-files/account-settings"
	const initial = `{"type":"codex","note":"before","account_settings":{"refresh_enabled":false}}`
	const turnOn = `{"name":"` + name + `","note":"unexpected","disabled":true,"refresh_enabled":true}`
	const editNote = `{"name":"` + name + `","note":"after"}`
	for _, tc := range []struct {
		name                                      string
		memoryOnly, signIn, quarantined, diskLock bool
	}{
		{name: "runtime sign-in on file", signIn: true},
		{name: "runtime quarantine on file", quarantined: true},
		{name: "runtime sign-in in memory", signIn: true, memoryOnly: true},
		{name: "file reauth lock while manager is stale", diskLock: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			path := filepath.Join(dir, name)
			var store coreauth.Store
			var auth *coreauth.Auth
			if tc.memoryOnly {
				store = &memoryAuthStore{}
				auth = &coreauth.Auth{
					ID: name, FileName: name, Provider: "codex", Status: coreauth.StatusActive,
					Metadata: map[string]any{"note": "before", "account_settings": map[string]any{"refresh_enabled": false}},
				}
			} else {
				if err := os.WriteFile(path, []byte(initial), 0600); err != nil {
					t.Fatal(err)
				}
				fileStore := sdkauth.NewFileTokenStore()
				fileStore.SetBaseDir(dir)
				store = fileStore
				records, err := store.List(ctx)
				if err != nil || len(records) != 1 {
					t.Fatalf("load fixture: count=%d err=%v", len(records), err)
				}
				auth = records[0]
			}
			auth.RequiresSignIn = tc.signIn
			auth.AutoQuarantined = tc.quarantined
			manager := coreauth.NewManager(store, nil, nil)
			registerCtx := ctx
			if !tc.memoryOnly {
				registerCtx = coreauth.WithSkipPersist(ctx)
			}
			if _, err := manager.Register(registerCtx, auth); err != nil {
				t.Fatal(err)
			}
			h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: dir}, manager)
			h.tokenStore = store
			router := gin.New()
			router.GET(endpoint, h.GetOrdinaryAuthFileAccountSettings)
			router.PATCH(endpoint, h.PatchOrdinaryAuthFileAccountSettings)
			call := func(method, body, etag string) *httptest.ResponseRecorder {
				req := httptest.NewRequest(method, endpoint+"?name="+name, strings.NewReader(body))
				req.Header.Set("If-Match", etag)
				res := httptest.NewRecorder()
				router.ServeHTTP(res, req)
				return res
			}
			readSettings := func(note string) string {
				t.Helper()
				res := call(http.MethodGet, "", "")
				var view struct {
					Settings ordinaryAccountSettingsView `json:"account_settings"`
				}
				if res.Code != http.StatusOK || json.Unmarshal(res.Body.Bytes(), &view) != nil ||
					view.Settings.RefreshEnabled || view.Settings.Disabled || view.Settings.Note != note {
					t.Fatalf("settings GET %d: %s", res.Code, res.Body.String())
				}
				etag := res.Header().Get("ETag")
				if etag == "" {
					t.Fatal("settings GET omitted ETag")
				}
				return etag
			}
			etag := readSettings("before")
			wantRaw := initial
			if tc.diskLock {
				wantRaw = strings.TrimSuffix(initial, "}") + `,"refresh_status":"reauth_required"}`
				if err := os.WriteFile(path, []byte(wantRaw), 0600); err != nil {
					t.Fatal(err)
				}
				res := call(http.MethodPatch, turnOn, etag)
				if res.Code != http.StatusPreconditionFailed {
					t.Fatalf("stale ETag PATCH %d: %s", res.Code, res.Body.String())
				}
				etag = readSettings("before")
			}
			res := call(http.MethodPatch, turnOn, etag)
			if res.Code != http.StatusConflict ||
				!strings.Contains(res.Body.String(), "automatic refresh is blocked by credential state") {
				t.Errorf("locked PATCH %d: %s", res.Code, res.Body.String())
			}
			if !tc.memoryOnly {
				data, err := os.ReadFile(path)
				if err != nil || string(data) != wantRaw {
					t.Errorf("rejected PATCH changed source bytes: %v", err)
				}
			}
			live, ok := manager.GetByID(name)
			if !ok || !live.RefreshDisabled() || live.RequiresSignIn != tc.signIn ||
				live.AutoQuarantined != tc.quarantined || live.Disabled {
				t.Error("rejected PATCH changed the live lock or credential state")
			}
			if got := readSettings("before"); got != etag {
				t.Fatal("rejected PATCH changed the setting revision")
			}
			res = call(http.MethodPatch, editNote, "")
			if res.Code != http.StatusOK {
				t.Fatalf("note-only PATCH %d: %s", res.Code, res.Body.String())
			}
			readSettings("after")

			if !tc.memoryOnly {
				cold := sdkauth.NewFileTokenStore()
				cold.SetBaseDir(dir)
				store = cold
				info, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				if info.Mode().Perm() != 0600 {
					t.Fatalf("file permissions changed to %v", info.Mode().Perm())
				}
			}
			records, err := store.List(ctx)
			if err != nil || len(records) != 1 {
				t.Fatalf("reload scoped credential: count=%d err=%v", len(records), err)
			}
			saved := records[0]
			if !saved.RefreshDisabled() || saved.Disabled || saved.Metadata["note"] != "after" ||
				(tc.diskLock && saved.Status != coreauth.StatusError) {
				t.Fatalf("stored credential lost refresh block or note: refreshDisabled=%v status=%s disabled=%v",
					saved.RefreshDisabled(), saved.Status, saved.Disabled)
			}
		})
	}
}
