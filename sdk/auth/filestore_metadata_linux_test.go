package auth

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func TestOrdinaryFileStoreExplicitEditMetadata(t *testing.T) {
	const before = `{"type":"fixture","note":"old"}`
	const after = `{"type":"fixture","note":"new"}`
	setup := func(t *testing.T) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "ordinary.json")
		if err := os.WriteFile(path, []byte(before), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o640); err != nil {
			t.Fatal(err)
		}
		return path
	}
	read := func(t *testing.T, path, want string) os.FileInfo {
		t.Helper()
		raw, err := os.ReadFile(path)
		if err != nil || string(raw) != want {
			t.Fatalf("read file = %q, %v; want %q", raw, err, want)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		return info
	}

	t.Run("ordinary replacement keeps mode and ownership", func(t *testing.T) {
		path := setup(t)
		old := read(t, path, before)
		if err := saveOrdinaryAuthJSON(path, []byte(after), true); err != nil {
			t.Fatal(err)
		}
		next := read(t, path, after)
		oldID, nextID := old.Sys().(*syscall.Stat_t), next.Sys().(*syscall.Stat_t)
		if next.Mode().Perm() != 0o640 || oldID.Uid != nextID.Uid || oldID.Gid != nextID.Gid || os.SameFile(old, next) {
			t.Fatalf("old mode, uid and gid = %v/%d/%d, new = %v/%d/%d", old.Mode(), oldID.Uid, oldID.Gid, next.Mode(), nextID.Uid, nextID.Gid)
		}
	})

	t.Run("hard link rejects replacement before archival", func(t *testing.T) {
		path := setup(t)
		alias := filepath.Join(filepath.Dir(path), "alias.json")
		if err := os.Link(path, alias); err != nil {
			t.Fatal(err)
		}
		if err := saveOrdinaryAuthJSON(path, []byte(after), true); !errors.Is(err, ErrUnsafeCredentialMetadata) {
			t.Fatalf("edit error = %v", err)
		}
		if !os.SameFile(read(t, path, before), read(t, alias, before)) {
			t.Fatal("hard link was replaced")
		}
		if _, err := os.Stat(filepath.Join(filepath.Dir(path), authFileHistoryDirName)); !os.IsNotExist(err) {
			t.Fatalf("history directory after rejection: %v", err)
		}
	})

	t.Run("attribute rejects replacement", func(t *testing.T) {
		path := setup(t)
		const name = "user.ordinary-fixture"
		if err := unix.Setxattr(path, name, []byte("marker"), 0); err != nil {
			if errors.Is(err, unix.EOPNOTSUPP) {
				t.Skip("filesystem does not support extended attributes")
			}
			t.Fatal(err)
		}
		if err := saveOrdinaryAuthJSON(path, []byte(after), true); !errors.Is(err, ErrUnsafeCredentialMetadata) {
			t.Fatalf("edit error = %v", err)
		}
		read(t, path, before)
		got := make([]byte, len("marker"))
		if n, err := unix.Getxattr(path, name, got); err != nil || string(got[:n]) != "marker" {
			t.Fatalf("attribute after rejection = %q, %v", got, err)
		}
	})
}
