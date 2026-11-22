package management

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/watcher/synthesizer"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestOrdinaryConfigToggleCleanupUsesCommittedDirectory(t *testing.T) {
	dirA, dirB := t.TempDir(), t.TempDir()
	const key, endpoint = "synthetic-key", "https://fixture.invalid/v1"
	cfg := &config.Config{AuthDir: dirA, CodexKey: []config.CodexKey{{APIKey: key, BaseURL: endpoint}}}
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	id, _ := synthesizer.NewStableIDGenerator().Next("codex:apikey", key, endpoint, "", "", "")
	a, b := filepath.Join(dirA, id), filepath.Join(dirB, id)
	if err := os.WriteFile(a, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("other"), 0600); err != nil {
		t.Fatal(err)
	}
	manager := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{ID: id, Provider: "codex", Status: coreauth.StatusActive,
		Attributes: map[string]string{"api_key": key, "base_url": endpoint, "source": "config:codex[fixture]"}}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	h := NewHandler(cfg, configPath, manager)
	h.tokenStore = sdkAuth.NewFileTokenStore()
	reloaded := false
	h.SetConfigReloadHook(func(_ context.Context, snapshot *config.Config) {
		if snapshot.AuthDir != dirA || len(snapshot.CodexKey) != 1 ||
			len(snapshot.CodexKey[0].ExcludedModels) != 1 || snapshot.CodexKey[0].ExcludedModels[0] != "*" {
			t.Fatalf("unexpected committed configuration")
		}
		next := snapshot.CloneForRuntime()
		next.AuthDir = dirB
		h.SetConfig(next)
		reloaded = true
	})
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPatch, "/v0/management/auth-files/status",
		strings.NewReader(`{"name":"`+id+`","disabled":true}`))
	c.Request.Header.Set("Content-Type", "application/json")
	h.PatchAuthFileStatus(c)
	if rec.Code != http.StatusOK || !reloaded || h.configSnapshot().AuthDir != dirB {
		t.Fatalf("expected successful response and reload to B, got %d %s", rec.Code, rec.Body.String())
	}
	_, errA := os.Stat(a)
	dataB, errB := os.ReadFile(b)
	if !errors.Is(errA, os.ErrNotExist) || errB != nil || string(dataB) != "other" {
		t.Fatalf("want A removed and B unchanged, got A error %v and B contents %q error %v", errA, dataB, errB)
	}
}
