package executor

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	codexauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codex"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestOrdinaryCodexRefreshHonorsCurrentFileSettings(t *testing.T) {
	for _, tc := range []struct {
		name   string
		extra  string
		paused bool
	}{
		{"current file pauses refresh", `"account_settings":{"refresh_enabled":false},`, true},
		{"current file disables credential", `"disabled":true,`, true},
		{"valid grant preserves large integers", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "credential.json")
			original := []byte(`{` + tc.extra + `"account_id":"fixture-account","refresh_token":"fixture-refresh","access_token":"fixture-access","unknown":{"meter":9007199254740993}}`)
			if err := os.WriteFile(path, original, 0600); err != nil {
				t.Fatal(err)
			}
			record := &coreauth.Auth{ID: "credential.json", Provider: "codex", FileName: "credential.json", Metadata: map[string]any{
				"account_id": "fixture-account", "refresh_token": "fixture-refresh", "access_token": "fixture-access",
			}}
			calls := 0
			fake := ordinaryCodexGrantFunc(func(context.Context, string) (*codexauth.CodexTokenData, error) {
				calls++
				return &codexauth.CodexTokenData{AccessToken: "fixture-next", RefreshToken: "fixture-next-refresh", AccountID: "fixture-account"}, nil
			})
			ctx := context.WithValue(t.Context(), ordinaryCodexGrantKey{}, fake)
			updated, err := NewCodexExecutor(&config.Config{AuthDir: dir}).Refresh(ctx, record)
			onDisk, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if tc.paused {
				if calls != 0 || !errors.Is(err, coreauth.ErrSelectedRefreshUnavailable) || !bytes.Equal(onDisk, original) {
					t.Fatalf("paused disk credential: grant calls=%d err=%v unchanged=%t", calls, err, bytes.Equal(onDisk, original))
				}
				return
			}
			if err != nil || updated == nil || calls != 1 || updated.Metadata["access_token"] != "fixture-next" ||
				!bytes.Contains(onDisk, []byte("9007199254740993")) {
				t.Fatalf("valid grant: calls=%d err=%v precise=%t", calls, err, bytes.Contains(onDisk, []byte("9007199254740993")))
			}
		})
	}
}

func TestOrdinaryCodexRefreshWithoutFileBinding(t *testing.T) {
	dir := t.TempDir()
	record := &coreauth.Auth{ID: "memory-only", Provider: "codex", Metadata: map[string]any{
		"account_id": "fixture-account", "refresh_token": "fixture-refresh",
	}}
	calls := 0
	grant := ordinaryCodexGrantFunc(func(_ context.Context, token string) (*codexauth.CodexTokenData, error) {
		calls++
		if token != "fixture-refresh" {
			t.Fatal("unexpected grant")
		}
		return &codexauth.CodexTokenData{AccessToken: "fixture-updated", AccountID: "fixture-account"}, nil
	})
	ctx := context.WithValue(t.Context(), ordinaryCodexGrantKey{}, grant)
	updated, err := NewCodexExecutor(&config.Config{AuthDir: dir}).Refresh(ctx, record)
	if err != nil || calls != 1 || updated == nil || updated.Metadata["access_token"] != "fixture-updated" {
		t.Fatalf("memory credential refresh: calls=%d err=%v", calls, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("memory credential wrote files: count=%d err=%v", len(entries), err)
	}
}

func TestOrdinaryClaudePausedRefreshIsANoop(t *testing.T) {
	auth := &coreauth.Auth{Provider: "claude", Metadata: map[string]any{"refresh_token": "fixture-refresh", "account_settings": map[string]any{"refresh_enabled": false}}}
	got, err := NewClaudeExecutor(&config.Config{}).Refresh(t.Context(), auth)
	if err != nil || got != auth {
		t.Fatalf("paused grant did not return the original credential: %v", err)
	}
}
