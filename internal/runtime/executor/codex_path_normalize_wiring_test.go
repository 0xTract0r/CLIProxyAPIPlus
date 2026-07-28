package executor

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

// anticorrTruePtr returns a *bool set to true, used to turn the (production-dormant)
// normalize-account-env switch ON at the Config level so the gated codex path
// normalization / restore wiring actually fires. LoadConfig neutralizes this
// pointer to nil in production, but config.NormalizeAccountEnvEnabled still honors a
// directly-constructed Config (by design, so these wiring guards can exercise the
// dormant-but-not-deleted normalize/restore implementations end to end).
func anticorrTruePtr() *bool { b := true; return &b }

const (
	// codexWiringRealCwd is a distinctive real cwd sentinel that must never reach
	// the codex upstream once normalization is on. It is placed as the primary cwd
	// (turn-metadata workspaces KEY) so it maps to helps.AccountCanonicalCwd.
	codexWiringRealCwd = "/Users/guardsecret/Project/anticorr-f3f4-cwd"
)

// codexWiringBody builds a codex /responses body that leaks the real cwd in the
// turn-metadata workspaces KEY (authoritative primary cwd) and in the
// environment_context <cwd> block inside input text — both channels the fork's
// normalizeCodexPaths is responsible for scrubbing.
func codexWiringBody() []byte {
	turnMetadata := `{"session_id":"019edf57-9c1e-78b3-860a-c6ff641bdeac",` +
		`"workspaces":{"` + codexWiringRealCwd + `":{"associated_remote_urls":{"origin":"git@github.com:guardsecret/secret.git"},` +
		`"latest_git_commit_hash":"e2b18565b7d477866f1bb502d3c017f129f4f03d","has_changes":true}}}`
	envCtx := "<environment_context>\n  <cwd>" + codexWiringRealCwd + "</cwd>\n  <shell>zsh</shell>\n" +
		"  <filesystem><workspace_roots><root>" + codexWiringRealCwd + "</root></workspace_roots></filesystem>\n</environment_context>"
	inputText := "User request.\n\n" + envCtx
	// Build with sjson-safe raw JSON string assembly via a plain literal.
	body := `{"model":"gpt-5.4-mini",` +
		`"instructions":"# AGENTS.md instructions for ` + codexWiringRealCwd + `",` +
		`"client_metadata":{"x-codex-turn-metadata":` + jsonString(turnMetadata) + `},` +
		`"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":` + jsonString(inputText) + `}]}]}`
	return []byte(body)
}

// jsonString encodes s as a JSON string literal (including surrounding quotes).
func jsonString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		case '\r':
			b.WriteString(`\r`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func newCodexWiringExecutor(t *testing.T, serverURL string) (*CodexExecutor, *cliproxyauth.Auth, string) {
	t.Helper()
	helps.ResetCodexClientProfileCacheForTests()

	store := &codexServingHighWaterStore{}
	mgr := cliproxyauth.NewManager(store, nil, nil)

	const authID = "codex-path-wiring-1"
	const apiKey = "key-path-wiring"
	registered := &cliproxyauth.Auth{
		ID:       authID,
		Provider: "codex",
		Metadata: map[string]any{"type": "codex"},
		Attributes: map[string]string{
			"api_key":  apiKey,
			"base_url": serverURL,
		},
	}
	if _, err := mgr.Register(context.Background(), registered); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	// Switch ON: only then does e.normalizeCodexPaths / restoreCodexResponseCwd
	// wiring engage (see config.NormalizeAccountEnvEnabled).
	cfg := &config.Config{AuthDir: t.TempDir(), NormalizeAccountEnv: anticorrTruePtr()}
	if !config.NormalizeAccountEnvEnabled(cfg) {
		t.Fatal("test precondition: NormalizeAccountEnv switch must be ON for the wiring to engage")
	}
	executor := NewCodexExecutorWithManager(cfg, mgr)
	auth := &cliproxyauth.Auth{
		ID:       authID,
		ProxyURL: "direct",
		Provider: "codex",
		Attributes: map[string]string{
			"api_key":  apiKey,
			"base_url": serverURL,
		},
	}
	return executor, auth, apiKey
}

// TestCodexExecutor_Execute_NormalizesOutboundCwdOnServingPath is the fork(anticorr)
// F3 guard — outbound codex cwd/CODEX_HOME/git normalization.
//
// codex_executor.go Execute (~L947) calls `body = e.normalizeCodexPaths(...)` so
// the real cwd never reaches the codex upstream. The helps normalize unit tests
// cover the transform in isolation but cannot catch a merge that keeps the helper
// yet drops the executor call site. This drives the real Execute serving flow and
// asserts the OUTBOUND wire body contains no real cwd literal.
//
// Red condition: delete `body = e.normalizeCodexPaths(ctx, body, auth, apiKey)` in
// Execute (codex_executor.go ~L947). The real cwd then leaks to the wire and the
// assertion below fails.
//
// Level: executor-wiring.
func TestCodexExecutor_Execute_NormalizesOutboundCwdOnServingPath(t *testing.T) {
	var mu sync.Mutex
	var capturedBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		capturedBody = b
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"status\":\"completed\",\"model\":\"gpt-5.4-mini\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n"))
	}))
	defer server.Close()

	executor, auth, _ := newCodexWiringExecutor(t, server.URL)

	if _, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.4-mini",
		Payload: codexWiringBody(),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
	}); err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	mu.Lock()
	body := capturedBody
	mu.Unlock()
	if len(body) == 0 {
		t.Fatal("upstream captured no request body")
	}
	if bytes.Contains(body, []byte(codexWiringRealCwd)) {
		t.Fatalf("real cwd %q leaked to codex upstream (normalizeCodexPaths call site missing):\n%s", codexWiringRealCwd, body)
	}
	// Positive: the per-account canonical cwd must be present, proving the outbound
	// body was actually normalized (not merely emptied).
	canonical := helps.AccountCanonicalCwd(auth, "key-path-wiring")
	if !bytes.Contains(body, []byte(canonical)) {
		t.Fatalf("canonical cwd %q not present in outbound body (normalization did not run):\n%s", canonical, body)
	}
}

