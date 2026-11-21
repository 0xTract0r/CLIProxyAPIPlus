package config

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestConfigInodeRecoverySerializesCommit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("port: 8317\nhost: localhost\nproxy-url: http://fixture.invalid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	for range 12 {
		workers.Go(func() {
			if err := SaveConfigPreserveCommentsUpdateNestedScalar(path, []string{"host"}, "local-host"); err != nil {
				t.Error(err)
			}
		})
		workers.Go(func() {
			if err := SaveConfigPreserveCommentsUpdateNestedScalar(path, []string{"proxy-url"}, "http://other-fixture.invalid"); err != nil {
				t.Error(err)
			}
		})
	}
	workers.Wait()
	got, err := LoadConfig(path)
	if err != nil || got.Host != "local-host" || got.ProxyURL != "http://other-fixture.invalid" {
		t.Fatalf("reopen after concurrent edits: config=%v error=%v", got, err)
	}
}

func TestConfigHardLinksShareSuccessfulSaves(t *testing.T) {
	dir := t.TempDir()
	path, alias := filepath.Join(dir, "config.yaml"), filepath.Join(dir, "alias.yaml")
	if err := os.WriteFile(path, []byte("port: 8317\nhost: localhost\nrequest-retry: 7\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(path, alias); err != nil {
		t.Fatal(err)
	}
	original, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.RequestRetry = 11
	if err := SaveConfigPreserveComments(path, cfg); err != nil {
		t.Fatal(err)
	}
	if err := SaveConfigPreserveCommentsUpdateNestedScalar(alias, []string{"host"}, "alias-host"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{path, alias} {
		info, err := os.Stat(name)
		if err != nil || !os.SameFile(original, info) || info.Mode().Perm() != 0o640 {
			t.Fatalf("hard link metadata changed: %v", err)
		}
		got, err := LoadConfig(name)
		if err != nil || got.RequestRetry != 11 || got.Host != "alias-host" {
			t.Fatalf("hard link reopened with wrong config: %v", err)
		}
	}
}
