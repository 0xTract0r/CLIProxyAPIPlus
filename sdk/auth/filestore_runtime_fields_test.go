package auth

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFileTokenStoreListRestoresRuntimeProxyURL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "codex-proxy.json")
	if err := os.WriteFile(path, []byte(`{"type":"codex","email":"proxy@example.test","proxy_url":"socks5://proxy.example:1080","headers":{"User-Agent":"managed-ua/1.0"}}`), 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}

	store := NewFileTokenStore()
	store.SetBaseDir(dir)
	auths, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("list auths: %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("auth count = %d, want 1", len(auths))
	}
	auth := auths[0]
	if got := auth.ProxyURL; got != "socks5://proxy.example:1080" {
		t.Fatalf("ProxyURL = %q, want %q", got, "socks5://proxy.example:1080")
	}
	if got := auth.Attributes["header:User-Agent"]; got != "managed-ua/1.0" {
		t.Fatalf("header:User-Agent = %q, want managed header", got)
	}
}

func TestFileTokenStoreListSkipsIncidentArchives(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"claude-live.json": `{"type":"claude","email":"live@example.test"}`,
		filepath.Join("incident-archives", "claude-live.pre-reauth.json"): `{"type":"claude","email":"archive@example.test"}`,
		filepath.Join("nested", "codex-live.json"):                        `{"type":"codex","email":"nested@example.test"}`,
	}
	for name, body := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatalf("create dir for %s: %v", name, err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	store := NewFileTokenStore()
	store.SetBaseDir(dir)
	auths, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("list auths: %v", err)
	}

	got := make(map[string]bool, len(auths))
	for _, auth := range auths {
		got[auth.ID] = true
		if strings.Contains(auth.ID, "incident-archives") {
			t.Fatalf("List returned archived auth ID %q", auth.ID)
		}
		if auth.Attributes["email"] == "archive@example.test" {
			t.Fatalf("List returned archived auth email for ID %q", auth.ID)
		}
	}
	if !got["claude-live.json"] {
		t.Fatalf("List missing live root auth: %#v", got)
	}
	if !got[filepath.Join("nested", "codex-live.json")] {
		t.Fatalf("List missing live nested auth: %#v", got)
	}
	if len(got) != 2 {
		t.Fatalf("auth count = %d, want 2: %#v", len(got), got)
	}
}
