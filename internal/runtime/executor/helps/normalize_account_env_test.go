package helps

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func authForEnv(fileName string) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{FileName: fileName}
}

// TestNormalizeAccountEnv_SystemEnvBlockRewritten covers P2.A7.2: the <env>
// working-directory and home paths inside the system field are rewritten to the
// per-account canonical path.
func TestNormalizeAccountEnv_SystemEnvBlockRewritten(t *testing.T) {
	payload := []byte(`{
		"system": [
			{"type": "text", "text": "You are Claude.\n<env>\nWorking directory: /Users/alice/Project/app\nIs directory a git repo: Yes\n</env>"}
		]
	}`)

	auth := authForEnv("acct-a.json")
	out := NormalizeAccountEnv(payload, auth, "key-1")

	got := gjson.GetBytes(out, "system.0.text").String()
	canonical := AccountCanonicalCwd(auth, "key-1")

	if strings.Contains(got, "/Users/alice") {
		t.Fatalf("real cwd not normalized: %q", got)
	}
	if !strings.Contains(got, "Working directory: "+canonical) {
		t.Fatalf("expected canonical cwd %q in %q", canonical, got)
	}
}

// TestNormalizeAccountEnv_SystemReminderInMessages covers the second leak site:
// <system-reminder> / <env> blocks embedded in messages content text.
func TestNormalizeAccountEnv_SystemReminderInMessages(t *testing.T) {
	payload := []byte(`{
		"messages": [
			{"role": "user", "content": [
				{"type": "text", "text": "<system-reminder>\nContents of /Users/bob/.claude/CLAUDE.md\nWorking directory: /home/bob/repo\n</system-reminder>"}
			]}
		]
	}`)

	auth := authForEnv("acct-b.json")
	out := NormalizeAccountEnv(payload, auth, "key-1")

	got := gjson.GetBytes(out, "messages.0.content.0.text").String()
	canonical := AccountCanonicalCwd(auth, "key-1")

	if strings.Contains(got, "/Users/bob") || strings.Contains(got, "/home/bob") {
		t.Fatalf("real paths not normalized inside <system-reminder>: %q", got)
	}
	if !strings.Contains(got, canonical) {
		t.Fatalf("expected canonical %q in %q", canonical, got)
	}
}

// TestNormalizeAccountEnv_StringContentSystemReminder covers a messages entry
// whose content is a plain string carrying a <system-reminder> block.
func TestNormalizeAccountEnv_StringContentSystemReminder(t *testing.T) {
	payload := []byte(`{
		"messages": [
			{"role": "user", "content": "<system-reminder>\nWorking directory: /Users/carol/work\n</system-reminder>"}
		]
	}`)

	auth := authForEnv("acct-c.json")
	out := NormalizeAccountEnv(payload, auth, "")

	got := gjson.GetBytes(out, "messages.0.content").String()
	if strings.Contains(got, "/Users/carol") {
		t.Fatalf("string content <system-reminder> not normalized: %q", got)
	}
}

// TestNormalizeAccountEnv_PerAccountDeterministic covers determinism: same account
// is stable across calls/apiKeys; distinct accounts differ.
func TestNormalizeAccountEnv_PerAccountDeterministic(t *testing.T) {
	a := authForEnv("acct-a.json")
	b := authForEnv("acct-b.json")

	cwdA1 := AccountCanonicalCwd(a, "key-1")
	cwdA2 := AccountCanonicalCwd(a, "key-2") // apiKey does not change scope for file-backed auth
	cwdB := AccountCanonicalCwd(b, "key-1")

	if cwdA1 != cwdA2 {
		t.Fatalf("canonical cwd not stable for same account: %q vs %q", cwdA1, cwdA2)
	}
	if cwdA1 == cwdB {
		t.Fatalf("canonical cwd should differ between accounts: %q == %q", cwdA1, cwdB)
	}
	if !strings.HasPrefix(cwdA1, canonicalHomeRoot+"/workspace-") {
		t.Fatalf("unexpected canonical cwd shape: %q", cwdA1)
	}
}

// TestNormalizeAccountEnv_DoesNotTouchToolUseArgs covers the safety boundary:
// tool_use input args and tool_result content are never rewritten even when they
// contain absolute paths.
func TestNormalizeAccountEnv_DoesNotTouchToolUseArgs(t *testing.T) {
	payload := []byte(`{
		"messages": [
			{"role": "assistant", "content": [
				{"type": "tool_use", "id": "t1", "name": "Read", "input": {"file_path": "/Users/dave/secret/notes.md"}}
			]},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "t1", "content": "cat /Users/dave/secret/notes.md output"}
			]}
		]
	}`)

	auth := authForEnv("acct-d.json")
	out := NormalizeAccountEnv(payload, auth, "")

	if gjson.GetBytes(out, "messages.0.content.0.input.file_path").String() != "/Users/dave/secret/notes.md" {
		t.Fatalf("tool_use input args must not be rewritten: %s", out)
	}
	if !strings.Contains(gjson.GetBytes(out, "messages.1.content.0.content").String(), "/Users/dave/secret/notes.md") {
		t.Fatalf("tool_result content must not be rewritten: %s", out)
	}
}

// TestNormalizeAccountEnv_DoesNotTouchPlainConversation covers that ordinary
// conversational text (no <env> / <system-reminder> tags) is left untouched even
// when it mentions an absolute path.
func TestNormalizeAccountEnv_DoesNotTouchPlainConversation(t *testing.T) {
	payload := []byte(`{
		"messages": [
			{"role": "user", "content": [
				{"type": "text", "text": "Please open /Users/eve/main.go and explain it."}
			]}
		]
	}`)

	auth := authForEnv("acct-e.json")
	out := NormalizeAccountEnv(payload, auth, "")

	if got := gjson.GetBytes(out, "messages.0.content.0.text").String(); !strings.Contains(got, "/Users/eve/main.go") {
		t.Fatalf("plain conversation path must not be rewritten: %q", got)
	}
}

// TestNormalizeAccountEnv_InvalidPayloadPassesThrough covers the no-400 contract:
// an unparseable body is returned byte-for-byte unchanged.
func TestNormalizeAccountEnv_InvalidPayloadPassesThrough(t *testing.T) {
	payload := []byte(`{not valid json <env>/Users/x</env>`)
	auth := authForEnv("acct-f.json")
	out := NormalizeAccountEnv(payload, auth, "")
	if string(out) != string(payload) {
		t.Fatalf("invalid payload must pass through unchanged, got %q", out)
	}
}

// TestNormalizeAccountEnv_StringSystemEnvBlock covers a system field that is a
// plain string (not an array of text blocks).
func TestNormalizeAccountEnv_StringSystemEnvBlock(t *testing.T) {
	payload := []byte(`{"system": "<env>\nWorking directory: /home/frank/svc\n</env>"}`)
	auth := authForEnv("acct-g.json")
	out := NormalizeAccountEnv(payload, auth, "")
	got := gjson.GetBytes(out, "system").String()
	if strings.Contains(got, "/home/frank") {
		t.Fatalf("string system <env> not normalized: %q", got)
	}
}
