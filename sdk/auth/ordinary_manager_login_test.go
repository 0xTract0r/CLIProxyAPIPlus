package auth

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type ordinaryLocalAuthenticator struct{}

func (ordinaryLocalAuthenticator) Provider() string            { return "local-fixture" }
func (ordinaryLocalAuthenticator) RefreshLead() *time.Duration { return nil }
func (ordinaryLocalAuthenticator) Login(_ context.Context, cfg *config.Config, _ *LoginOptions) (*coreauth.Auth, error) {
	label := "default"
	if cfg != nil {
		label = filepath.Base(cfg.AuthDir)
	}
	return testLocalFileRecord("login.json", label), nil
}

func TestOrdinaryManagerLoginPreservesDefaultFileDirectory(t *testing.T) {
	rootDir, dirA, dirB := t.TempDir(), t.TempDir(), t.TempDir()
	root := NewFileTokenStore()
	root.SetBaseDir(rootDir)
	manager := NewManager(root, ordinaryLocalAuthenticator{})
	type loginResult struct {
		got, want string
		err       error
	}
	start := make(chan struct{})
	results := make(chan loginResult, 2)
	for _, dir := range []string{dirA, dirB} {
		go func() {
			<-start
			_, saved, err := manager.Login(context.Background(), "local-fixture", &config.Config{AuthDir: dir}, nil)
			results <- loginResult{saved, filepath.Join(dir, "login.json"), err}
		}()
	}
	close(start)
	for range 2 {
		result := <-results
		if result.err != nil || result.got != result.want {
			t.Errorf("explicit Login path=%q, want %q (%v)", result.got, result.want, result.err)
		}
	}
	_, saved, err := manager.Login(context.Background(), "local-fixture", nil, nil)
	if err != nil || saved != filepath.Join(rootDir, "login.json") || root.baseDirSnapshot() != rootDir {
		t.Errorf("nil-config Login failed to preserve default directory: path=%q err=%v", saved, err)
	}
	for _, dir := range []string{rootDir, dirA, dirB} {
		if _, err := os.Stat(filepath.Join(dir, "login.json")); err != nil {
			t.Errorf("expected local credential file: %v", err)
		}
	}
	if got, err := manager.SaveAuth(testLocalFileRecord("save.json", "ordinary"), &config.Config{AuthDir: dirA}); err != nil || got != filepath.Join(dirA, "save.json") {
		t.Errorf("SaveAuth explicit path=%q err=%v", got, err)
	}
	if got, err := manager.SaveAuth(testLocalFileRecord("save.json", "default"), nil); err != nil || got != filepath.Join(rootDir, "save.json") {
		t.Errorf("SaveAuth default path=%q err=%v", got, err)
	}
}
