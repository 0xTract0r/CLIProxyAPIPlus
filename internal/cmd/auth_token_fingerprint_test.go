package cmd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunAuthTokenFingerprintRedactsTokenValues(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "codex-user@example.com-plus.json")
	content := `{
  "type": "codex",
  "email": "user@example.com",
  "account_id": "acct_123",
  "access_token": "access-secret-value",
  "refresh_token": "refresh-secret-value",
  "last_refresh": "2026-05-28T10:00:00Z",
  "expired": "2026-05-28T11:00:00Z",
  "account_settings": {"refresh_enabled": false}
}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write auth fixture: %v", err)
	}

	var out bytes.Buffer
	err := RunAuthTokenFingerprint(context.Background(), &out, AuthTokenFingerprintOptions{
		Paths:     []string{dir},
		Provider:  "codex",
		Recursive: true,
		Format:    "tsv",
	})
	if err != nil {
		t.Fatalf("RunAuthTokenFingerprint() error = %v", err)
	}
	got := out.String()
	if strings.Contains(got, "access-secret-value") || strings.Contains(got, "refresh-secret-value") {
		t.Fatalf("output leaked token values: %s", got)
	}
	if !strings.Contains(got, shortHashForTest("access-secret-value")) {
		t.Fatalf("output missing access token fingerprint: %s", got)
	}
	if !strings.Contains(got, shortHashForTest("refresh-secret-value")) {
		t.Fatalf("output missing refresh token fingerprint: %s", got)
	}
	if !strings.Contains(got, "false") {
		t.Fatalf("output missing refresh_enabled flag: %s", got)
	}
}

func TestRunAuthTokenFingerprintJSONL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "codex-access-token-only.json")
	content := `{"type":"codex","access_token":"access-only","refresh_disabled":true}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write auth fixture: %v", err)
	}

	var out bytes.Buffer
	err := RunAuthTokenFingerprint(context.Background(), &out, AuthTokenFingerprintOptions{
		Paths:     []string{path},
		Provider:  "codex",
		Recursive: true,
		Format:    "jsonl",
	})
	if err != nil {
		t.Fatalf("RunAuthTokenFingerprint() error = %v", err)
	}
	got := out.String()
	if strings.Contains(got, "access-only") {
		t.Fatalf("JSONL output leaked token value: %s", got)
	}
	if !strings.Contains(got, `"has_refresh_token":false`) {
		t.Fatalf("JSONL output missing refresh-token absence: %s", got)
	}
	if !strings.Contains(got, `"refresh_disabled":"true"`) {
		t.Fatalf("JSONL output missing refresh_disabled flag: %s", got)
	}
}

func shortHashForTest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:16]
}
