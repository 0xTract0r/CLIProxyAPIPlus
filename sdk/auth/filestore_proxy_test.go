package auth

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestFileTokenStoreList_LoadsProxyURLAndPrefix verifies that cold-start file loading
// populates ProxyURL and Prefix on the runtime Auth struct from metadata.
func TestFileTokenStoreList_LoadsProxyURLAndPrefix(t *testing.T) {
	dir := t.TempDir()
	store := NewFileTokenStore()
	store.SetBaseDir(dir)

	content := []byte(`{
		"type": "antigravity",
		"proxy_url": "http://127.0.0.1:7890",
		"prefix": "custom-prefix",
		"project_id": "test-project",
		"access_token": "ya29.test-token"
	}`)
	err := os.WriteFile(filepath.Join(dir, "antigravity.json"), content, 0o600)
	if err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	auths, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List error: %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("got %d auths, want 1", len(auths))
	}
	if auths[0].ProxyURL != "http://127.0.0.1:7890" {
		t.Fatalf("ProxyURL = %q, want http://127.0.0.1:7890", auths[0].ProxyURL)
	}
	if auths[0].Prefix != "custom-prefix" {
		t.Fatalf("Prefix = %q, want custom-prefix", auths[0].Prefix)
	}
}

// TestFileTokenStoreList_DoesNotBlockColdStartWithUnproxiedNetworkRequest verifies that
// loading auth files on cold start does not trigger unproxied network requests (e.g. FetchAntigravityProjectID).
func TestFileTokenStoreList_DoesNotBlockColdStartWithUnproxiedNetworkRequest(t *testing.T) {
	called := false
	origTransport := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = origTransport })
	http.DefaultTransport = stubRoundTripper(func(*http.Request) (*http.Response, error) {
		called = true
		return nil, errors.New("cold load tried to issue a network request")
	})

	dir := t.TempDir()
	store := NewFileTokenStore()
	store.SetBaseDir(dir)

	// Auth file without project_id, but with proxy_url
	content := []byte(`{
		"type": "antigravity",
		"proxy_url": "http://127.0.0.1:9999",
		"access_token": "ya29.test-token"
	}`)
	err := os.WriteFile(filepath.Join(dir, "antigravity.json"), content, 0o600)
	if err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	start := time.Now()
	auths, err := store.List(context.Background())
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("List error: %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("got %d auths, want 1", len(auths))
	}
	if auths[0].ProxyURL != "http://127.0.0.1:9999" {
		t.Fatalf("ProxyURL = %q, want http://127.0.0.1:9999", auths[0].ProxyURL)
	}
	if called {
		t.Fatal("cold load made an HTTP request")
	}
	// Cold start file load must be fast and not perform outbound network I/O
	if elapsed > 2*time.Second {
		t.Fatalf("List took %v, expected < 2s (should not perform blocking network calls during file load)", elapsed)
	}
}
