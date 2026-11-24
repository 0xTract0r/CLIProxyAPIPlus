package store

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestGitDeleteExistingAuthFileKeepsPendingUploadMirror(t *testing.T) {
	dir := t.TempDir()
	remote := setupGitRemoteRepository(t, dir, "master", testBranchSpec{name: "master", contents: "original sibling\n"})
	s := NewGitTokenStore(remote, "", "", "")
	s.SetBaseDir(filepath.Join(dir, "replica", "auths"))
	if err := s.EnsureRepository(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.AuthDir(), "ordinary.json")
	old := `{"type":"qwen","note":"old"}`
	replacement := []byte(`{"type":"qwen","note":"new"}`)
	writeSeedTestFile(t, path, old)
	if err := s.PersistAuthUpload(t.Context(), path, replacement, func(commit func() error) error {
		if err := commit(); err != nil {
			return err
		}
		return errors.New("fixture local install deferred")
	}); !errors.Is(err, coreauth.ErrAuthUploadLocalPending) {
		t.Fatalf("upload = %v", err)
	}
	if err := s.DeleteExistingAuthFile(t.Context(), path); !errors.Is(err, coreauth.ErrAuthUploadPayloadConflict) {
		t.Fatalf("pending delete = %v", err)
	}
	if got := string(readSeedTestFile(t, path)); got != old {
		t.Fatal("pending delete changed local mirror")
	}
	assertRemoteFileContents(t, remote, "master", "auths/ordinary.json", string(replacement))
	if err := s.PersistAuthUpload(t.Context(), path, replacement, func(commit func() error) error {
		if err := commit(); err != nil {
			return err
		}
		return os.WriteFile(path, replacement, 0o600)
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteExistingAuthFile(t.Context(), path); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteExistingAuthFile(t.Context(), path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("second delete = %v", err)
	}
	assertRemoteFileContents(t, remote, "master", "branch.txt", "original sibling\n")
}
