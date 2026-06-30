package helps

import (
	"context"
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

// boolPtr returns a *bool for building config gate states in tests.
func boolPtr(b bool) *bool { return &b }

// TestNormalizeAccountEnvWithRestore_CapturesFakeToReal verifies the outbound
// rewrite captures the canonicalCwd→realCwd mapping into the ctx collector, which
// is the single source the response side uses (no response-side re-probing).
func TestNormalizeAccountEnvWithRestore_CapturesFakeToReal(t *testing.T) {
	realCwd := "/Users/alice/Project/app"
	payload := systemTextPayload(t, "You are Claude.\n<env>\nWorking directory: "+realCwd+"\nIs directory a git repo: Yes\n</env>")

	auth := authForEnv("acct-cap.json")
	ctx, collector := ContextWithCwdRestoreCollector(context.Background())
	out := NormalizeAccountEnvWithRestore(ctx, payload, auth, "key-1", nil)

	canonical := AccountCanonicalCwd(auth, "key-1")
	got := gjson.GetBytes(out, "system.0.text").String()
	if strings.Contains(got, realCwd) {
		t.Fatalf("real cwd not normalized outbound: %q", got)
	}

	pairs := collector.Pairs()
	if len(pairs) != 1 {
		t.Fatalf("expected exactly one captured pair, got %d: %+v", len(pairs), pairs)
	}
	if pairs[0].Fake != canonical || pairs[0].Real != realCwd {
		t.Fatalf("captured mapping wrong: got fake=%q real=%q want fake=%q real=%q",
			pairs[0].Fake, pairs[0].Real, canonical, realCwd)
	}
}

// TestRestoreClaudeToolUseCwdInResponse_RestoresOnlyToolUse is the claude
// non-stream red line: a fake-rooted path inside tool_use.input is restored to
// the real cwd, while conversational text content is left byte-for-byte
// unchanged even when it contains the same fake root.
func TestRestoreClaudeToolUseCwdInResponse_RestoresOnlyToolUse(t *testing.T) {
	fake := "/Users/agent/workspace-deadbeef"
	real := "/Users/alice/Project/app"
	pairs := []CwdRestorePair{{Fake: fake, Real: real}}

	// content[0] is text (must NOT change); content[1] is a tool_use whose input
	// path argument carries the fake root (must be restored).
	body := []byte(`{"content":[` +
		`{"type":"text","text":"working in ` + fake + ` now"},` +
		`{"type":"tool_use","id":"toolu_1","name":"Read","input":{"file_path":"` + fake + `/main.go"}}` +
		`]}`)

	out := RestoreClaudeToolUseCwdInResponse(pairs, body)

	toolPath := gjson.GetBytes(out, "content.1.input.file_path").String()
	if toolPath != real+"/main.go" {
		t.Fatalf("tool_use path not restored: got %q want %q", toolPath, real+"/main.go")
	}
	// The text block must be untouched: the fake root is still present verbatim.
	gotText := gjson.GetBytes(out, "content.0.text").String()
	if gotText != "working in "+fake+" now" {
		t.Fatalf("text content was modified: %q", gotText)
	}
}

// TestRestoreClaudeToolUseCwdInResponse_NoPairsIsNoop verifies the gate-off
// behavior: with no captured mappings the response bytes are returned unchanged.
func TestRestoreClaudeToolUseCwdInResponse_NoPairsIsNoop(t *testing.T) {
	body := []byte(`{"content":[{"type":"tool_use","id":"t","name":"Read","input":{"file_path":"/Users/agent/workspace-x/main.go"}}]}`)
	out := RestoreClaudeToolUseCwdInResponse(nil, body)
	if string(out) != string(body) {
		t.Fatalf("expected no-op with empty pairs, got %q", out)
	}
}

// TestNormalizeCodexPathsWithRestore_CapturesAndRestores covers the codex red
// line end to end at the helper layer: the outbound rewrite captures both
// canonicalCwd→realCwd and canonicalHome→realHome, and a whole-payload byte
// restore (the codex response path) maps a fake-rooted function_call argument
// back to the real root.
func TestNormalizeCodexPathsWithRestore_CapturesAndRestores(t *testing.T) {
	auth := codexAuth("codex-restore.json")
	ctx, collector := ContextWithCwdRestoreCollector(context.Background())

	out := NormalizeCodexPathsWithRestore(ctx, codexBodyFixture(t), auth, "key-1")
	if strings.Contains(string(out), codexRealCwd) {
		t.Fatalf("real cwd not normalized outbound:\n%s", out)
	}

	pairs := collector.Pairs()
	canonicalCwd := AccountCanonicalCwd(auth, "key-1")
	canonicalHome := canonicalCodexHome(ClaudeAccountScopeKey(auth, "key-1"))

	var sawCwd, sawHome bool
	for _, p := range pairs {
		if p.Fake == canonicalCwd && p.Real == codexRealCwd {
			sawCwd = true
		}
		if p.Fake == canonicalHome && p.Real == codexRealHome {
			sawHome = true
		}
	}
	if !sawCwd {
		t.Fatalf("missing cwd mapping in %+v", pairs)
	}
	if !sawHome {
		t.Fatalf("missing CODEX_HOME mapping in %+v", pairs)
	}

	// Simulate a streamed function_call argument echoed back with the fake roots
	// (the assembled .done event form). Restoration must yield the real roots.
	respLine := []byte(`data: {"type":"response.output_item.done","item":{"type":"function_call",` +
		`"name":"shell","arguments":"{\"command\":\"cat ` + canonicalCwd + `/main.go\",\"workdir\":\"` + canonicalCwd + `\"}"}}`)
	restored := RestoreCwdInBytes(pairs, respLine)
	if strings.Contains(string(restored), canonicalCwd) {
		t.Fatalf("fake cwd still present after restore:\n%s", restored)
	}
	if !strings.Contains(string(restored), codexRealCwd) {
		t.Fatalf("real cwd missing after restore:\n%s", restored)
	}
}

// TestNormalizeCodexPathsWithRestore_GateOffNoCapture confirms that when no
// collector is attached (gate off path), the helper behaves exactly like the
// plain NormalizeCodexPaths and captures nothing.
func TestNormalizeCodexPathsWithRestore_GateOffNoCapture(t *testing.T) {
	auth := codexAuth("codex-gateoff.json")
	// No collector on the context.
	plain := NormalizeCodexPaths(codexBodyFixture(t), auth, "key-1")
	withRestore := NormalizeCodexPathsWithRestore(context.Background(), codexBodyFixture(t), auth, "key-1")
	if string(plain) != string(withRestore) {
		t.Fatalf("WithRestore diverged from plain normalize when no collector attached")
	}
}

// TestNormalizeAccountEnvEnabled_GateMatrix documents that the single switch the
// codex path now reuses is the same one claude reads.
func TestNormalizeAccountEnvEnabled_GateMatrix(t *testing.T) {
	if config.NormalizeAccountEnvEnabled(nil) {
		t.Fatalf("nil cfg must be off")
	}
	if config.NormalizeAccountEnvEnabled(&config.Config{NormalizeAccountEnv: boolPtr(false)}) {
		t.Fatalf("explicit false must be off")
	}
	if !config.NormalizeAccountEnvEnabled(&config.Config{NormalizeAccountEnv: boolPtr(true)}) {
		t.Fatalf("explicit true must be on")
	}
}
