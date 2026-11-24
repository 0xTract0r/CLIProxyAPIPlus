package management

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestReadOAuthCallbackFileWaitsOnPartialWrite: a half-written callback file
// must not be consumed. It must report "not ready", stay on disk, and be
// accepted once the complete payload is present.
func TestReadOAuthCallbackFileWaitsOnPartialWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".oauth-codex-state123.oauth")
	if err := os.WriteFile(path, []byte(`{"code":"abc","sta`), 0o600); err != nil {
		t.Fatalf("write partial: %v", err)
	}
	if payload, ok := readOAuthCallbackFile(path); ok {
		t.Fatalf("partial callback file consumed as %v; want not-ready", payload)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("partial callback file was removed: %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"code":"abc","state":"state123","error":""}`), 0o600); err != nil {
		t.Fatalf("write complete: %v", err)
	}
	payload, ok := readOAuthCallbackFile(path)
	if !ok || payload["state"] != "state123" || payload["code"] != "abc" {
		t.Fatalf("complete callback file = %v ok=%v, want code/state", payload, ok)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("consumed callback file still present: %v", err)
	}
}

// TestWriteOAuthCallbackFileIsAtomic: while callback files are being written,
// a concurrent reader polling the final path must only ever observe "absent"
// or a complete, parseable payload, and no temp files may be left behind.
func TestWriteOAuthCallbackFileIsAtomic(t *testing.T) {
	authDir := t.TempDir()
	state := "atomicstate0123456789"
	finalPath := filepath.Join(authDir, ".oauth-codex-"+state+".oauth")
	longCode := strings.Repeat("c", 64*1024)

	var stop atomic.Bool
	var partial atomic.Int32
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for !stop.Load() {
			data, err := os.ReadFile(finalPath)
			if err != nil {
				continue
			}
			var payload map[string]string
			if json.Unmarshal(data, &payload) != nil || payload["state"] != state || payload["code"] != longCode {
				partial.Add(1)
			}
		}
	}()
	for i := 0; i < 200; i++ {
		if _, err := writeOAuthCallbackFile(authDir, "codex", state, longCode, ""); err != nil {
			stop.Store(true)
			wg.Wait()
			t.Fatalf("write callback file: %v", err)
		}
	}
	time.Sleep(10 * time.Millisecond)
	stop.Store(true)
	wg.Wait()
	if n := partial.Load(); n != 0 {
		t.Fatalf("reader observed %d partial/invalid callback payload(s) at the final path", n)
	}
	entries, err := os.ReadDir(authDir)
	if err != nil {
		t.Fatalf("read auth dir: %v", err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".tmp-") {
			t.Fatalf("temp file left behind: %s", entry.Name())
		}
	}
	info, err := os.Stat(finalPath)
	if err != nil {
		t.Fatalf("stat final callback file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("callback file perm = %o, want 600", perm)
	}
}
