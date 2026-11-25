package watcher

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestInodeRecoveryContinuesActualConfigHotReload(t *testing.T) {
	path := os.Getenv("ROLLBACK_BIND_FIXTURE")
	if path == "" {
		t.Skip("requires a local bind fixture")
	}
	root := t.TempDir()
	authDir := filepath.Join(root, "auth")
	if err := os.Mkdir(authDir, 0o700); err != nil {
		t.Fatal(err)
	}
	initial := []byte("port: 8317\nrequest-retry: 7\nhost: localhost\nauth-dir: " + authDir + "\n")
	if err := os.WriteFile(path, initial, 0o640); err != nil {
		t.Fatal(err)
	}
	changes := make(chan *config.Config, 8)
	w, err := NewWatcher(path, authDir, func(cfg *config.Config) {
		changes <- cfg
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Stop() })
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	w.SetConfig(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := w.Start(ctx); err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		nextCfg := cfg.CloneForRuntime()
		nextCfg.RequestRetry = 11
		if i == 0 {
			err = config.SaveConfigPreserveComments(path, nextCfg)
		} else {
			err = config.SaveConfigPreserveCommentsUpdateNestedScalar(path, []string{"host"}, "other-host")
		}
		if err != nil {
			t.Fatal(err)
		}
		deadline := time.After(6 * time.Second)
	receive:
		for {
			select {
			case value := <-changes:
				if value.RequestRetry == 11 && ((i == 0 && value.Host == "localhost") || (i == 1 && value.Host == "other-host")) {
					t.Logf("actual watcher reload received request-retry=%d host=%s", value.RequestRetry, value.Host)
					break receive
				}
			case <-deadline:
				t.Fatalf("watcher never published edit %d", i+1)
			}
		}
	}
}
