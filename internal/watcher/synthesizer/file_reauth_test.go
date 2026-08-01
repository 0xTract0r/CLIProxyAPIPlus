package synthesizer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// TestSynthesizeFileAuths_RestoresReauthRequiredStateOnColdLoad covers the live
// file-watcher synthesizer's side of the reauth_required persistence gap. The
// synthesizer runs on every file-watcher event (a *second* loader independent
// from sdk/auth/filestore.go's readAuthFiles); before the fix it rebuilt a dead
// refresh token as a fresh StatusActive credential every ~5 minutes, undoing
// the cold-load restore and re-exposing a false-green account.
func TestSynthesizeFileAuths_RestoresReauthRequiredStateOnColdLoad(t *testing.T) {
	tempDir := t.TempDir()
	fullPath := filepath.Join(tempDir, "claude-auth.json")
	authData := map[string]any{
		"type":                    "claude",
		"email":                   "reauth@example.com",
		"access_token":            "token-value",
		"refresh_disabled":        true,
		"refresh_status":          "reauth_required",
		"refresh_error_code":      "invalid_grant",
		"refresh_disabled_reason": "reauth_required",
		"reauth_required":         true,
		"refresh_disabled_at":     time.Now().UTC().Format(time.RFC3339),
	}
	data, errMarshal := json.Marshal(authData)
	if errMarshal != nil {
		t.Fatalf("marshal auth data: %v", errMarshal)
	}
	if errWrite := os.WriteFile(fullPath, data, 0644); errWrite != nil {
		t.Fatalf("write auth file: %v", errWrite)
	}

	ctx := &SynthesisContext{
		Config:      &config.Config{},
		AuthDir:     tempDir,
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}

	auths := SynthesizeAuthFile(ctx, fullPath, data)
	if len(auths) != 1 {
		t.Fatalf("SynthesizeAuthFile() len = %d, want 1", len(auths))
	}
	got := auths[0]
	if got.Status != coreauth.StatusError {
		t.Fatalf("Status = %q, want %q (disk reauth_required must survive live synthesis)", got.Status, coreauth.StatusError)
	}
	if got.StatusMessage != "reauth_required" {
		t.Fatalf("StatusMessage = %q, want %q", got.StatusMessage, "reauth_required")
	}
	if !got.Unavailable {
		t.Fatalf("Unavailable = false, want true (mirrors the abnormal terminal state)")
	}
}

// TestSynthesizeFileAuths_DisabledTakesPriorityOverReauthRequiredStatus proves
// the priority ordering (disabled > reauth_required): an operator-disabled
// credential that also carries the reauth lock keeps StatusDisabled for
// display, while still being blocked either way.
func TestSynthesizeFileAuths_DisabledTakesPriorityOverReauthRequiredStatus(t *testing.T) {
	tempDir := t.TempDir()
	fullPath := filepath.Join(tempDir, "claude-auth.json")
	authData := map[string]any{
		"type":            "claude",
		"email":           "disabled-reauth@example.com",
		"access_token":    "token-value",
		"disabled":        true,
		"reauth_required": true,
		"refresh_status":  "reauth_required",
	}
	data, errMarshal := json.Marshal(authData)
	if errMarshal != nil {
		t.Fatalf("marshal auth data: %v", errMarshal)
	}
	if errWrite := os.WriteFile(fullPath, data, 0644); errWrite != nil {
		t.Fatalf("write auth file: %v", errWrite)
	}

	ctx := &SynthesisContext{
		Config:      &config.Config{},
		AuthDir:     tempDir,
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}

	auths := SynthesizeAuthFile(ctx, fullPath, data)
	if len(auths) != 1 {
		t.Fatalf("SynthesizeAuthFile() len = %d, want 1", len(auths))
	}
	got := auths[0]
	if !got.Disabled {
		t.Fatalf("Disabled = false, want true")
	}
	if got.Status != coreauth.StatusDisabled {
		t.Fatalf("Status = %q, want %q (disabled takes display precedence over reauth_required)", got.Status, coreauth.StatusDisabled)
	}
}

// TestSynthesizeFileAuths_HealthyRecordStaysActive is the negative guard: a
// healthy Claude auth file with no reauth lock keys must synthesize as
// StatusActive, so the read-back never over-blocks ordinary credentials.
func TestSynthesizeFileAuths_HealthyRecordStaysActive(t *testing.T) {
	tempDir := t.TempDir()
	fullPath := filepath.Join(tempDir, "claude-auth.json")
	authData := map[string]any{
		"type":         "claude",
		"email":        "healthy@example.com",
		"access_token": "token-value",
	}
	data, errMarshal := json.Marshal(authData)
	if errMarshal != nil {
		t.Fatalf("marshal auth data: %v", errMarshal)
	}
	if errWrite := os.WriteFile(fullPath, data, 0644); errWrite != nil {
		t.Fatalf("write auth file: %v", errWrite)
	}

	ctx := &SynthesisContext{
		Config:      &config.Config{},
		AuthDir:     tempDir,
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}

	auths := SynthesizeAuthFile(ctx, fullPath, data)
	if len(auths) != 1 {
		t.Fatalf("SynthesizeAuthFile() len = %d, want 1", len(auths))
	}
	got := auths[0]
	if got.Status != coreauth.StatusActive {
		t.Fatalf("Status = %q, want %q", got.Status, coreauth.StatusActive)
	}
	if got.Unavailable {
		t.Fatalf("Unavailable = true, want false for a healthy record")
	}
}
