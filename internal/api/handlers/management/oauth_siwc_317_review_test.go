package management

import (
	"context"
	"errors"
	fileauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"net/http"
	"os"
	"testing"
)

type review317Store struct {
	*fileauth.FileTokenStore
	beforeFail func()
}

func (s *review317Store) Save(context.Context, *coreauth.Auth) (string, error) {
	s.beforeFail()
	return "", errors.New("317 atomic save refusal")
}

func TestReview317CommitBoundary(t *testing.T) {
	for _, mode := range []string{"post-hook-fails", "save-fails-exchange-ready"} {
		t.Run(mode, func(t *testing.T) {
			f := setupSIWCFlow(t, true)
			f.router.POST("/credentials", f.h.UploadAuthFileV8)
			state, flow := f.start(t)
			cb := siwcRequest(t, f.router, http.MethodPost, "/callback", map[string]any{"provider": "codex", "redirect_url": siwcCallbackURL(state)})
			if cb.Code != 200 {
				t.Fatal(cb.Body.String())
			}
			<-f.service.inputs
			old, _ := f.manager.GetByID(f.auth.ID)
			calls := 0
			if mode == "post-hook-fails" {
				f.h.postAuthPersistHook = func(ctx context.Context, a *coreauth.Auth) error {
					calls++
					if f.h.siwcCommitMu.TryLock() {
						f.h.siwcCommitMu.Unlock()
						t.Error("upload gate released inside post hook")
					}
					if flow.ctx.Err() == nil || f.store.IsPending(state, "codex") {
						t.Error("317: durable commit did not cancel before hook")
					}
					live, _ := f.manager.GetByID(f.auth.ID)
					data, err := os.ReadFile(f.path)
					if err != nil || len(data) == 0 || live.Metadata["access_token"] != "new-outer" || live.RegistrationEpoch == old.RegistrationEpoch {
						t.Error("hook ran before durable publication")
					}
					session, _ := f.store.Get(state)
					commitSIWCUploadMutation(ctx)
					commitSIWCUploadMutation(ctx)
					again, _ := f.store.Get(state)
					if again.ExpiresAt != session.ExpiresAt || again.Status != session.Status {
						t.Error("commit notification not idempotent")
					}
					f.unblock()
					return errors.New("317 post-commit hook failure")
				}
			} else {
				f.h.tokenStore = &review317Store{FileTokenStore: fileauth.NewFileTokenStore(), beforeFail: func() {
					calls++
					if f.h.siwcCommitMu.TryLock() {
						f.h.siwcCommitMu.Unlock()
						t.Error("save not serialized")
					}
					if flow.ctx.Err() != nil || !f.store.IsPending(state, "codex") {
						t.Error("uncommitted save canceled pending")
					}
					f.unblock()
				}}
			}
			w := siwcRequest(t, f.router, http.MethodPost, "/credentials?name=codex.json", completeSIWCUpload315())
			if w.Code < 400 {
				t.Fatalf("expected surfaced failure: %d", w.Code)
			}
			waitSIWCExchange(t, flow)
			live, _ := f.manager.GetByID(f.auth.ID)
			if calls != 1 {
				t.Errorf("hook/save called %d times", calls)
			}
			if mode == "post-hook-fails" {
				if live.Metadata["access_token"] != "new-outer" || live.Metadata["use_siwc"] == true {
					t.Fatal("late exchange overwrote committed upload")
				}
			} else {
				if status := performOAuthStatusRequest(t, f.router, state); status.Status != "ok" {
					t.Fatalf("failed save prevented ready exchange: %#v", status)
				}
				if live.Metadata["access_token"] != old.Metadata["access_token"] || live.Metadata["siwc"].(map[string]any)["access_token"] != "new-siwc" {
					t.Fatal("failed save changed outer credential or prevented binding")
				}
			}
		})
	}
}
