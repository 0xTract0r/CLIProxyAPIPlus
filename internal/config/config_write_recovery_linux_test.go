package config

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
)

var recoveryOriginal = []byte("port: 8317\n# original trailing comment\n")

func TestConfigInodeRecoveryAfterPartialWrite(t *testing.T) {
	for _, tc := range []struct {
		name     string
		writeErr error
	}{
		{name: "short write", writeErr: nil},
		{name: "partial write with IO error", writeErr: os.ErrInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, recoveryOriginal, 0o640); err != nil {
				t.Fatal(err)
			}
			before, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			f, err := os.OpenFile(path, os.O_RDWR, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			next := []byte("port: 9\n")
			err = writeConfigWithRestore(f, recoveryOriginal, next, func(data []byte, off int64) (int, error) {
				n, err := f.WriteAt(data[:7], off)
				if err != nil || n != 7 {
					t.Fatalf("genuine prefix write n=%d err=%v", n, err)
				}
				bytesDuringFailure, err := os.ReadFile(path)
				if err != nil || bytes.Equal(bytesDuringFailure, recoveryOriginal) {
					t.Fatalf("fixture did not mutate bytes: %v", err)
				}
				return n, tc.writeErr
			})
			want := tc.writeErr
			if want == nil {
				want = io.ErrShortWrite
			}
			if !errors.Is(err, want) {
				t.Fatalf("wanted %v, got %v", want, err)
			}
			after, err := os.Stat(path)
			if err != nil || !os.SameFile(before, after) || after.Mode().Perm() != 0o640 {
				t.Fatalf("inode/mode changed: %v", err)
			}
			got, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(got, recoveryOriginal) {
				t.Fatalf("restore=%q err=%v", got, err)
			}
		})
	}
}

func TestConfigReplacementPreservesMountSemantics(t *testing.T) {
	path := os.Getenv("ROLLBACK_BIND_FIXTURE")
	if path == "" {
		t.Skip("requires a local Docker bind fixture")
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"port: 9001\n", "port: 9002\n# growth from prior\n"} {
		if err = writeConfigYAML(path, []byte(s)); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(path)
		if err != nil || string(got) != s {
			t.Fatalf("saved bytes %q %v", got, err)
		}
	}
	after, err := os.Stat(path)
	wantSame := os.Getenv("ROLLBACK_EXPECT_FILE_MOUNT") == "yes"
	if err != nil || os.SameFile(before, after) != wantSame || before.Mode().Perm() != after.Mode().Perm() ||
		before.Sys().(*syscall.Stat_t).Uid != after.Sys().(*syscall.Stat_t).Uid ||
		before.Sys().(*syscall.Stat_t).Gid != after.Sys().(*syscall.Stat_t).Gid {
		t.Fatalf("unexpected inode or metadata after save: %v", err)
	}
}

func TestConfigReplacementDoesNotFallbackOnPermissionFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("requires non-root directory permissions")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, recoveryOriginal, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if err := writeConfigYAML(path, []byte("port: 9001\n")); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("write error was %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, recoveryOriginal) {
		t.Fatalf("permission failure changed bytes: %q %v", got, err)
	}
}

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

func TestConfigCommittedOnDirectorySyncWarning(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, recoveryOriginal, 0o600); err != nil {
		t.Fatal(err)
	}
	original, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	next := []byte("port: 8318\n")
	if err := replaceConfigYAML(path, dir, original, next, func(_ *os.File) error {
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, next) {
			t.Fatalf("directory sync attempted before visible commit: %q %v", got, err)
		}
		return syscall.EIO
	}); err != nil {
		t.Fatalf("committed update must not trigger caller rollback: %v", err)
	}
	cfg, err := LoadConfig(path)
	if err != nil || cfg.Port != 8318 {
		t.Fatalf("committed update did not survive reopen: %v", err)
	}
}
