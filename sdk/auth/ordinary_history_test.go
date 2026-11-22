package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// listHistorySnapshots returns the snapshot file names for baseName in the history
// subdirectory of dir.
func listHistorySnapshots(t *testing.T, dir, baseName string) []string {
	t.Helper()
	historyDir := filepath.Join(dir, authFileHistoryDirName)
	entries, err := os.ReadDir(historyDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read history dir: %v", err)
	}
	prefix := baseName + "."
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasPrefix(name, prefix) && strings.HasSuffix(name, ".bak") {
			names = append(names, name)
		}
	}
	return names
}

func TestFileTokenStore_Save_RollingWholeFileSnapshots(t *testing.T) {
	ctx := context.Background()
	baseDir := t.TempDir()
	baseName := "account.json"
	path := filepath.Join(baseDir, baseName)

	store := NewFileTokenStore()
	store.SetBaseDir(baseDir)

	// Seed an initial file so the first Save has a prior version to snapshot.
	if err := os.WriteFile(path, []byte(`{"type":"test","rev":0}`), 0o600); err != nil {
		t.Fatalf("seed auth file: %v", err)
	}

	// Perform 10 distinct writes. Each Save snapshots the prior on-disk content, so
	// after the loop more than 7 versions have existed but only the 7 most recent
	// snapshots must be retained.
	for rev := 1; rev <= 10; rev++ {
		auth := &cliproxyauth.Auth{
			ID:       baseName,
			Provider: "test",
			FileName: baseName,
			Metadata: map[string]any{"type": "test", "rev": float64(rev)},
		}
		if _, err := store.Save(ctx, auth); err != nil {
			t.Fatalf("Save() rev %d error: %v", rev, err)
		}
	}

	snapshots := listHistorySnapshots(t, baseDir, baseName)
	if len(snapshots) != authFileHistoryKeep {
		t.Fatalf("snapshot count = %d, want %d (rolling window)", len(snapshots), authFileHistoryKeep)
	}

	// History directory must be permission-tightened to owner-only access.
	historyInfo, err := os.Stat(filepath.Join(baseDir, authFileHistoryDirName))
	if err != nil {
		t.Fatalf("stat history dir: %v", err)
	}
	if perm := historyInfo.Mode().Perm(); perm != 0o700 {
		t.Fatalf("history dir perm = %o, want 0700", perm)
	}

	// Snapshots are stored as whole files; the newest snapshot must hold the content
	// that was on disk right before the final write (rev 9), proving full-file capture.
	historyDir := filepath.Join(baseDir, authFileHistoryDirName)
	newest := snapshots[0]
	for _, name := range snapshots {
		if name > newest {
			newest = name
		}
	}
	newestData, err := os.ReadFile(filepath.Join(historyDir, newest))
	if err != nil {
		t.Fatalf("read newest snapshot: %v", err)
	}
	if want := fmt.Sprintf(`"rev":%d`, 9); !strings.Contains(string(newestData), want) {
		t.Fatalf("newest snapshot = %s, want it to contain %q", string(newestData), want)
	}
}

func TestFileTokenStore_Save_NoSnapshotWhenNoPriorFile(t *testing.T) {
	ctx := context.Background()
	baseDir := t.TempDir()
	baseName := "fresh.json"

	store := NewFileTokenStore()
	store.SetBaseDir(baseDir)

	auth := &cliproxyauth.Auth{
		ID:       baseName,
		Provider: "test",
		FileName: baseName,
		Metadata: map[string]any{"type": "test"},
	}
	if _, err := store.Save(ctx, auth); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	if snapshots := listHistorySnapshots(t, baseDir, baseName); len(snapshots) != 0 {
		t.Fatalf("snapshot count = %d, want 0 when there was no prior file", len(snapshots))
	}
}

