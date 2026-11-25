package watcher

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestRollupActualWatcherRepeatedSaves(t *testing.T) {
	const childPath = "SOL_696_WATCH_CHILD_PATH"
	if path := os.Getenv(childPath); path != "" {
		if err := config.SaveConfigPreserveCommentsUpdateNestedScalar(path, []string{"host"}, "fixture-child"); err != nil {
			t.Fatal(err)
		}
		return
	}

	for _, mode := range []struct {
		name                 string
		bind, externalRename bool
	}{{"directory-default", false, false}, {"directory-external-replace", false, true}, {"single-bind", true, false}} {
		t.Run(mode.name, func(t *testing.T) {
			authDir := t.TempDir()
			path := filepath.Join(t.TempDir(), "config.yaml")
			if mode.bind {
				path = os.Getenv("ROLLBACK_BIND_FIXTURE")
				if path == "" {
					t.Skip("requires a real single-file bind fixture")
				}
			}
			initial := "port: 8317\nrequest-retry: 7\nhost: localhost\nauth-dir: " + authDir + "\nserver: {server-id: 00000000-0000-4000-8000-000000000001}\n"
			if err := os.WriteFile(path, []byte(initial), 0640); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.LoadConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			published := make(chan *config.Config, 16)
			ctx, cancel := context.WithCancel(t.Context())
			w, err := NewWatcher(path, authDir, func(cfg *config.Config) {
				select {
				case published <- cfg.CloneForRuntime():
				case <-ctx.Done():
				}
			})
			if err != nil {
				cancel()
				t.Fatal(err)
			}
			t.Cleanup(func() {
				cancel()
				if err := w.Stop(); err != nil {
					t.Error(err)
				}
			})
			w.SetConfig(cfg)
			if err := w.Start(ctx); err != nil {
				t.Fatal(err)
			}
			updated := cfg.CloneForRuntime()
			updated.RequestRetry = 11
			for i, edit := range []struct {
				retry int
				host  string
				save  func() error
			}{
				{11, "localhost", func() error {
					if !mode.externalRename {
						return config.SaveConfigPreserveComments(path, updated)
					}
					// Editors may replace the file even though the config API writes
					// in place. Subsequent child and CAS saves must still reload.
					tempPath := path + ".editor"
					if err := os.WriteFile(tempPath, []byte(strings.Replace(initial, "request-retry: 7", "request-retry: 11", 1)), 0640); err != nil {
						return err
					}
					return os.Rename(tempPath, path)
				}},
				{11, "fixture-child", func() error {
					child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRollupActualWatcherRepeatedSaves$", "-test.timeout=10s")
					child.Env = append(os.Environ(), childPath+"="+path)
					return child.Run()
				}},
				{11, "fixture-final", func() error {
					snapshot, err := config.ReadConfigFileSnapshot(path)
					if err != nil {
						return err
					}
					data := "port: 8317\nrequest-retry: 11\nhost: fixture-final\nauth-dir: " + authDir + "\nserver: {server-id: 00000000-0000-4000-8000-000000000001}\n"
					_, err = config.WriteConfigFileCAS(path, snapshot, []byte(data))
					return err
				}},
			} {
				before, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := edit.save(); err != nil {
					t.Fatalf("edit %d: %v", i+1, err)
				}
				after, err := os.Stat(path)
				if err != nil || !os.SameFile(before, after) != (mode.externalRename && i == 0) {
					t.Fatalf("edit %d unexpected inode identity, err=%v", i+1, err)
				}
				disk, err := config.ReadConfigFileSnapshot(path)
				if err != nil {
					t.Fatal(err)
				}
				parsed, err := config.ParseConfigBytes(disk.Bytes())
				if err != nil || parsed.RequestRetry != edit.retry || parsed.Host != edit.host {
					t.Fatalf("edit %d disk mismatch, err=%v", i+1, err)
				}
				timer := time.NewTimer(6 * time.Second)
			wait:
				for {
					select {
					case observed := <-published:
						if observed.RequestRetry == edit.retry && observed.Host == edit.host && observed.SourceHash == disk.SourceHash() {
							t.Logf("observed edit=%d retry=%d host=%s hash_match=true inode_changed=%t", i+1, observed.RequestRetry, observed.Host, !os.SameFile(before, after))
							break wait
						}
					case <-timer.C:
						t.Fatalf("no matching watcher snapshot for edit %d", i+1)
					}
				}
				timer.Stop()
			}
		})
	}
}

func TestSearchableConfigParent(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("requires a non-root test user")
	}
	for _, fixture := range []string{"file-fallback", "unwatchable-file", "unwatchable-auth", "symlink-resolved-parent"} {
		t.Run(fixture, func(t *testing.T) {
			root := t.TempDir()
			parent, auth := filepath.Join(root, "config"), filepath.Join(root, "auth")
			for _, dir := range []string{parent, auth} {
				if err := os.Mkdir(dir, 0700); err != nil {
					t.Fatal(err)
				}
			}
			path := filepath.Join(parent, "config.yaml")
			raw := "port: 8317\nhost: initial\nauth-dir: " + auth + "\nserver: {server-id: 00000000-0000-4000-8000-000000000001}\n"
			if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.LoadConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(parent, 0100); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(parent, 0700) })
			switch fixture {
			case "unwatchable-file":
				if err := os.Chmod(path, 0000); err != nil {
					t.Fatal(err)
				}
			case "unwatchable-auth":
				if err := os.Chmod(auth, 0100); err != nil {
					t.Fatal(err)
				}
			case "symlink-resolved-parent":
				alias := filepath.Join(root, "config.yaml")
				if err := os.Symlink(path, alias); err != nil {
					t.Fatal(err)
				}
				path = alias
			}
			changes := make(chan string, 16)
			w, err := NewWatcher(path, auth, func(v *config.Config) { changes <- v.Host })
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = w.Stop() })
			w.SetConfig(cfg)
			err = w.Start(t.Context())
			if fixture != "file-fallback" && fixture != "symlink-resolved-parent" {
				if !errors.Is(err, os.ErrPermission) {
					t.Fatalf("expected permission error, got %v", err)
				}
				if fixture == "unwatchable-auth" && !strings.Contains(err.Error(), auth) {
					t.Fatalf("expected auth-directory watch error, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"change-one", "change-two"} {
				if err := config.SaveConfigPreserveCommentsUpdateNestedScalar(filepath.Join(parent, "config.yaml"), []string{"host"}, want); err != nil {
					t.Fatal(err)
				}
				timer := time.NewTimer(6 * time.Second)
			wait:
				for {
					select {
					case got := <-changes:
						if got == want {
							t.Logf("observed %s", got)
							break wait
						}
					case <-timer.C:
						t.Fatalf("missing fsnotify reload for %q", want)
					}
				}
				timer.Stop()
			}
		})
	}
}
