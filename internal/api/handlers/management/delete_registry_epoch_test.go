package management

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	sdkauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type pausedLocalDeleteSelector struct {
	*coreauth.RoundRobinSelector
	id      string
	retired chan struct{}
	resume  chan struct{}
}

func (s *pausedLocalDeleteSelector) InvalidateAuth(id string) {
	if id == s.id {
		close(s.retired)
		<-s.resume
	}
}

func TestLocalV8DeleteRegistryCleanupFollowsRegistrationEpoch(t *testing.T) {
	for _, mode := range []string{"remove-old-models", "preserve-new-registration"} {
		t.Run(mode, func(t *testing.T) {
			const oldModel = "fixture-delete-epoch-old"
			const newModel = "fixture-delete-epoch-new"
			dir := t.TempDir()
			oldPath := filepath.Join(dir, "old.json")
			newPath := filepath.Join(dir, "new.json")
			for _, path := range []string{oldPath, newPath} {
				if err := os.WriteFile(path, []byte(`{"note":"local fixture"}`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			id := "fixture-delete-epoch-" + mode
			pause := &pausedLocalDeleteSelector{RoundRobinSelector: &coreauth.RoundRobinSelector{}, id: id, retired: make(chan struct{}), resume: make(chan struct{})}
			var selector coreauth.Selector = pause.RoundRobinSelector
			if mode == "preserve-new-registration" {
				selector = pause
			}
			m := coreauth.NewManager(nil, selector, nil)
			h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: dir}, m)
			h.tokenStore = sdkauth.NewFileTokenStore()
			router := gin.New()
			router.DELETE("/v8/management/credentials", h.DeleteAuthFileV8)
			reg := registry.GetGlobalRegistry()
			t.Cleanup(func() { reg.UnregisterClient(id) })

			ctx := coreauth.WithSkipPersist(context.Background())
			register := func(path, model string) *coreauth.Auth {
				t.Helper()
				a, err := m.Register(ctx, &coreauth.Auth{
					ID: id, Provider: "claude", Attributes: map[string]string{coreauth.AttributePath: path},
				})
				if err != nil {
					t.Fatal(err)
				}
				seenRegistryEpoch := reg.ClientRegistrationEpoch(id)
				if !m.PublishIfRegistrationCurrent(id, a.RegistrationEpoch, a.Prefix, a.Provider, false, a.AuthKind(), a.Attributes, func() bool {
					return reg.RegisterClientIfEpochCurrent(id, "claude", seenRegistryEpoch, []*registry.ModelInfo{{ID: model}}, func() bool { return true })
				}) {
					t.Fatalf("model %s could not publish for registration %d", model, a.RegistrationEpoch)
				}
				return a
			}
			old := register(oldPath, oldModel)
			original := reg.GetModelsForClient(id)
			if len(original) != 1 || original[0].ID != oldModel {
				t.Fatalf("invalid registry fixture: %#v", original)
			}

			out := httptest.NewRecorder()
			done := make(chan struct{})
			go func() {
				defer close(done)
				router.ServeHTTP(out, httptest.NewRequest(http.MethodDelete, "/v8/management/credentials?name=old.json", nil))
			}()
			finish := sync.OnceFunc(func() { close(pause.resume) })
			defer func() { finish(); <-done }()
			if mode == "preserve-new-registration" {
				select {
				case <-pause.retired:
				case <-time.After(5 * time.Second):
					t.Fatal("retirement did not reach its post-guard callback")
				}
				if _, present := m.GetByID(id); present {
					t.Fatal("old registration was not retired")
				}
				replacement := register(newPath, newModel)
				if replacement.RegistrationEpoch <= old.RegistrationEpoch {
					t.Fatal("replacement did not receive a newer epoch")
				}
				beforeResume := reg.GetModelsForClient(id)
				if len(beforeResume) != 1 || beforeResume[0].ID != newModel {
					t.Fatal("replacement did not publish before old handler cleanup")
				}
			}
			finish()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("delete did not finish")
			}

			if out.Code != http.StatusOK {
				t.Fatalf("delete HTTP %d: %s", out.Code, out.Body)
			}
			if _, err := os.Stat(oldPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("old file remains: %v", err)
			}
			if _, err := os.Stat(newPath); err != nil {
				t.Fatalf("unrelated source missing: %v", err)
			}
			models := reg.GetModelsForClient(id)
			if mode == "remove-old-models" {
				if len(models) != 0 {
					t.Fatal("deleted credential kept old models")
				}
			} else {
				if got, ok := m.GetByID(id); !ok || got.Attributes[coreauth.AttributePath] != newPath {
					t.Fatal("new Manager registration was lost")
				}
				if len(models) != 1 || models[0].ID != newModel {
					t.Fatal("stale delete cleanup erased the replacement model mapping")
				}
			}
		})
	}
}
