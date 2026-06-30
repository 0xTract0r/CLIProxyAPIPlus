package helps

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// findPairReal returns the captured real path for a given fake root, or "" when the
// fake root was not captured.
func findPairReal(pairs []CwdRestorePair, fake string) string {
	for _, p := range pairs {
		if p.Fake == fake {
			return p.Real
		}
	}
	return ""
}

// TestNormalizeAccountEnv_AdditionalWorkingDirsNormalized covers scope-fix A: the
// directories under an "Additional working directories:" heading are list items with
// no key and (here) non-home roots (/tmp, /var). The old key-anchored pattern + the
// home-only secondary sweep both missed them, leaking the real paths upstream. Each
// must now be normalized to its own fake root, and each fake→real mapping captured so
// the response side can restore that specific directory.
func TestNormalizeAccountEnv_AdditionalWorkingDirsNormalized(t *testing.T) {
	realA := "/tmp/build-cache/projectA"
	realB := "/var/folders/zz/scratch/projectB"
	envText := "You are Claude.\n" +
		"# Environment\n" +
		" - Primary working directory: /Users/alice/repo\n" +
		" - Additional working directories:\n" +
		"   - " + realA + "\n" +
		"   - " + realB + "\n" +
		" - Is a git repository: false\n"

	payload := systemTextPayload(t, envText)
	auth := authForEnv("acct-add.json")

	ctx, collector := ContextWithCwdRestoreCollector(context.Background())
	out := NormalizeAccountEnvWithRestore(ctx, payload, auth, "key-1", nil)
	got := gjson.GetBytes(out, "system.0.text").String()

	// No real path of any root may survive outbound.
	for _, leak := range []string{realA, realB, "/Users/alice/repo", "/tmp/build-cache", "/var/folders"} {
		if strings.Contains(got, leak) {
			t.Fatalf("real path %q leaked outbound:\n%s", leak, got)
		}
	}

	// Each additional dir maps to its own deterministic fake; both must be present.
	fakeA := derivedFakeWorkspaceRoot(realA)
	fakeB := derivedFakeWorkspaceRoot(realB)
	if fakeA == fakeB {
		t.Fatalf("distinct additional dirs collided on one fake root: %q", fakeA)
	}
	if !strings.Contains(got, fakeA) {
		t.Fatalf("fake root for %q (%q) missing:\n%s", realA, fakeA, got)
	}
	if !strings.Contains(got, fakeB) {
		t.Fatalf("fake root for %q (%q) missing:\n%s", realB, fakeB, got)
	}

	// The "Is a git repository" line after the list must NOT be swallowed/rewritten.
	if !strings.Contains(got, "Is a git repository: false") {
		t.Fatalf("non-list line after additional dirs was altered:\n%s", got)
	}

	// Response side can restore each additional dir back to its own real path.
	pairs := collector.Pairs()
	if r := findPairReal(pairs, fakeA); r != realA {
		t.Fatalf("fakeA must restore to %q, got %q (pairs=%+v)", realA, r, pairs)
	}
	if r := findPairReal(pairs, fakeB); r != realB {
		t.Fatalf("fakeB must restore to %q, got %q (pairs=%+v)", realB, r, pairs)
	}
}

// TestNormalizeAccountEnv_ToolResultKnownCwdRewritten covers scope-fix B (claude):
// a tool_result that echoes the real cwd already captured from the env block must be
// rewritten real→fake; a tool_result containing an UNRELATED real path that was NOT
// captured must be left untouched (proving this is not a generalized /Users sweep).
func TestNormalizeAccountEnv_ToolResultKnownCwdRewritten(t *testing.T) {
	realCwd := "/Users/alice/Project/app"
	unrelated := "/Users/someoneelse/x/secret"
	canonical := ""

	// Body: env block declaring the real cwd + a tool_result echoing both the real
	// cwd (via pwd) and an unrelated real path.
	envText := "# Environment\n - Primary working directory: " + realCwd + "\n"
	payload := []byte(`{
		"system": [{"type":"text","text":` + mustJSONString(t, envText) + `}],
		"messages": [
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"t1","content":` +
		mustJSONString(t, "$ pwd\n"+realCwd+"\n$ cat /etc/hostname\nhost\n"+realCwd+"/sub/file.go\nunrelated: "+unrelated) +
		`}
			]}
		]
	}`)

	auth := authForEnv("acct-tr.json")
	canonical = AccountCanonicalCwd(auth, "key-1")

	ctx, _ := ContextWithCwdRestoreCollector(context.Background())
	out := NormalizeAccountEnvWithRestore(ctx, payload, auth, "key-1", nil)
	tr := gjson.GetBytes(out, "messages.0.content.0.content").String()

	// The captured real cwd (and its child path) must be real→fake rewritten.
	if strings.Contains(tr, realCwd) {
		t.Fatalf("captured real cwd not rewritten in tool_result:\n%s", tr)
	}
	if !strings.Contains(tr, canonical) {
		t.Fatalf("expected canonical fake root %q in tool_result:\n%s", canonical, tr)
	}
	if !strings.Contains(tr, canonical+"/sub/file.go") {
		t.Fatalf("child path under real cwd not rewritten prefix-safely:\n%s", tr)
	}
	// The UNRELATED real path must be untouched (not a generalized sweep).
	if !strings.Contains(tr, unrelated) {
		t.Fatalf("unrelated uncaptured path %q was wrongly rewritten:\n%s", unrelated, tr)
	}
}

// TestRewriteKnownRealToFake_PrefixSafe verifies the prefix-boundary rule directly:
// "<real>" and "<real>/sub" are rewritten, but "<real>Other" (a longer sibling that
// merely shares the text prefix) is NOT.
func TestRewriteKnownRealToFake_PrefixSafe(t *testing.T) {
	pairs := []CwdRestorePair{{Fake: "/Users/agent/workspace-abc", Real: "/Users/bob/Project"}}
	in := "/Users/bob/Project and /Users/bob/Project/sub and /Users/bob/ProjectOther"
	got := rewriteKnownRealToFake(pairs, in)

	if !strings.Contains(got, "/Users/agent/workspace-abc and") {
		t.Fatalf("exact root not rewritten: %q", got)
	}
	if !strings.Contains(got, "/Users/agent/workspace-abc/sub") {
		t.Fatalf("child path not rewritten: %q", got)
	}
	if !strings.Contains(got, "/Users/bob/ProjectOther") {
		t.Fatalf("sibling path wrongly rewritten (not prefix-safe): %q", got)
	}
}

func mustJSONString(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal json string: %v", err)
	}
	return string(b)
}
