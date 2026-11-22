package auth

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type storageHistoryWriter func(string) error

func (w storageHistoryWriter) SaveTokenToFile(path string) error { return w(path) }

func TestOrdinaryTokenStorageKeepsRecoveryCopy(t *testing.T) {
	dir := t.TempDir()
	const name = "credential.json"
	path := dir + string(os.PathSeparator) + "." + string(os.PathSeparator) + name
	history := filepath.Join(dir, authFileHistoryDirName)
	read := func(path string) string {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	write := func(value string) error { return os.WriteFile(path, []byte(value), 0o600) }
	if err := write("original"); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("fixture failure")
	revision := 0
	writer := storageHistoryWriter(func(got string) error {
		if got != path {
			t.Fatalf("writer received %q, want %q", got, path)
		}
		if revision == 0 {
			if err := write("partial"); err != nil {
				return err
			}
			return failure
		}
		return write(fmt.Sprint(revision))
	})
	auth := &cliproxyauth.Auth{ID: name, Provider: "fixture", Storage: writer,
		Attributes: map[string]string{cliproxyauth.AttributePath: path}}
	store := NewFileTokenStore().WithBaseDir(dir)
	if saved, err := store.Save(context.Background(), auth); err != failure || saved != "" {
		t.Fatalf("failed save returned (%q, %v)", saved, err)
	}
	snapshots := listHistorySnapshots(t, dir, name)
	if len(snapshots) != 1 || read(filepath.Join(history, snapshots[0])) != "original" || read(path) != "partial" {
		t.Fatal("original bytes must remain available after a partial write")
	}
	for p, mode := range map[string]os.FileMode{history: 0o700, filepath.Join(history, snapshots[0]): 0o600} {
		info, err := os.Stat(p)
		if err != nil || info.Mode().Perm() != mode {
			t.Fatalf("expected private mode %o on %s (%v)", mode, p, err)
		}
	}
	for revision = 1; revision <= 10; revision++ {
		saved, err := store.Save(context.Background(), auth)
		if err != nil || saved != path || read(path) != fmt.Sprint(revision) {
			t.Fatalf("retry/update %d returned (%q, %v)", revision, saved, err)
		}
		if revision == 1 && read(filepath.Join(history, snapshots[0])) != "original" {
			t.Fatal("retry lost recovery copy")
		}
	}
	snapshots = listHistorySnapshots(t, dir, name)
	if len(snapshots) != 7 {
		t.Fatalf("got %d snapshots, want 7", len(snapshots))
	}
	for i, snapshot := range snapshots {
		if got := read(filepath.Join(history, snapshot)); got != fmt.Sprint(i+3) {
			t.Fatalf("snapshot %d = %q, want revision %d", i, got, i+3)
		}
	}
}

func TestOrdinaryTokenStorageHistoryBestEffort(t *testing.T) {
	for _, blocked := range []bool{false, true} {
		dir := t.TempDir()
		const name = "fake.json"
		path := filepath.Join(dir, name)
		history := filepath.Join(dir, authFileHistoryDirName)
		if blocked {
			for _, file := range []string{path, history} {
				if err := os.WriteFile(file, []byte("fixture"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
		}
		writer := storageHistoryWriter(func(got string) error {
			if got != path {
				t.Fatalf("writer received %q, want %q", got, path)
			}
			return os.WriteFile(got, []byte("updated"), 0o600)
		})
		store := NewFileTokenStore().WithBaseDir(dir)
		saved, err := store.Save(context.Background(), &cliproxyauth.Auth{ID: name, Provider: "fixture", Storage: writer})
		if err != nil || saved != path {
			t.Fatalf("save returned (%q, %v)", saved, err)
		}
		if got, err := os.ReadFile(path); err != nil || string(got) != "updated" {
			t.Fatal("writer did not save bytes")
		}
		if !blocked {
			if _, err := os.Stat(history); !os.IsNotExist(err) {
				t.Fatalf("unexpected history for first write: %v", err)
			}
		}
	}
}
