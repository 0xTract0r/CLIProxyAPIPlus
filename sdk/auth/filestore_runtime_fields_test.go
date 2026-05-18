package auth

import (
	"context"
	"os"
	"path/filepath"
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
