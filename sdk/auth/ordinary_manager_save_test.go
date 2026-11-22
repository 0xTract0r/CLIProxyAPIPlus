package auth

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestIndependentManagerSaveDirectoryIsPerCall(t *testing.T) {
	base, a, b := t.TempDir(), t.TempDir(), t.TempDir()
	store := NewFileTokenStore()
	store.SetBaseDir(base)
	m := NewManager(store)
	expectedA, expectedB := filepath.Join(a, "same.json"), filepath.Join(b, "same.json")
	type result struct {
		got, want string
		err       error
	}
	results := make(chan result, 12)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 12; i++ {
		dir, label, want := a, "first fixture", expectedA
		if i%2 == 1 {
			dir, label, want = b, "second fixture", expectedB
		}
		wg.Go(func() {
			<-start
			saved, err := m.SaveAuth(testLocalFileRecord("same.json", label), &config.Config{AuthDir: dir})
			results <- result{saved, want, err}
		})
	}
	close(start)
	wg.Wait()
	close(results)
	for r := range results {
		if r.err != nil || r.got != r.want {
			t.Errorf("save=%q want=%q err=%v", r.got, r.want, r.err)
		}
	}
	if store.baseDirSnapshot() != base {
		t.Error("explicit saves changed the shared file store default directory")
	}
	saved, err := m.SaveAuth(testLocalFileRecord("default.json", "third fixture"), nil)
	if err != nil || saved != filepath.Join(base, "default.json") {
		t.Errorf("nil-config save=%q, %v", saved, err)
	}
	records, err := store.List(context.Background())
	if err != nil || len(records) != 1 || records[0].Label != "third fixture" {
		t.Errorf("plain List no longer reads configured default: count=%d err=%v", len(records), err)
	}
	for _, item := range []struct{ path, label string }{{expectedA, "first fixture"}, {expectedB, "second fixture"}} {
		raw, err := os.ReadFile(item.path)
		var metadata struct {
			Label string `json:"label"`
		}
		decodeErr := json.Unmarshal(raw, &metadata)
		if err != nil || decodeErr != nil || metadata.Label != item.label {
			t.Errorf("scope lost ordinary fixture label: %v (%v)", err, decodeErr)
		}
	}
}
