package management

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	fileauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestSIWCFileMutationRejectsLateExchange(t *testing.T) {
	for _, operation := range []string{"upload", "delete", "delete-all", "delete-batch"} {
		t.Run(operation, func(t *testing.T) {
			f := setupSIWCFlow(t, true)
			f.router.POST("/credentials", f.h.UploadAuthFileV8)
			f.router.DELETE("/credentials", f.h.DeleteAuthFileV8)
			state, flow := f.start(t)
			w := siwcRequest(t, f.router, http.MethodPost, "/callback", map[string]any{"state": state, "redirect_url": siwcCallbackURL(state), "provider": "codex"})
			if w.Code != http.StatusOK {
				t.Fatalf("callback: %d %s", w.Code, w.Body.String())
			}
			<-f.service.inputs
			original, errRead := os.ReadFile(f.path)
			if errRead != nil {
				t.Fatal(errRead)
			}
			switch operation {
			case "upload":
				var supplied map[string]any
				if err := json.Unmarshal(original, &supplied); err != nil {
					t.Fatal(err)
				}
				original, _ = json.Marshal(completeSIWCMaterial351(supplied, "mutation"))
				w = siwcRequest(t, f.router, http.MethodPost, "/credentials?name=codex.json", json.RawMessage(original))
			case "delete":
				w = siwcRequest(t, f.router, http.MethodDelete, "/credentials?name=codex.json", nil)
			case "delete-all":
				if errWrite := f.h.writeAuthFile(context.Background(), "other.json", original); errWrite != nil {
					t.Fatal(errWrite)
				}
				w = siwcRequest(t, f.router, http.MethodDelete, "/credentials?all=true", nil)
			case "delete-batch":
				if errWrite := f.h.writeAuthFile(context.Background(), "other.json", original); errWrite != nil {
					t.Fatal(errWrite)
				}
				w = siwcRequest(t, f.router, http.MethodDelete, "/credentials", []string{"codex.json", "other.json"})
			}
			if w.Code != http.StatusOK {
				t.Fatalf("mutation: %d %s", w.Code, w.Body.String())
			}
			session, ok := f.store.Get(state)
			if !ok || session.Status == "" || session.Completed || flow.ctx.Err() == nil {
				t.Fatal("mutation did not invalidate pending binding")
			}
			f.unblock()
			waitSIWCExchange(t, flow)
			if operation == "upload" {
				data, err := os.ReadFile(f.path)
				if err != nil || string(data) != string(original) {
					t.Fatalf("late exchange changed reupload: %s, %v", data, err)
				}
			} else {
				if _, err := os.Stat(f.path); !os.IsNotExist(err) {
					t.Fatalf("deleted file resurrected: %v", err)
				}
				if _, ok := f.manager.GetByID(f.auth.ID); ok {
					t.Fatal("deleted registration resurrected")
				}
			}
		})
	}
}

func TestSIWCFileMutationScopeAndValidation(t *testing.T) {
	f := setupSIWCFlow(t, false)
	f.router.POST("/credentials", f.h.UploadAuthFileV8)
	f.router.POST("/legacy", f.h.UploadAuthFile)
	state, flow := f.start(t)
	for _, test := range []struct {
		path   string
		body   any
		status int
	}{
		{"/credentials?name=codex.json", map[string]any{"excluded_models": 123}, http.StatusBadRequest},
		{"/credentials?name=other.json", f.auth.Metadata, http.StatusOK},
		{"/legacy?name=codex.json", completeSIWCMaterial351(f.auth.Metadata, "legacy"), http.StatusOK},
	} {
		w := siwcRequest(t, f.router, http.MethodPost, test.path, test.body)
		if w.Code != test.status {
			t.Fatalf("%s: %d %s", test.path, w.Code, w.Body.String())
		}
		if flow.ctx.Err() != nil || !f.store.IsPending(state, "codex") {
			t.Fatalf("%s invalidated unrelated or legacy binding", test.path)
		}
	}
	f.h.postAuthPersistHook = func(_ context.Context, _ *coreauth.Auth) error {
		if f.h.siwcCommitMu.TryLock() {
			f.h.siwcCommitMu.Unlock()
			t.Error("commit gate released before publication hook")
		}
		return nil
	}
	w := siwcRequest(t, f.router, http.MethodPost, "/credentials?name=codex.json", completeSIWCMaterial351(f.auth.Metadata, "final"))
	if w.Code != http.StatusOK || flow.ctx.Err() == nil {
		t.Fatalf("reupload failed to invalidate binding: %d", w.Code)
	}
}

