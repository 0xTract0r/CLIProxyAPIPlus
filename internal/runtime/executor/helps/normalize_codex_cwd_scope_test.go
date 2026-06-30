package helps

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// codexInputTextBody builds a minimal codex responses body with one input item
// carrying the given text block, so tests can exercise rewriteCodexText paths.
func codexInputTextBody(t *testing.T, instructions string, inputText string) []byte {
	t.Helper()
	instrQ, _ := json.Marshal(instructions)
	textQ, _ := json.Marshal(inputText)
	return []byte(`{
		"instructions": ` + string(instrQ) + `,
		"input": [
			{"type":"message","role":"user","content":[{"type":"input_text","text":` + string(textQ) + `}]}
		]
	}`)
}

// TestNormalizeCodexPaths_MultipleDistinctRealCwds covers scope-fix C: a codex body
// can declare SEVERAL distinct real cwds (multiple <root> entries / an AGENTS.md
// header for a different dir). They previously all collapsed to one fake root, so the
// restore collector (de-dup on fake) dropped every real cwd after the first and a
// tool path under the second real cwd restored to the WRONG directory. Each distinct
// real cwd must now map to its OWN fake root and restore 1:1.
func TestNormalizeCodexPaths_MultipleDistinctRealCwds(t *testing.T) {
	primary := "/Users/alice/Project/main"
	extra := "/Users/alice/Project/other"

	envCtx := "<environment_context>\n" +
		"  <cwd>" + primary + "</cwd>\n" +
		"  <shell>zsh</shell>\n" +
		"  <filesystem><workspace_roots>" +
		"<root>" + primary + "</root>" +
		"<root>" + extra + "</root>" +
		"</workspace_roots></filesystem>\n" +
		"</environment_context>"
	body := codexInputTextBody(t, "# AGENTS.md instructions for "+extra+"\nbe nice", envCtx)

	auth := authForEnv("acct-codex-multi.json")
	ctx, collector := ContextWithCwdRestoreCollector(context.Background())
	out := NormalizeCodexPathsWithRestore(ctx, body, auth, "key-1")

	gotInput := gjson.GetBytes(out, "input.0.content.0.text").String()
	gotInstr := gjson.GetBytes(out, "instructions").String()

	// Neither real cwd may survive outbound, anywhere.
	for _, leak := range []string{primary, extra} {
		if strings.Contains(gotInput, leak) || strings.Contains(gotInstr, leak) {
			t.Fatalf("real cwd %q leaked outbound:\ninput=%s\ninstr=%s", leak, gotInput, gotInstr)
		}
	}

	canonical := AccountCanonicalCwd(auth, "key-1")
	extraFake := derivedFakeWorkspaceRoot(extra)
	if extraFake == canonical {
		t.Fatalf("extra cwd collided with primary fake root %q", canonical)
	}

	// Primary maps to canonical; extra maps to its own derived fake.
	if !strings.Contains(gotInput, canonical) {
		t.Fatalf("primary cwd not mapped to canonical fake:\n%s", gotInput)
	}
	if !strings.Contains(gotInput, extraFake) {
		t.Fatalf("extra cwd not mapped to its own fake root %q:\n%s", extraFake, gotInput)
	}

	pairs := collector.Pairs()
	if r := findPairReal(pairs, canonical); r != primary {
		t.Fatalf("primary must restore to %q, got %q (pairs=%+v)", primary, r, pairs)
	}
	if r := findPairReal(pairs, extraFake); r != extra {
		t.Fatalf("extra must restore to %q, got %q (pairs=%+v)", extra, r, pairs)
	}
}

// TestNormalizeCodexPaths_SingleRealCwdInvariant documents the common-case invariant:
// when the body exposes exactly ONE real cwd (the codex client's actual cwd, which
// also keys the turn-metadata workspaces object), it maps to the per-account
// canonical cwd and nothing is split into a derived fake — the single-cwd behavior is
// unchanged by the C-fix.
func TestNormalizeCodexPaths_SingleRealCwdInvariant(t *testing.T) {
	real := "/Users/bob/repo"
	envCtx := "<environment_context>\n  <cwd>" + real + "</cwd>\n" +
		"  <filesystem><workspace_roots><root>" + real + "</root></workspace_roots></filesystem>\n" +
		"</environment_context>"
	body := codexInputTextBody(t, "do work", envCtx)

	auth := authForEnv("acct-codex-single.json")
	ctx, collector := ContextWithCwdRestoreCollector(context.Background())
	out := NormalizeCodexPathsWithRestore(ctx, body, auth, "key-1")

	got := gjson.GetBytes(out, "input.0.content.0.text").String()
	canonical := AccountCanonicalCwd(auth, "key-1")
	if strings.Contains(got, real) {
		t.Fatalf("single real cwd leaked:\n%s", got)
	}
	if !strings.Contains(got, canonical) {
		t.Fatalf("single real cwd not mapped to canonical:\n%s", got)
	}

	pairs := collector.Pairs()
	// Exactly one cwd pair, mapped to the canonical (no spurious derived fake).
	cwdPairs := 0
	for _, p := range pairs {
		if p.Real == real {
			cwdPairs++
			if p.Fake != canonical {
				t.Fatalf("single cwd must map to canonical %q, got fake %q", canonical, p.Fake)
			}
		}
	}
	if cwdPairs != 1 {
		t.Fatalf("expected exactly 1 cwd pair for single-cwd body, got %d (pairs=%+v)", cwdPairs, pairs)
	}
}

// TestNormalizeCodexPaths_ToolOutputKnownCwd covers scope-fix B (codex analog): a
// function_call_output (input[].output) echoing a bare real cwd captured elsewhere in
// the request must be rewritten real→fake; an unrelated uncaptured path must be left
// untouched.
func TestNormalizeCodexPaths_ToolOutputKnownCwd(t *testing.T) {
	real := "/Users/carol/Project/svc"
	unrelated := "/Users/nobody/elsewhere"

	envCtx := "<environment_context>\n  <cwd>" + real + "</cwd>\n</environment_context>"
	envQ, _ := json.Marshal(envCtx)
	// The tool output has NO env tags, only a bare pwd echo of the real cwd plus an
	// unrelated path, so it is only reachable via the captured-pairs pass.
	outputQ, _ := json.Marshal("$ pwd\n" + real + "\n" + real + "/sub/x.go: error\nother: " + unrelated)

	body := []byte(`{
		"instructions": "go",
		"input": [
			{"type":"message","role":"user","content":[{"type":"input_text","text":` + string(envQ) + `}]},
			{"type":"function_call_output","call_id":"c1","output":` + string(outputQ) + `}
		]
	}`)

	auth := authForEnv("acct-codex-tooloutput.json")
	ctx, _ := ContextWithCwdRestoreCollector(context.Background())
	out := NormalizeCodexPathsWithRestore(ctx, body, auth, "key-1")

	gotOut := gjson.GetBytes(out, "input.1.output").String()
	canonical := AccountCanonicalCwd(auth, "key-1")

	if strings.Contains(gotOut, real) {
		t.Fatalf("captured real cwd not rewritten in tool output:\n%s", gotOut)
	}
	if !strings.Contains(gotOut, canonical+"/sub/x.go") {
		t.Fatalf("child path under real cwd not rewritten prefix-safely:\n%s", gotOut)
	}
	if !strings.Contains(gotOut, unrelated) {
		t.Fatalf("unrelated uncaptured path %q wrongly rewritten:\n%s", unrelated, gotOut)
	}
}