func TestOrdinaryFileStoreHistoryConcurrentViews(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	record := func(rev int) *cliproxyauth.Auth {
		return &cliproxyauth.Auth{ID: "fixture.json", Provider: "test", Metadata: map[string]any{"type": "test", "rev": rev}}
	}
	one, two, other := NewFileTokenStore().WithBaseDir(a), NewFileTokenStore().WithBaseDir(a), NewFileTokenStore().WithBaseDir(b)
	ctx := context.Background()
	if _, err := one.Save(ctx, record(0)); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	errors := make(chan error, 3)
	for i, store := range []*FileTokenStore{one, two, other} {
		go func() {
			<-start
			_, err := store.Save(ctx, record(i+1))
			errors <- err
		}()
	}
	close(start)
	for range 3 {
		if err := <-errors; err != nil {
			t.Fatal(err)
		}
	}
	snapshots := listHistorySnapshots(t, a, "fixture.json")
	if len(snapshots) != 2 || len(listHistorySnapshots(t, b, "fixture.json")) != 0 {
		t.Fatalf("wrong history counts, A=%d", len(snapshots))
	}
	read := func(path string) int {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var value struct {
			Rev int `json:"rev"`
		}
		if err := json.Unmarshal(data, &value); err != nil {
			t.Fatal(err)
		}
		return value.Rev
	}
	final := read(filepath.Join(a, "fixture.json"))
	if first, second := read(filepath.Join(a, authFileHistoryDirName, snapshots[0])), read(filepath.Join(a, authFileHistoryDirName, snapshots[1])); first != 0 || second != 3-final || read(filepath.Join(b, "fixture.json")) != 3 {
		t.Fatalf("history does not preserve the immediate predecessor: %d,%d -> %d", first, second, final)
	}
	if _, err := one.Save(ctx, record(final)); err != nil {
		t.Fatal(err)
	}
	bad := record(4)
	bad.Metadata["invalid"] = make(chan int)
	if _, err := two.Save(ctx, bad); err == nil {
		t.Fatal("serialization should fail")
	}
	if len(listHistorySnapshots(t, a, "fixture.json")) != 2 || read(filepath.Join(a, "fixture.json")) != final {
		t.Fatal("no-op or failed save changed file or history")
	}
}

func TestOrdinaryFileStoreCannotReplaceWithoutDirectoryWriteAccess(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("mode permission checks require a non-root test user")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "fixture.json")
	before := []byte(`{"type":"test","rev":1}`)
	if err := os.WriteFile(path, before, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	store := NewFileTokenStore().WithBaseDir(dir)
	auth := &cliproxyauth.Auth{ID: "fixture.json", Provider: "test", Metadata: map[string]any{"type": "test", "rev": 2}}
	_, saveErr := store.Save(context.Background(), auth)
	after, err := os.ReadFile(path)
	if saveErr == nil || err != nil || string(after) != string(before) {
		t.Fatalf("failed atomic save changed prior bytes or reported success: save=%v read=%v preserved=%t", saveErr, err, string(after) == string(before))
	}
	if len(listHistorySnapshots(t, dir, "fixture.json")) != 0 {
		t.Fatal("failed save added a snapshot")
	}
}

func TestOrdinaryFileStoreHistoryIsPrivateAndOutsideList(t *testing.T) {
	record := func(rev int) *cliproxyauth.Auth {
		return &cliproxyauth.Auth{ID: "fixture.json", Provider: "test", Metadata: map[string]any{"type": "test", "rev": rev}}
	}
	for _, name := range []string{"private", "wide", "alias"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			history := filepath.Join(dir, authFileHistoryDirName)
			target := history
			switch name {
			case "alias":
				target = t.TempDir()
				if err := os.Symlink(target, history); err != nil {
					t.Fatal(err)
				}
			case "private", "wide":
				if err := os.Mkdir(history, 0o700); err != nil {
					t.Fatal(err)
				}
				if name == "wide" {
					if err := os.Chmod(history, 0o755); err != nil {
						t.Fatal(err)
					}
				}
			}
			ctx := context.Background()
			store := NewFileTokenStore().WithBaseDir(dir)
			for rev := 0; rev < 2; rev++ {
				if _, err := store.Save(ctx, record(rev)); err != nil {
					t.Fatal(err)
				}
			}
			entries, err := os.ReadDir(target)
			wantBackups := 0
			if name == "private" {
				wantBackups = 1
			}
			if err != nil || len(entries) != wantBackups {
				t.Fatalf("history entries=%d want=%d err=%v", len(entries), wantBackups, err)
			}
			if name == "private" {
				if err := os.Chmod(history, 0); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(history, 0o700) })
			}
			list, err := store.List(ctx)
			if err != nil || len(list) != 1 || list[0].ID != "fixture.json" {
				t.Fatalf("history changed normal List: entries=%d err=%v", len(list), err)
			}
			if err := store.Delete(ctx, "fixture.json"); err != nil {
				t.Fatal(err)
			}
			remaining, err := store.List(ctx)
			if err != nil || len(remaining) != 0 {
				t.Fatalf("history reappeared after Delete: entries=%d err=%v", len(remaining), err)
			}
		})
	}
}