// TestCodexExecutor_Execute_RestoresResponseCwdOnServingPath is the fork(anticorr)
// F4 guard — response-side fake→real cwd restoration for tool-call arguments.
//
// When normalization is on, Execute attaches a cwd-restore collector to ctx
// (~L908), the outbound normalize captures the canonical→real mapping, and the
// response path calls `restoreCodexResponseCwd(ctx, ...)` (~L1084) to restore the
// real cwd inside function_call arguments before returning to the client. Without
// the restore, a local agent would receive fake-rooted tool-call paths it cannot
// act on.
//
// The upstream fake response echoes the CANONICAL (fake) cwd inside a
// function_call's arguments; after Execute the client-facing payload must carry
// the REAL cwd again.
//
// Red condition: delete `clientCompletedData = restoreCodexResponseCwd(ctx, ...)`
// in Execute (codex_executor.go ~L1084). The canonical fake cwd then survives in
// the client payload and the real cwd is never restored — the assertion fails.
//
// Level: executor-wiring.
func TestCodexExecutor_Execute_RestoresResponseCwdOnServingPath(t *testing.T) {
	// The canonical cwd depends only on auth scope + apiKey, so precompute it to
	// build the fake upstream response before wiring the server.
	scopeAuth := &cliproxyauth.Auth{
		ID:         "codex-path-wiring-1",
		ProxyURL:   "direct",
		Provider:   "codex",
		Attributes: map[string]string{"api_key": "key-path-wiring"},
	}
	canonical := helps.AccountCanonicalCwd(scopeAuth, "key-path-wiring")
	if canonical == "" || strings.Contains(canonical, codexWiringRealCwd) {
		t.Fatalf("unexpected canonical cwd derivation: %q", canonical)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		// response.completed carrying a function_call whose arguments echo the
		// CANONICAL (fake) cwd — this is what the restore must turn back into real.
		args := jsonString(`{"command":["bash","-lc","ls"],"workdir":"` + canonical + `"}`)
		completed := `data: {"type":"response.completed","response":{"id":"resp_1","object":"response","status":"completed","model":"gpt-5.4-mini",` +
			`"output":[{"type":"function_call","name":"shell","call_id":"call_1","arguments":` + args + `}],` +
			`"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}` + "\n\n"
		_, _ = w.Write([]byte(completed))
	}))
	defer server.Close()

	executor, auth, _ := newCodexWiringExecutor(t, server.URL)

	resp, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.4-mini",
		Payload: codexWiringBody(),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
	})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if len(resp.Payload) == 0 {
		t.Fatal("Execute returned empty response payload")
	}

	if !bytes.Contains(resp.Payload, []byte(codexWiringRealCwd)) {
		t.Fatalf("real cwd %q NOT restored in client response (restoreCodexResponseCwd call site missing):\n%s", codexWiringRealCwd, resp.Payload)
	}
	if bytes.Contains(resp.Payload, []byte(canonical)) {
		t.Fatalf("canonical fake cwd %q still present in client response (restore did not fully fire):\n%s", canonical, resp.Payload)
	}
}
