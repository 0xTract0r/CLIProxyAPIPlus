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

// TestRestoreClaudeToolUseCwdInResponse_BackslashRealCwdStaysValidJSON is the F2
// escaping red line for the claude non-stream path: when the real cwd contains a
// backslash (Windows C:\Users\bob), restoring it inside tool_use.input must keep
// the response VALID JSON (the backslash JSON-escaped, not injected raw) and the
// parsed file_path must equal the real backslash path.
func TestRestoreClaudeToolUseCwdInResponse_BackslashRealCwdStaysValidJSON(t *testing.T) {
	fake := "/Users/agent/workspace-deadbeef"
	real := `C:\Users\bob`
	pairs := []CwdRestorePair{{Fake: fake, Real: real}}

	body := []byte(`{"content":[` +
		`{"type":"tool_use","id":"toolu_1","name":"Read","input":{"file_path":"` + fake + `/main.go","note":"plain"}}` +
		`]}`)

	out := RestoreClaudeToolUseCwdInResponse(pairs, body)

	if !gjson.ValidBytes(out) {
		t.Fatalf("restored body is not valid JSON (backslash injected raw):\n%s", out)
	}
	got := gjson.GetBytes(out, "content.0.input.file_path").String()
	want := `C:\Users\bob/main.go`
	if got != want {
		t.Fatalf("file_path not restored to real backslash path: got %q want %q", got, want)
	}
	if strings.Contains(string(out), fake) {
		t.Fatalf("fake root survived:\n%s", out)
	}
}

// TestRestoreClaudeToolUseCwdInResponse_QuoteAndControlRealCwdStaysValidJSON
// covers the F2 escaping red line for a real cwd containing a double quote and a
// tab. A literal swap would inject an unescaped quote and break the JSON; the
// structural restore must escape both and keep the document valid.
func TestRestoreClaudeToolUseCwdInResponse_QuoteAndControlRealCwdStaysValidJSON(t *testing.T) {
	fake := "/Users/agent/workspace-deadbeef"
	real := "/Users/a\"b\tc/app" // embedded quote and tab.
	pairs := []CwdRestorePair{{Fake: fake, Real: real}}

	body := []byte(`{"content":[` +
		`{"type":"tool_use","id":"toolu_1","name":"Read","input":{"file_path":"` + fake + `/main.go"}}` +
		`]}`)

	out := RestoreClaudeToolUseCwdInResponse(pairs, body)
	if !gjson.ValidBytes(out) {
		t.Fatalf("restored body is not valid JSON (quote/control injected raw):\n%s", out)
	}
	got := gjson.GetBytes(out, "content.0.input.file_path").String()
	want := real + "/main.go"
	if got != want {
		t.Fatalf("file_path not restored: got %q want %q", got, want)
	}
}

// TestRestoreCwdInToolUseInputRaw_NestedAndArrays verifies the structural restore
// descends into nested objects and arrays (a path argument at any depth), while
// never rewriting object KEYS, and leaves non-string leaves untouched.
func TestRestoreCwdInToolUseInputRaw_NestedAndArrays(t *testing.T) {
	fake := "/Users/agent/workspace-deadbeef"
	real := "/Users/alice/Project/app"
	pairs := []CwdRestorePair{{Fake: fake, Real: real}}

	raw := `{"opts":{"cwd":"` + fake + `"},"paths":["` + fake + `/a.go","` + fake + `/b.go"],"count":2,"flag":true}`
	out := RestoreCwdInToolUseInputRaw(pairs, raw)

	if !gjson.Valid(out) {
		t.Fatalf("structural restore produced invalid JSON: %s", out)
	}
	if got := gjson.Get(out, "opts.cwd").String(); got != real {
		t.Fatalf("nested object value not restored: got %q", got)
	}
	if got := gjson.Get(out, "paths.0").String(); got != real+"/a.go" {
		t.Fatalf("array[0] not restored: got %q", got)
	}
	if got := gjson.Get(out, "paths.1").String(); got != real+"/b.go" {
		t.Fatalf("array[1] not restored: got %q", got)
	}
	if got := gjson.Get(out, "count").Int(); got != 2 {
		t.Fatalf("number leaf changed: %d", got)
	}
	if got := gjson.Get(out, "flag").Bool(); got != true {
		t.Fatalf("bool leaf changed: %v", got)
	}
}