func TestSIWCMutationPathAliases(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(dir, "alias.json")
	if err := os.Symlink(path, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if !sameSIWCMutationPath(alias, path) || sameSIWCMutationPath(path, filepath.Join(dir, "other.json")) {
		t.Fatal("incorrect canonical path matching")
	}
}

type failingSIWCStore315 struct{ *fileauth.FileTokenStore }

func (s *failingSIWCStore315) Save(context.Context, *coreauth.Auth) (string, error) {
	return "", errors.New("synthetic atomic Save failure")
}
func completeSIWCUpload315() map[string]any {
	return map[string]any{"type": "codex", "account_id": "account", "access_token": "new-outer", "refresh_token": "new-outer-refresh", "id_token": "local-synthetic-id", "note": "upload-315"}
}
func TestSIWCPending315RejectedUploadContinues(t *testing.T) {
	for _, mode := range []string{"material", "identity", "provider", "in-gate-validation", "persist-failure"} {
		t.Run(mode, func(t *testing.T) {
			f := setupSIWCFlow(t, false)
			f.router.POST("/credentials", f.h.UploadAuthFileV8)
			state, flow := f.start(t)
			before, _ := os.ReadFile(f.path)
			old, _ := f.manager.GetByID(f.auth.ID)
			payload := completeSIWCUpload315()
			switch mode {
			case "material":
				delete(payload, "id_token")
			case "identity":
				payload["account_id"] = "other"
			case "provider":
				payload["type"] = "other"
			case "in-gate-validation":
				f.h.postAuthHook = func(_ context.Context, a *coreauth.Auth) error { a.Metadata["account_id"] = "other"; return nil }
			case "persist-failure":
				f.h.tokenStore = &failingSIWCStore315{fileauth.NewFileTokenStore()}
			}
			w := siwcRequest(t, f.router, http.MethodPost, "/credentials?name=codex.json", payload)
			after, _ := os.ReadFile(f.path)
			live, _ := f.manager.GetByID(f.auth.ID)
			session, exists := f.store.Get(state)
			// F 1228bf9 crud:494–533,559–565 accepts generic provider replacement.
			if mode == "provider" {
				if w.Code != 200 || live.Provider != "other" || live.RegistrationEpoch <= old.RegistrationEpoch || live.CredentialVersion <= old.CredentialVersion || flow.ctx.Err() == nil || f.store.IsPending(state, "codex") {
					t.Fatal("generic provider replacement did not retire old registration")
				}
				if live.Runtime != nil || live.Storage != nil || live.CodexRefreshPatch != nil || live.Metadata["access_token"] != payload["access_token"] || live.Metadata["refresh_token"] != payload["refresh_token"] {
					t.Fatal("replacement inherited old credential state")
				}
				var disk map[string]any
				if err := json.Unmarshal(after, &disk); err != nil {
					t.Fatal(err)
				}
				for _, metadata := range []map[string]any{live.Metadata, disk} {
					for _, key := range []string{"account_settings", "headers", "siwc"} {
						if _, ok := metadata[key]; ok {
							t.Fatalf("inherited old %s", key)
						}
					}
					if metadata["note"] != payload["note"] {
						t.Fatal("submitted note not preserved")
					}
				}
				return
			}
			if w.Code < 400 {
				t.Fatalf("reject returned %d", w.Code)
			}
			if !exists || session.Status != "" || session.Completed || !f.store.IsPending(state, "codex") || flow.ctx.Err() != nil || string(before) != string(after) || live.RegistrationEpoch != old.RegistrationEpoch || live.CredentialVersion != old.CredentialVersion {
				t.Fatalf("rejected upload damaged pending: code=%d pending=%v canceled=%v status=%s diskEqual=%v", w.Code, f.store.IsPending(state, "codex"), flow.ctx.Err(), session.Status, string(before) == string(after))
			}
			f.h.postAuthHook = nil
			f.h.tokenStore = nil
			cb := siwcRequest(t, f.router, http.MethodPost, "/callback", map[string]any{"provider": "codex", "redirect_url": siwcCallbackURL(state)})
			if cb.Code != http.StatusOK {
				t.Fatalf("continuation callback: %d %s", cb.Code, cb.Body.String())
			}
			waitSIWCExchange(t, flow)
			if status := performOAuthStatusRequest(t, f.router, state); status.Status != "ok" {
				t.Fatalf("rejected upload prevented original bind: %#v", status)
			}
			live, _ = f.manager.GetByID(f.auth.ID)
			if live.Metadata["siwc"].(map[string]any)["access_token"] != "new-siwc" {
				t.Fatal("original bind did not publish its SIWC token")
			}
		})
	}
}
func TestSIWCPending315SuccessfulUploadFencesLateExchange(t *testing.T) {
	f := setupSIWCFlow(t, true)
	f.router.POST("/credentials", f.h.UploadAuthFileV8)
	state, flow := f.start(t)
	cb := siwcRequest(t, f.router, http.MethodPost, "/callback", map[string]any{"provider": "codex", "redirect_url": siwcCallbackURL(state)})
	if cb.Code != 200 {
		t.Fatal(cb.Body.String())
	}
	<-f.service.inputs
	other := siwcRequest(t, f.router, http.MethodPost, "/credentials?name=other.json", f.auth.Metadata)
	if other.Code != 200 || flow.ctx.Err() != nil {
		t.Fatal("unrelated file invalidated old binding")
	}
	w := siwcRequest(t, f.router, http.MethodPost, "/credentials?name=codex.json", completeSIWCUpload315())
	if w.Code != 200 {
		t.Fatalf("valid upload: %d %s", w.Code, w.Body.String())
	}
	if flow.ctx.Err() == nil || f.store.IsPending(state, "codex") {
		t.Fatal("committed mutation did not invalidate old intent")
	}
	before, _ := os.ReadFile(f.path)
	f.unblock()
	waitSIWCExchange(t, flow)
	after, _ := os.ReadFile(f.path)
	live, _ := f.manager.GetByID(f.auth.ID)
	if string(before) != string(after) || live.Metadata["access_token"] != "new-outer" || live.Metadata["use_siwc"] == true {
		t.Fatal("late exchange overwrote committed upload")
	}
}

// Diagnostic only: supply current-request identity and fresh credential partitions.
// Cross-account/provider and unchanged epoch expectations are deliberately kept.
func completeSIWCMaterial351(input map[string]any, tag string) map[string]any {
	raw, _ := json.Marshal(input)
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	account, _ := out["account_id"].(string)
	if account == "" {
		account = "account"
		out["account_id"] = account
	}
	claims, _ := json.Marshal(map[string]any{"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": account}})
	out["id_token"] = "e30." + base64.RawURLEncoding.EncodeToString(claims) + "."
	if _, ok := out["access_token"]; !ok {
		out["access_token"] = "fresh-access-" + tag
	}
	if tag == "mutation" || tag == "legacy" || tag == "final" {
		out["access_token"] = "fresh-access-" + tag
		out["refresh_token"] = "fresh-refresh-" + tag
	}
	out["disabled"] = false
	if tag == "commit" {
		out["siwc"] = map[string]any{"access_token": "fresh-siwc-access-" + tag, "refresh_token": "fresh-siwc-refresh-" + tag}
	}
	return out
}
