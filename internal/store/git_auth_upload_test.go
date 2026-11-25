package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v6"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestGitPreparedAuthUploadPublishesBytesBeforeLocalInstall(t *testing.T) {
	root := t.TempDir()
	remote := setupGitRemoteRepository(t, root, "master", testBranchSpec{name: "master", contents: "base\n"})
	s := NewGitTokenStore(remote, "", "", "")
	s.SetBaseDir(filepath.Join(root, "replica", "auths"))
	if err := s.EnsureRepository(); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(s.AuthDir(), "fixture.json")
	before := []byte(`{"type":"codex","note":"old"}`)
	prepared := []byte(`{"type":"codex","note":"prepared"}`)
	writeSeedTestFile(t, target, string(before))
	calls := 0
	s.pushFn = func(ctx context.Context, r *git.Repository, p *git.PushOptions) error {
		calls++
		if err := r.PushContext(ctx, p); err != nil {
			return err
		}
		return errors.New("fixture lost the successful push reply")
	}
	stopped := errors.New("fixture install deferred")
	err := s.PersistAuthUpload(t.Context(), target, prepared, func(commit func() error) error {
		if err := commit(); err != nil {
			return err
		}
		if string(readSeedTestFile(t, target)) != string(before) {
			t.Fatal("backend wrote the credential before the scoped installer")
		}
		return stopped
	})
	if !errors.Is(err, stopped) || calls != 1 {
		t.Fatalf("wanted confirmed remote and deferred installation: calls=%d err=%v", calls, err)
	}
	assertRemoteFileContents(t, remote, "master", "auths/fixture.json", string(prepared))
	assertRemoteFileContents(t, remote, "master", "branch.txt", "base\n")
	if err := s.PersistAuthUpload(t.Context(), target, []byte(`{"type":"codex","note":"different"}`), func(func() error) error {
		t.Fatal("different bytes reached local installer while previous upload pending")
		return nil
	}); err == nil || calls != 1 {
		t.Fatalf("pending upload accepted different payload: calls=%d err=%v", calls, err)
	}
	err = s.PersistAuthUpload(t.Context(), target, prepared, func(commit func() error) error {
		if err := commit(); err != nil {
			return err
		}
		return os.WriteFile(target, prepared, 0600)
	})
	if err != nil || calls != 1 || string(readSeedTestFile(t, target)) != string(prepared) {
		t.Fatalf("same bytes should complete local install only: calls=%d err=%v", calls, err)
	}
}

