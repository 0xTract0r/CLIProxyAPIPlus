package management

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	fileauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"net/http"
	"os"
	"testing"
	"time"
)

func identity372(account string) string {
	b, _ := json.Marshal(map[string]any{"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": account}})
	return "e30." + base64.RawURLEncoding.EncodeToString(b) + "."
}
func TestOperator372CodexPriorCapability(t *testing.T) {
	for _, provider := range []string{"codex"} {
		t.Run(provider, func(t *testing.T) {
			f := setupSIWCFlow(t, false)
			f.router.POST("/credentials", f.h.UploadAuthFileV8)
			state, flow := f.start(t)
			payload := map[string]any{"type": provider, "account_id": "account-b", "access_token": "fresh-access-b", "refresh_token": "fresh-refresh-b", "id_token": identity372("account-b")}
			w := siwcRequest(t, f.router, http.MethodPost, "/credentials?name=codex.json", payload)
			if w.Code != http.StatusOK {
				t.Fatalf("operator replacement rejected: provider=%s status=%d pending=%v cancelled=%v", provider, w.Code, f.store.IsPending(state, "codex"), flow.ctx.Err())
			}
			live, _ := f.manager.GetByID(f.auth.ID)
			if live.Provider != provider || live.Metadata["account_id"] != "account-b" {
				t.Fatal("replacement not published")
			}
		})
	}
}
func TestOperator372StoreDoesNotInheritPriorIdentity(t *testing.T) {
	for _, provider := range []string{"codex", "claude"} {
		t.Run(provider, func(t *testing.T) {
			store := fileauth.NewFileTokenStore()
			store.SetBaseDir(t.TempDir())
			ctx := coreauth.WithAuthCreationIntent(context.Background())
			old := &coreauth.Auth{ID: "codex.json", FileName: "codex.json", Provider: "codex", Metadata: map[string]any{"type": "codex", "account_id": "account-a", "access_token": "old-access", "refresh_token": "old-refresh", "id_token": identity372("account-a"), "siwc": map[string]any{"access_token": "old-siwc", "refresh_token": "old-siwc-refresh", "device_id": "old-device"}, "account_settings": map[string]any{"tls_profile": map[string]any{"preset": "codex_go_http11_v1"}}, "headers": map[string]any{"X-Old-Identity": "old-only"}}}
			if _, err := store.Save(ctx, old); err != nil {
				t.Fatal(err)
			}
			incoming := &coreauth.Auth{ID: old.ID, FileName: old.FileName, Provider: provider, Metadata: map[string]any{"type": provider, "account_id": "account-b", "access_token": "fresh-access", "refresh_token": "fresh-refresh", "id_token": identity372("account-b")}}
			path, err := store.Save(ctx, incoming)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var disk map[string]any
			if err = json.Unmarshal(raw, &disk); err != nil {
				t.Fatal(err)
			}
			for _, key := range []string{"siwc", "account_settings", "headers"} {
				if _, ok := disk[key]; ok {
					t.Errorf("cross-identity replacement retained prior %s", key)
				}
			}
			if disk["account_id"] != "account-b" || disk["type"] != provider {
				t.Fatal("replacement identity did not persist")
			}
		})
	}
}



type operator372Store struct {
	*fileauth.FileTokenStore
	entered, release chan struct{}
	fail             bool
}

