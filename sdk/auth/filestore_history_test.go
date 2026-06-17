package auth

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
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