// TestRestoreCwdInToolUseInputRaw_DoesNotRewriteKeys is the scope red line: a fake
// root that appears as an object KEY (not a value) must NOT be rewritten — only
// string values carry tool-call path arguments.
func TestRestoreCwdInToolUseInputRaw_DoesNotRewriteKeys(t *testing.T) {
	fake := "/Users/agent/workspace-deadbeef"
	real := "/Users/alice/Project/app"
	pairs := []CwdRestorePair{{Fake: fake, Real: real}}

	raw := `{"` + fake + `":"keep-key","file_path":"` + fake + `/main.go"}`
	out := RestoreCwdInToolUseInputRaw(pairs, raw)

	if !gjson.Valid(out) {
		t.Fatalf("invalid JSON: %s", out)
	}
	if got := gjson.Get(out, "file_path").String(); got != real+"/main.go" {
		t.Fatalf("value not restored: got %q", got)
	}
	if !gjson.Get(out, escapeSjsonKey(fake)).Exists() {
		t.Fatalf("object key was rewritten (red line violation): %s", out)
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
	// (the assembled .done event form). Structural restoration must yield the real
	// roots while keeping the result valid JSON.
	respLine := []byte(`data: {"type":"response.output_item.done","item":{"type":"function_call",` +
		`"name":"shell","arguments":"{\"command\":\"cat ` + canonicalCwd + `/main.go\",\"workdir\":\"` + canonicalCwd + `\"}"}}`)
	restored, changed := RestoreCodexFunctionCallCwdInResponse(pairs, respLine)
	if !changed {
		t.Fatalf("expected a restore change, got none:\n%s", restored)
	}
	if strings.Contains(string(restored), canonicalCwd) {
		t.Fatalf("fake cwd still present after restore:\n%s", restored)
	}
	if !strings.Contains(string(restored), codexRealCwd) {
		t.Fatalf("real cwd missing after restore:\n%s", restored)
	}
	// The "data: " prefix must be preserved and the JSON portion must stay valid.
	if !strings.HasPrefix(string(restored), "data: ") {
		t.Fatalf("SSE data prefix lost:\n%s", restored)
	}
	jsonPart := strings.TrimPrefix(string(restored), "data: ")
	if !gjson.Valid(jsonPart) {
		t.Fatalf("restored SSE JSON is invalid:\n%s", jsonPart)
	}
	// The arguments string must decode to the real workdir.
	argStr := gjson.Get(jsonPart, "item.arguments").String()
	if got := gjson.Get(argStr, "workdir").String(); got != codexRealCwd {
		t.Fatalf("workdir not restored: got %q want %q", got, codexRealCwd)
	}
}

// TestRestoreCodexFunctionCallCwdInResponse_BackslashStaysValidJSON is the F2
// escaping red line for codex: a real cwd containing a backslash, restored inside
// function_call.arguments (itself a JSON string), must keep BOTH the outer
// response JSON and the inner arguments JSON valid.
func TestRestoreCodexFunctionCallCwdInResponse_BackslashStaysValidJSON(t *testing.T) {
	fakeCwd := "/Users/agent/codex-ws-abcdef01"
	realCwd := `C:\Users\bob\proj`
	pairs := []CwdRestorePair{{Fake: fakeCwd, Real: realCwd}}

	respLine := []byte(`data: {"type":"response.output_item.done","item":{"type":"function_call",` +
		`"name":"shell","arguments":"{\"workdir\":\"` + fakeCwd + `\"}"}}`)

	restored, changed := RestoreCodexFunctionCallCwdInResponse(pairs, respLine)
	if !changed {
		t.Fatalf("expected change, got none:\n%s", restored)
	}
	jsonPart := strings.TrimPrefix(string(restored), "data: ")
	if !gjson.Valid(jsonPart) {
		t.Fatalf("outer JSON invalid after backslash restore:\n%s", jsonPart)
	}
	argStr := gjson.Get(jsonPart, "item.arguments").String()
	if !gjson.Valid(argStr) {
		t.Fatalf("inner arguments JSON invalid after backslash restore: %q", argStr)
	}
	if got := gjson.Get(argStr, "workdir").String(); got != realCwd {
		t.Fatalf("workdir not restored to real backslash path: got %q want %q", got, realCwd)
	}
}

// TestRestoreCodexFunctionCallCwdInResponse_OnlyToolArgsRestored is the scope red
// line for codex: a fake root that appears in conversational reasoning / output
// text must NOT be restored; only function_call.arguments are restored.
func TestRestoreCodexFunctionCallCwdInResponse_OnlyToolArgsRestored(t *testing.T) {
	fakeCwd := "/Users/agent/codex-ws-abcdef01"
	realCwd := "/Users/corylin/proj"
	pairs := []CwdRestorePair{{Fake: fakeCwd, Real: realCwd}}

	// A completed payload: one reasoning item (must stay fake) + one function_call
	// (arguments must restore).
	body := []byte(`{"type":"response.completed","response":{"output":[` +
		`{"type":"reasoning","summary":[{"type":"summary_text","text":"working in ` + fakeCwd + ` now"}]},` +
		`{"type":"function_call","name":"shell","arguments":"{\"workdir\":\"` + fakeCwd + `\"}"}` +
		`]}}`)

	restored, changed := RestoreCodexFunctionCallCwdInResponse(pairs, body)
	if !changed {
		t.Fatalf("expected function_call restore, got none:\n%s", restored)
	}
	if !gjson.ValidBytes(restored) {
		t.Fatalf("restored payload invalid JSON:\n%s", restored)
	}
	// Reasoning text must still contain the FAKE root (not restored).
	reasoning := gjson.GetBytes(restored, "response.output.0.summary.0.text").String()
	if !strings.Contains(reasoning, fakeCwd) || strings.Contains(reasoning, realCwd) {
		t.Fatalf("reasoning text was restored (scope red line violation): %q", reasoning)
	}
	// function_call arguments must be restored to the real root.
	argStr := gjson.GetBytes(restored, "response.output.1.arguments").String()
	if got := gjson.Get(argStr, "workdir").String(); got != realCwd {
		t.Fatalf("function_call workdir not restored: got %q", got)
	}
}

// TestRestoreCodexFunctionCallCwdInResponse_DeltaLineUnchanged confirms a streamed
// function_call_arguments.delta fragment (display-only, no function_call item) is
// returned unchanged — the authoritative .done/.completed event carries the whole
// argument and is the one that gets restored.
func TestRestoreCodexFunctionCallCwdInResponse_DeltaLineUnchanged(t *testing.T) {
	fakeCwd := "/Users/agent/codex-ws-abcdef01"
	pairs := []CwdRestorePair{{Fake: fakeCwd, Real: "/Users/corylin/proj"}}
	line := []byte(`data: {"type":"response.function_call_arguments.delta","delta":"` + fakeCwd + `"}`)
	out, changed := RestoreCodexFunctionCallCwdInResponse(pairs, line)
	if changed {
		t.Fatalf("delta line should be unchanged (no function_call item), got change:\n%s", out)
	}
	if string(out) != string(line) {
		t.Fatalf("delta line mutated:\n got %s\nwant %s", out, line)
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
