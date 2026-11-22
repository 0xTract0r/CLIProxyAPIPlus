package watcher

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestReferenceWatcherRejectedDirectoryPreservesActiveSubscription(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, name := range []string{"missing", "no-read", "no-search"} {
		t.Run(name, func(t *testing.T) {
			a, c := t.TempDir(), t.TempDir()
			bad := filepath.Join(t.TempDir(), "bad")
			if name != "missing" {
				if err := os.Mkdir(bad, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(bad, "entry.json"), []byte("{}"), 0600); err != nil {
					t.Fatal(err)
				}
				mode := os.FileMode(0)
				if name == "no-search" {
					mode = 0400
				}
				if err := os.Chmod(bad, mode); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(bad, 0700) })
				if _, err := os.Stat(filepath.Join(bad, "entry.json")); !os.IsPermission(err) {
					t.Fatalf("fixture must deny actual child access: %v", err)
				}
				if _, err := os.ReadDir(bad); (err == nil) != (name == "no-search") {
					t.Fatalf("unexpected fixture listing result: %v", err)
				}
			}
			path := filepath.Join(t.TempDir(), "config.yaml")
			write := func(dir string, retry int) {
				t.Helper()
				data := fmt.Sprintf("port: 8317\nserver: {server-id: 11111111-1111-4111-8111-111111111111}\nauth-dir: %q\nrequest-retry: %d\n", dir, retry)
				if err := os.WriteFile(path, []byte(data), 0600); err != nil {
					t.Fatal(err)
				}
			}
			write(a, 1)
			parsed, callbacks, updates := make(chan struct{}, 8), make(chan *config.Config, 8), make(chan AuthUpdate, 32)
			priorLoader := loadConfigFileFunc
			loadConfigFileFunc = func(source string) (*config.Config, error) {
				cfg, err := priorLoader(source)
				if source == path {
					parsed <- struct{}{}
				}
				return cfg, err
			}
			t.Cleanup(func() { loadConfigFileFunc = priorLoader })
			w, err := NewWatcher(path, a, func(cfg *config.Config) { callbacks <- cfg })
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(func() {
				cancel()
				_ = w.Stop()
				w.configApplyMu.Lock()
				w.configApplyMu.Unlock()
			})
			w.SetConfig(&config.Config{AuthDir: a, RequestRetry: 1})
			w.SetAuthUpdateQueue(updates)
			if err := w.Start(ctx); err != nil {
				t.Fatal(err)
			}
			accept := func(dir string, retry int) {
				t.Helper()
				select {
				case cfg := <-callbacks:
					if cfg.AuthDir != dir || cfg.RequestRetry != retry {
						t.Fatalf("published directory=%q retry=%d", cfg.AuthDir, cfg.RequestRetry)
					}
				case <-time.After(4 * time.Second):
					t.Fatal("expected callback was not delivered")
				}
			}
			observed := func(label string) {
				t.Helper()
				select {
				case <-parsed:
					w.configApplyMu.Lock()
					w.configApplyMu.Unlock()
				case <-time.After(4 * time.Second):
					t.Fatalf("real config file event did not attempt %s", label)
				}
			}
			fileEvent := func(dir, id string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(dir, id), []byte(`{"type":"local-fixture","note":"ordinary"}`), 0600); err != nil {
					t.Fatal(err)
				}
				timeout := time.NewTimer(4 * time.Second)
				defer timeout.Stop()
				for {
					select {
					case update := <-updates:
						if update.ID == id && update.Auth != nil {
							return
						}
					case <-timeout.C:
						t.Fatalf("real file event %s did not reach the queue", id)
					}
				}
			}
			watched := func(dir string) bool {
				t.Helper()
				physical, err := filepath.EvalSymlinks(dir)
				if err != nil {
					t.Fatal(err)
				}
				return slices.Contains(w.watcher.WatchList(), physical)
			}
			accept(a, 1)
			write(bad, 9)
			observed("bad target")
			select {
			case cfg := <-callbacks:
				t.Errorf("bad target reached callback: dir=%q retry=%d", cfg.AuthDir, cfg.RequestRetry)
			default:
			}
			w.clientsMutex.RLock()
			active, accepted := w.authDir, w.config.AuthDir
			w.clientsMutex.RUnlock()
			if active != a || accepted != a || !watched(a) {
				t.Fatalf("lost A after rejected write: active=%q accepted=%q watches=%v", active, accepted, w.watcher.WatchList())
			}
			w.SetConfig(&config.Config{AuthDir: bad, RequestRetry: 9})
			if w.currentAuthDir() != a {
				t.Fatal("direct SetConfig accepted an inaccessible directory")
			}
			fileEvent(a, "after-rejected.json")
			write(c, 2)
			observed("valid C")
			accept(c, 2)
			if w.currentAuthDir() != c || watched(a) || !watched(c) {
				t.Fatalf("valid C did not become the active directory: %v", w.watcher.WatchList())
			}
			fileEvent(c, "after-switch.json")
		})
	}
}