func TestGitPreparedAuthUploadRetriesLockedIndex(t *testing.T) {
	root := t.TempDir()
	remote := setupGitRemoteRepository(t, root, "master", testBranchSpec{name: "master", contents: "base\n"})
	replica := filepath.Join(root, "replica")
	s := NewGitTokenStore(remote, "", "", "")
	s.SetBaseDir(filepath.Join(replica, "auths"))
	if err := s.EnsureRepository(); err != nil {
		t.Fatal(err)
	}
	repo, err := git.PlainOpen(replica)
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	oldHead, err := repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	sibling := filepath.Join(replica, "branch.txt")
	const localSibling = "ordinary local edit\n"
	writeSeedTestFile(t, sibling, localSibling)
	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	siblingHash, err := worktree.Add("branch.txt")
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(s.AuthDir(), "fixture.json")
	prepared := []byte(`{"type":"codex","note":"prepared"}`)
	lock := filepath.Join(replica, ".git", "index.lock")
	pushes := 0
	s.pushFn = func(ctx context.Context, repo *git.Repository, options *git.PushOptions) error {
		pushes++
		return repo.PushContext(ctx, options)
	}
	err = s.PersistAuthUpload(t.Context(), target, prepared, func(commit func() error) error {
		if err := commit(); err != nil {
			return err
		}
		if err := os.WriteFile(target, prepared, 0600); err != nil {
			return err
		}
		return os.WriteFile(lock, nil, 0600)
	})
	if !errors.Is(err, coreauth.ErrAuthUploadLocalPending) || pushes != 1 {
		t.Fatalf("expected confirmed upload with pending local index: pushes=%d err=%v", pushes, err)
	}
	head, err := repo.Head()
	if err != nil || head.Hash() != oldHead.Hash() {
		t.Fatalf("local HEAD advanced before its index: head=%v err=%v", head, err)
	}
	assertRemoteFileContents(t, remote, "master", "auths/fixture.json", string(prepared))
	assertRemoteFileContents(t, remote, "master", "branch.txt", "base\n")
	if records, err := s.List(t.Context()); err != nil || len(records) != 1 || records[0].ID != "fixture.json" {
		t.Fatalf("local List unavailable during recovery: records=%d err=%v", len(records), err)
	}
	if err := s.EnsureRepository(); !errors.Is(err, coreauth.ErrAuthUploadLocalPending) {
		t.Fatalf("Git synchronization should wait for the shared index: %v", err)
	}
	if err := s.PersistAuthUpload(t.Context(), target, []byte(`{"type":"codex","note":"different"}`), func(func() error) error {
		t.Fatal("different payload reached the installer")
		return nil
	}); !errors.Is(err, coreauth.ErrAuthUploadPayloadConflict) {
		t.Fatalf("different payload was not rejected: %v", err)
	}
	if err := os.Remove(lock); err != nil {
		t.Fatal(err)
	}
	writeSeedTestFile(t, target, `{"type":"codex","note":"uncommitted worktree edit"}`)
	err = s.PersistAuthUpload(t.Context(), target, prepared, func(commit func() error) error { return commit() })
	if !errors.Is(err, coreauth.ErrAuthUploadPayloadConflict) || !errors.Is(err, coreauth.ErrAuthUploadLocalPending) {
		t.Fatalf("uncommitted target bytes should prevent local finalization: %v", err)
	}
	head, err = repo.Head()
	if err != nil || head.Hash() != oldHead.Hash() {
		t.Fatalf("local HEAD advanced for uncommitted bytes: head=%v err=%v", head, err)
	}
	err = s.PersistAuthUpload(t.Context(), target, prepared, func(commit func() error) error {
		if err := commit(); err != nil {
			return err
		}
		return os.WriteFile(target, prepared, 0600)
	})
	if err != nil || pushes != 1 {
		t.Fatalf("retry should only complete local index: pushes=%d err=%v", pushes, err)
	}
	bare, err := git.PlainOpen(remote)
	if err != nil {
		t.Fatal(err)
	}
	defer bare.Close()
	remoteHead, err := bare.Reference(oldHead.Name(), true)
	if err != nil {
		t.Fatal(err)
	}
	head, err = repo.Head()
	if err != nil || head.Hash() != remoteHead.Hash() {
		t.Fatalf("local/remote HEAD mismatch: local=%v remote=%v err=%v", head, remoteHead, err)
	}
	tree, err := gitTreeAt(repo, head.Hash())
	if err != nil {
		t.Fatal(err)
	}
	entry, err := tree.FindEntry("auths/fixture.json")
	if err != nil {
		t.Fatal(err)
	}
	idx, err := repo.Storer.Index()
	if err != nil {
		t.Fatal(err)
	}
	targetIndex, err := idx.Entry("auths/fixture.json")
	if err != nil || targetIndex.Hash != entry.Hash || targetIndex.Mode != entry.Mode {
		t.Fatalf("index does not match confirmed credential: %v", err)
	}
	siblingIndex, err := idx.Entry("branch.txt")
	if err != nil || siblingIndex.Hash != siblingHash || string(readSeedTestFile(t, sibling)) != localSibling {
		t.Fatalf("unrelated staged edit was consumed: %v", err)
	}
}