func (s *operator372Store) Save(ctx context.Context, a *coreauth.Auth) (string, error) {
	close(s.entered)
	<-s.release
	if s.fail {
		return "", fmt.Errorf("atomic372 failure")
	}
	return s.FileTokenStore.Save(ctx, a)
}
func TestOperator372AtomicReplacement(t *testing.T) {
	for _, mode := range []string{"commit", "failed-save", "commit-disabled"} {
		t.Run(mode, func(t *testing.T) {
			f := setupSIWCFlow(t, true)
			f.router.POST("/credentials", f.h.UploadAuthFileV8)
			state, flow := f.start(t)
			cb := siwcRequest(t, f.router, http.MethodPost, "/callback", map[string]any{"provider": "codex", "redirect_url": siwcCallbackURL(state)})
			if cb.Code != 200 {
				t.Fatal(cb.Code)
			}
			<-f.service.inputs
			old, _ := f.manager.GetByID(f.auth.ID)
			before, _ := os.ReadFile(f.path)
			store := &operator372Store{FileTokenStore: fileauth.NewFileTokenStore(), entered: make(chan struct{}), release: make(chan struct{}), fail: mode == "failed-save"}
			f.h.tokenStore = store
			hooks := 0
			f.h.postAuthPersistHook = func(ctx context.Context, a *coreauth.Auth) error {
				hooks++
				sentinel := fmt.Errorf("hook-gate-check")
				_, _, err := f.manager.Modify(ctx, a.ID, a.RegistrationEpoch, func(*coreauth.Auth) error { return sentinel })
				if err != sentinel {
					t.Errorf("post hook failed manager reentry: %v", err)
				}
				return nil
			}
			payload := map[string]any{"type": "codex", "account_id": "account-b", "access_token": "fresh-access-b", "refresh_token": "fresh-refresh-b", "id_token": identity372("account-b")}
			if mode == "commit-disabled" {
				payload["disabled"] = true
			}
			finished := make(chan int, 1)
			go func() {
				w := siwcRequest(t, f.router, http.MethodPost, "/credentials?name=codex.json", payload)
				finished <- w.Code
			}()
			select {
			case <-store.entered:
			case <-time.After(3 * time.Second):
				t.Fatal("replacement never reached Store")
			}
			live, _ := f.manager.GetByID(old.ID)
			middle, _ := os.ReadFile(f.path)
			if live.RegistrationEpoch != old.RegistrationEpoch || string(middle) != string(before) || flow.ctx.Err() != nil || !f.store.IsPending(state, "codex") || hooks != 0 {
				t.Fatal("published or invalidated pending before durable Save")
			}
			close(store.release)
			code := <-finished
			if mode == "failed-save" {
				after, _ := os.ReadFile(f.path)
				live, _ = f.manager.GetByID(old.ID)
				if code < 400 || string(after) != string(before) || live.RegistrationEpoch != old.RegistrationEpoch || live.CredentialVersion != old.CredentialVersion || flow.ctx.Err() != nil || hooks != 0 {
					t.Fatal("failed Save changed old identity/pending")
				}
				f.unblock()
				waitSIWCExchange(t, flow)
				if status := performOAuthStatusRequest(t, f.router, state); status.Status != "ok" {
					t.Fatalf("old pending failed after rejected replacement: %#v", status)
				}
				return
			}
			if code != 200 || hooks != 1 || flow.ctx.Err() == nil || f.store.IsPending(state, "codex") {
				t.Fatalf("commit status=%d hooks=%d pending=%v", code, hooks, f.store.IsPending(state, "codex"))
			}
			committed, _ := os.ReadFile(f.path)
			live, _ = f.manager.GetByID(old.ID)
			if live.Metadata["account_id"] != "account-b" || live.RegistrationEpoch <= old.RegistrationEpoch || live.Runtime != nil || live.Disabled != (mode == "commit-disabled") {
				t.Fatal("new principal not published fresh")
			}
			f.unblock()
			waitSIWCExchange(t, flow)
			f.manager.MarkResult(context.Background(), coreauth.Result{AuthID: old.ID, Provider: old.Provider, RegistrationEpoch: old.RegistrationEpoch, CredentialVersion: old.CredentialVersion, AccessToken: "outer", Error: &coreauth.Error{HTTPStatus: 401, Message: "old result"}})
			stale := old.Clone()
			stale.Metadata["access_token"] = "late-old"
			if _, err := f.manager.UpdateRefreshedAuth(context.Background(), old, stale); err == nil {
				t.Fatal("stale epoch refresh accepted")
			}
			after, _ := os.ReadFile(f.path)
			live, _ = f.manager.GetByID(old.ID)
			if string(after) != string(committed) || live.Metadata["account_id"] != "account-b" || live.Metadata["access_token"] != "fresh-access-b" {
				t.Fatal("old SIWC/result/refresh polluted replacement")
			}
		})
	}
}
