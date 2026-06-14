package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

// TestClaudeExecutor_CountTokensNormalizesAccountEnvWhenEnabled covers requirement ⑦:
// the count_tokens path must run NormalizeAccountEnv with the same switch gating as
// the main messages path (applyCloaking), so the real cwd inside <env> blocks is
// rewritten to the per-account canonical path before the body is sent upstream.
// Before the fix the count_tokens path skipped normalization and leaked the real cwd.
func TestClaudeExecutor_CountTokensNormalizesAccountEnvWhenEnabled(t *testing.T) {
	resetClaudeDeviceProfileCache()

	var capturedBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "count_tokens") {
			t.Errorf("unexpected upstream path: %s", r.URL.Path)
		}
		capturedBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"input_tokens":7}`))
	}))
	defer server.Close()

	on := true
	executor := NewClaudeExecutor(&config.Config{
		AuthDir:             t.TempDir(),
		NormalizeAccountEnv: &on,
	})
	auth := &cliproxyauth.Auth{
		ID: "acct-ct",
		Attributes: map[string]string{
			"api_key":  "key-ct",
			"base_url": server.URL,
		},
	}

	payload := []byte(`{
		"model": "claude-sonnet-4-5",
		"system": [
			{"type": "text", "text": "You are Claude.\n<env>\nWorking directory: /Users/realdev/Project/secret\n</env>"}
		],
		"messages": [
			{"role": "user", "content": [{"type": "text", "text": "hello"}]}
		]
	}`)

	if _, err := executor.CountTokens(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-sonnet-4-5",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("claude"),
	}); err != nil {
		t.Fatalf("CountTokens() error = %v", err)
	}

	if capturedBody == nil {
		t.Fatal("upstream never received a count_tokens request body")
	}
	// checkSystemInstructionsWithVersion reshapes the system/messages layout
	// (it moves the original <env> block into a system-reminder), so assert over
	// the whole serialized body rather than a fixed JSON path.
	body := string(capturedBody)
	if strings.Contains(body, "/Users/realdev") {
		t.Fatalf("count_tokens must normalize cwd when switch is on, got %q", body)
	}
	canonical := helps.AccountCanonicalCwd(auth, "key-ct")
	if !strings.Contains(body, canonical) {
		t.Fatalf("expected canonical cwd %q in count_tokens body, got %q", canonical, body)
	}
}

// TestClaudeExecutor_CountTokensLeavesEnvUntouchedWhenDisabled is the off-switch
// guard for requirement ⑦: with the global switch off (default), the count_tokens
// body must keep the real cwd byte-for-byte (zero behavior change).
func TestClaudeExecutor_CountTokensLeavesEnvUntouchedWhenDisabled(t *testing.T) {
	resetClaudeDeviceProfileCache()

	var capturedBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"input_tokens":7}`))
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{AuthDir: t.TempDir()}) // switch unset -> off
	auth := &cliproxyauth.Auth{
		ID: "acct-ct-off",
		Attributes: map[string]string{
			"api_key":  "key-ct-off",
			"base_url": server.URL,
		},
	}

	payload := []byte(`{
		"model": "claude-sonnet-4-5",
		"system": [
			{"type": "text", "text": "You are Claude.\n<env>\nWorking directory: /Users/realdev/Project/secret\n</env>"}
		],
		"messages": [
			{"role": "user", "content": [{"type": "text", "text": "hello"}]}
		]
	}`)

	if _, err := executor.CountTokens(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-sonnet-4-5",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("claude"),
	}); err != nil {
		t.Fatalf("CountTokens() error = %v", err)
	}

	if capturedBody == nil {
		t.Fatal("upstream never received a count_tokens request body")
	}
	body := string(capturedBody)
	if !strings.Contains(body, "/Users/realdev/Project/secret") {
		t.Fatalf("switch off must leave count_tokens cwd untouched, got %q", body)
	}
}

// TestApplyClaudeHeaders_NonStructuredOperatorXAppCannotLeakNonCli covers requirement ⑥:
// on the non-structured managed-header path, an operator header:X-App override to a
// non-cli value (e.g. "browser") must not leak. X-App is the de-anonymization anchor
// and is pinned to "cli" on both the structured and non-structured paths. Before the
// fix this path let header:X-App=browser overwrite the forced "cli".
func TestApplyClaudeHeaders_NonStructuredOperatorXAppCannotLeakNonCli(t *testing.T) {
	resetClaudeDeviceProfileCache()

	req := newClaudeHeaderTestRequest(t, http.Header{
		"X-App": []string{"cli"},
	})
	// Attrs-only auth (no account_settings metadata) -> non-structured path. The
	// operator tries to override X-App to a non-cli value through header:X-App.
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"api_key":      "key-xapp-nonstruct",
			"header:X-App": "browser",
		},
	}
	if cliproxyauth.HasStructuredAccountSettingsMetadata(auth) {
		t.Fatal("test setup error: auth should take the non-structured managed-header path")
	}

	applyClaudeHeaders(req, auth, "key-xapp-nonstruct", false, nil, nil)

	if got := req.Header.Get("X-App"); got != "cli" {
		t.Fatalf("X-App = %q, want %q (operator header:X-App must not leak a non-cli value)", got, "cli")
	}
}

// TestApplyClaudeHeaders_NonStructuredOperatorOtherHeaderStillOverrides confirms the
// ⑥ fix only pins X-App: other managed headers on the non-structured path still
// honor the operator header:<name> override.
func TestApplyClaudeHeaders_NonStructuredOperatorOtherHeaderStillOverrides(t *testing.T) {
	resetClaudeDeviceProfileCache()

	req := newClaudeHeaderTestRequest(t, http.Header{
		"X-App": []string{"cli"},
	})
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"api_key":                    "key-other-nonstruct",
			"header:X-Stainless-Timeout": "123",
		},
	}
	if cliproxyauth.HasStructuredAccountSettingsMetadata(auth) {
		t.Fatal("test setup error: auth should take the non-structured managed-header path")
	}

	applyClaudeHeaders(req, auth, "key-other-nonstruct", false, nil, nil)

	if got := req.Header.Get("X-Stainless-Timeout"); got != "123" {
		t.Fatalf("X-Stainless-Timeout = %q, want %q (operator override must still apply)", got, "123")
	}
	if got := req.Header.Get("X-App"); got != "cli" {
		t.Fatalf("X-App = %q, want %q", got, "cli")
	}
}