func TestGitPreparedAuthUploadColdStart(t *testing.T) {
	for _, tc := range []struct {
		name, localEdit, stagedEdit string
		wantError                   bool
	}{
		{name: "installed-file"},
		{name: "matching-index-and-file", stagedEdit: "prepared"},
		{name: "different-worktree", localEdit: "ordinary local edit", wantError: true},
		{name: "different-index", stagedEdit: "ordinary staged edit", wantError: true},
		{name: "matching-index-but-different-worktree", stagedEdit: "prepared", localEdit: "ordinary local edit", wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			remote := setupGitRemoteRepository(t, root, "master", testBranchSpec{name: "master", contents: "baseline\n"})
			replica := filepath.Join(root, "replica")
			authDir := filepath.Join(replica, "auths")
			target := filepath.Join(authDir, "fixture.json")
			s := NewGitTokenStore(remote, "", "", "")
			s.SetBaseDir(authDir)
			if err := s.EnsureRepository(); err != nil {
				t.Fatal(err)
			}
			repo, err := git.PlainOpen(replica)
			if err != nil {
				t.Fatal(err)
			}
			defer repo.Close()
			old, err := repo.Head()
			if err != nil {
				t.Fatal(err)
			}
			w, err := repo.Worktree()
			if err != nil {
				t.Fatal(err)
			}
			encode := func(note string) string { return `{"type":"codex","note":"` + note + `"}` }
			prepared := []byte(encode("prepared"))
			lock := filepath.Join(replica, ".git", "index.lock")
			pushes := 0
			s.pushFn = func(ctx context.Context, repo *git.Repository, opts *git.PushOptions) error {
				pushes++
				return repo.PushContext(ctx, opts)
			}
			err = s.PersistAuthUpload(t.Context(), target, prepared, func(commit func() error) error {
				if err := commit(); err != nil {
					return err
				}
				if err := os.WriteFile(target, prepared, 0600); err != nil {
					return err
				}
				return os.WriteFile(lock, nil, 0600)
			})
			if !errors.Is(err, coreauth.ErrAuthUploadLocalPending) || pushes != 1 {
				t.Fatalf("expected one push with pending index: pushes=%d err=%v", pushes, err)
			}
			if err := os.Remove(lock); err != nil {
				t.Fatal(err)
			}
			if tc.stagedEdit != "" {
				writeSeedTestFile(t, target, encode(tc.stagedEdit))
				if _, err := w.Add("auths/fixture.json"); err != nil {
					t.Fatal(err)
				}
				writeSeedTestFile(t, target, string(prepared))
			}
			if tc.localEdit != "" {
				writeSeedTestFile(t, target, encode(tc.localEdit))
			}
			sibling := filepath.Join(replica, "branch.txt")
			writeSeedTestFile(t, sibling, "ordinary unrelated edit\n")
			siblingHash, err := w.Add("branch.txt")
			if err != nil {
				t.Fatal(err)
			}
			before, err := os.Stat(target)
			if err != nil {
				t.Fatal(err)
			}
			targetBytes := string(readSeedTestFile(t, target))
			oldIndex, err := repo.Storer.Index()
			if err != nil {
				t.Fatal(err)
			}
			oldEntry, _ := oldIndex.Entry("auths/fixture.json")
			// Rebuild the store from scratch, with no in-memory upload receipt.
			cold := NewGitTokenStore(remote, "", "", "")
			cold.SetBaseDir(authDir)
			cold.pushFn = func(context.Context, *git.Repository, *git.PushOptions) error {
				t.Fatal("cold recovery attempted another push")
				return nil
			}
			err = cold.EnsureRepository()
			if (err != nil) != tc.wantError {
				t.Fatalf("cold EnsureRepository error=%v wantError=%v", err, tc.wantError)
			}
			head, err := repo.Head()
			if err != nil {
				t.Fatal(err)
			}
			index, err := repo.Storer.Index()
			if err != nil {
				t.Fatal(err)
			}
			entry, entryErr := index.Entry("auths/fixture.json")
			if tc.wantError {
				if head.Hash() != old.Hash() || (entry == nil) != (oldEntry == nil) || (entry != nil && entry.Hash != oldEntry.Hash) {
					t.Fatal("conflicting local index or HEAD changed")
				}
			} else {
				bare, err := git.PlainOpen(remote)
				if err != nil {
					t.Fatal(err)
				}
				defer bare.Close()
				remoteHead, err := bare.Reference(old.Name(), true)
				if err != nil {
					t.Fatal(err)
				}
				tree, err := gitTreeAt(repo, head.Hash())
				if err != nil {
					t.Fatal(err)
				}
				blob, err := tree.FindEntry("auths/fixture.json")
				if err != nil || entryErr != nil || head.Hash() != remoteHead.Hash() || entry.Hash != blob.Hash {
					t.Fatalf("cold index/HEAD did not match remote: %v %v", err, entryErr)
				}
				records, err := cold.List(t.Context())
				if err != nil || len(records) != 1 || records[0].ID != "fixture.json" {
					t.Fatalf("cold List count=%d err=%v", len(records), err)
				}
			}
			idxSibling, err := index.Entry("branch.txt")
			if err != nil || idxSibling.Hash != siblingHash || string(readSeedTestFile(t, sibling)) != "ordinary unrelated edit\n" {
				t.Fatalf("sibling did not survive cold start: %v", err)
			}
			after, err := os.Stat(target)
			if err != nil || !os.SameFile(before, after) || before.Mode() != after.Mode() || string(readSeedTestFile(t, target)) != targetBytes {
				t.Fatalf("local credential changed on cold start: %v", err)
			}
			assertRemoteFileContents(t, remote, "master", "auths/fixture.json", string(prepared))
			assertRemoteFileContents(t, remote, "master", "branch.txt", "baseline\n")
		})
	}
}
