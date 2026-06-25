package helps

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// systemTextPayload builds a {"system":[{"type":"text","text":<text>}]} body with
// the text JSON-escaped, so callers can embed backticks and newlines (as in the
// real "# auto memory" sentence) without hand-escaping a raw JSON literal.
func systemTextPayload(t *testing.T, text string) []byte {
	t.Helper()
	quoted, err := json.Marshal(text)
	if err != nil {
		t.Fatalf("marshal system text: %v", err)
	}
	return []byte(`{"system":[{"type":"text","text":` + string(quoted) + `}]}`)
}

func authForEnv(fileName string) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{ProxyURL: "direct", FileName: fileName}
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
	out := NormalizeAccountEnv(payload, auth, "key-1", nil)

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
	out := NormalizeAccountEnv(payload, auth, "key-1", nil)

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
	out := NormalizeAccountEnv(payload, auth, "", nil)

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
	out := NormalizeAccountEnv(payload, auth, "", nil)

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
	out := NormalizeAccountEnv(payload, auth, "", nil)

	if got := gjson.GetBytes(out, "messages.0.content.0.text").String(); !strings.Contains(got, "/Users/eve/main.go") {
		t.Fatalf("plain conversation path must not be rewritten: %q", got)
	}
}

// TestNormalizeAccountEnv_InvalidPayloadPassesThrough covers the no-400 contract:
// an unparseable body is returned byte-for-byte unchanged.
func TestNormalizeAccountEnv_InvalidPayloadPassesThrough(t *testing.T) {
	payload := []byte(`{not valid json <env>/Users/x</env>`)
	auth := authForEnv("acct-f.json")
	out := NormalizeAccountEnv(payload, auth, "", nil)
	if string(out) != string(payload) {
		t.Fatalf("invalid payload must pass through unchanged, got %q", out)
	}
}

// TestNormalizeAccountEnv_StringSystemEnvBlock covers a system field that is a
// plain string (not an array of text blocks).
func TestNormalizeAccountEnv_StringSystemEnvBlock(t *testing.T) {
	payload := []byte(`{"system": "<env>\nWorking directory: /home/frank/svc\n</env>"}`)
	auth := authForEnv("acct-g.json")
	out := NormalizeAccountEnv(payload, auth, "", nil)
	got := gjson.GetBytes(out, "system").String()
	if strings.Contains(got, "/home/frank") {
		t.Fatalf("string system <env> not normalized: %q", got)
	}
}

// macBaselineCfg returns a config with BOTH normalize-account-env and
// stabilize-device-profile on, whose stabilized device profile baseline OS is
// MacOS/arm64 (the default fingerprint). Body OS normalization is gated on
// stabilize, so it only targets the darwin / Darwin representation when stabilize
// is enabled — the realistic production posture where the outbound header is also
// pinned to MacOS.
func macBaselineCfg() *config.Config {
	on := true
	return &config.Config{
		NormalizeAccountEnv:  &on,
		ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{StabilizeDeviceProfile: &on},
	}
}

// normalizeOnStabilizeOffCfg returns a config with normalize-account-env on but
// stabilize-device-profile OFF. In this state the outbound X-Stainless-Os header
// passes through the real host OS, so body OS rewrite must NOT run (rewriting body
// to the MacOS baseline would contradict the real-OS header). cwd normalization
// still runs because it is independent of stabilize.
func normalizeOnStabilizeOffCfg() *config.Config {
	on := true
	off := false
	return &config.Config{
		NormalizeAccountEnv:  &on,
		ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{StabilizeDeviceProfile: &off},
	}
}

// realMarkdownEnvPayload mirrors the production claude-code 2.1.181 body shape:
// a "# Environment" Markdown section with a flat " - Key: value" list, including a
// /tmp working directory and a real Linux Platform / OS Version that contradict
// the stabilized MacOS header.
func realMarkdownEnvPayload() []byte {
	return []byte(`{
		"system": [
			{"type": "text", "text": "You are Claude Code.\n\n# Environment\nYou have been invoked in the following environment: \n - Primary working directory: /tmp/prodsess1\n - Is a git repository: false\n - Platform: linux\n - Shell: bash\n - OS Version: Linux 6.8.0-111-generic\n\n# Tools\nYou have tools."}
		]
	}`)
}

// TestNormalizeAccountEnv_RealMarkdownEnvCwdNormalized covers the core T052 bug:
// the real 2.1.181 "# Environment" Markdown block is recognized and its cwd
// (a /tmp path, not under /Users or /home) is normalized to the canonical
// per-account workspace, with zero real-path occurrences egressing.
func TestNormalizeAccountEnv_RealMarkdownEnvCwdNormalized(t *testing.T) {
	auth := authForEnv("acct-md.json")
	out := NormalizeAccountEnv(realMarkdownEnvPayload(), auth, "", macBaselineCfg())

	got := gjson.GetBytes(out, "system.0.text").String()
	canonical := AccountCanonicalCwd(auth, "")

	if strings.Contains(got, "/tmp/prodsess1") {
		t.Fatalf("real /tmp cwd in Markdown env not normalized: %q", got)
	}
	if strings.Count(got, "/tmp/prodsess1") != 0 {
		t.Fatalf("real cwd must have 0 occurrences in egress, got: %q", got)
	}
	if !strings.Contains(got, "Primary working directory: "+canonical) {
		t.Fatalf("expected canonical cwd %q on key line, got: %q", canonical, got)
	}
}

// TestNormalizeAccountEnv_RealMarkdownEnvOSAligned covers the body/header OS
// consistency fix: with a MacOS baseline, the Markdown block's "Platform: linux"
// and "OS Version: Linux ..." are rewritten to the darwin / Darwin representation
// that matches the outbound X-Stainless-Os header, so body and header describe one
// OS.
func TestNormalizeAccountEnv_RealMarkdownEnvOSAligned(t *testing.T) {
	cfg := macBaselineCfg()
	auth := authForEnv("acct-md-os.json")
	out := NormalizeAccountEnv(realMarkdownEnvPayload(), auth, "", cfg)

	got := gjson.GetBytes(out, "system.0.text").String()

	// Body must no longer report the real host (Linux) OS.
	if strings.Contains(got, "Platform: linux") {
		t.Fatalf("body Platform not aligned away from linux: %q", got)
	}
	if strings.Contains(got, "OS Version: Linux") {
		t.Fatalf("body OS Version not aligned away from Linux: %q", got)
	}

	// Body OS must match the baseline OS that the outbound header advertises.
	baselineOS := defaultClaudeDeviceProfile(cfg).OS // MacOS
	wantBody, ok := baselineBodyOSFor(baselineOS)
	if !ok {
		t.Fatalf("baseline OS %q has no body mapping", baselineOS)
	}
	if !strings.Contains(got, "Platform: "+wantBody.platform) {
		t.Fatalf("expected body Platform %q (aligned to header OS %q), got: %q", wantBody.platform, baselineOS, got)
	}
	if !strings.Contains(got, "OS Version: "+wantBody.osVersion) {
		t.Fatalf("expected body OS Version %q (aligned to header OS %q), got: %q", wantBody.osVersion, baselineOS, got)
	}

	// Sanity: the header path maps the same baseline OS to the same Stainless name
	// (MacOS), confirming body and header derive from one OS source.
	if baselineOS != "MacOS" {
		t.Fatalf("expected MacOS baseline for default fingerprint, got %q", baselineOS)
	}
}

// TestNormalizeAccountEnv_StabilizeOffLeavesBodyOSButNormalizesCwd covers the T053
// stabilize-on gate: with normalize-account-env ON but stabilize-device-profile
// OFF, the outbound X-Stainless-Os header passes through the real host OS, so the
// body Platform / OS Version lines must be LEFT as the real values (rewriting them
// to the MacOS baseline would itself create a body(Mac) vs header(real-Linux)
// contradiction). cwd is still normalized because that goal is independent of
// stabilize.
func TestNormalizeAccountEnv_StabilizeOffLeavesBodyOSButNormalizesCwd(t *testing.T) {
	cfg := normalizeOnStabilizeOffCfg()
	auth := authForEnv("acct-stabilize-off.json")
	out := NormalizeAccountEnv(realMarkdownEnvPayload(), auth, "", cfg)

	got := gjson.GetBytes(out, "system.0.text").String()
	canonical := AccountCanonicalCwd(auth, "")

	// cwd IS still normalized (independent of stabilize).
	if strings.Contains(got, "/tmp/prodsess1") {
		t.Fatalf("cwd must still be normalized when stabilize is off: %q", got)
	}
	if !strings.Contains(got, "Primary working directory: "+canonical) {
		t.Fatalf("expected canonical cwd %q when stabilize off, got: %q", canonical, got)
	}

	// Body OS lines must be LEFT as the real host values (NOT rewritten to the
	// MacOS baseline) because the header still carries the real OS.
	if !strings.Contains(got, "Platform: linux") {
		t.Fatalf("body Platform must stay real (linux) when stabilize off, got: %q", got)
	}
	if !strings.Contains(got, "OS Version: Linux 6.8.0-111-generic") {
		t.Fatalf("body OS Version must stay real when stabilize off, got: %q", got)
	}
	// And must NOT have been pushed to the MacOS baseline.
	if strings.Contains(got, "Platform: darwin") || strings.Contains(got, "Darwin 24.6.0") {
		t.Fatalf("body OS must not be rewritten to MacOS baseline when stabilize off, got: %q", got)
	}
}

// TestNormalizeAccountEnv_LegacyXMLEnvStillCompatible covers backward
// compatibility: the historical <env> XML block path is unchanged — cwd is still
// normalized (and, when OS lines are present and stabilize is on, OS is aligned too).
func TestNormalizeAccountEnv_LegacyXMLEnvStillCompatible(t *testing.T) {
	payload := []byte(`{
		"system": [
			{"type": "text", "text": "You are Claude.\n<env>\nWorking directory: /Users/legacy/Project/app\nIs directory a git repo: Yes\nPlatform: linux\n</env>"}
		]
	}`)
	auth := authForEnv("acct-xml.json")
	cfg := macBaselineCfg()
	out := NormalizeAccountEnv(payload, auth, "", cfg)

	got := gjson.GetBytes(out, "system.0.text").String()
	canonical := AccountCanonicalCwd(auth, "")

	if strings.Contains(got, "/Users/legacy") {
		t.Fatalf("legacy <env> cwd not normalized: %q", got)
	}
	if !strings.Contains(got, "Working directory: "+canonical) {
		t.Fatalf("expected canonical cwd %q in legacy <env>: %q", canonical, got)
	}
	// OS alignment also applies inside the legacy XML block.
	if strings.Contains(got, "Platform: linux") {
		t.Fatalf("legacy <env> Platform not aligned away from linux: %q", got)
	}
	if !strings.Contains(got, "Platform: darwin") {
		t.Fatalf("expected legacy <env> Platform aligned to darwin: %q", got)
	}
}

// TestNormalizeAccountEnv_MarkdownEnvSwitchOffByCallerUnchanged is a helper-level
// proxy for the zero-migration contract: NormalizeAccountEnv is only ever called
// when the switch is on (gated at the call sites). This test documents that the
// helper itself, when applied, leaves a body with NO environment block
// byte-for-byte unchanged — i.e. only environment blocks are touched.
func TestNormalizeAccountEnv_NoEnvBlockUnchanged(t *testing.T) {
	payload := []byte(`{
		"system": [{"type": "text", "text": "You are Claude. Open /tmp/prodsess1/main.go and Platform: linux is irrelevant prose."}],
		"messages": [{"role": "user", "content": "Platform: linux and /tmp/prodsess1 in plain chat"}]
	}`)
	auth := authForEnv("acct-noenv.json")
	out := NormalizeAccountEnv(payload, auth, "", macBaselineCfg())
	if string(out) != string(payload) {
		t.Fatalf("body without an environment block must be unchanged, got: %s", out)
	}
}

// TestNormalizeAccountEnv_MarkdownEnvDoesNotTouchToolBlocks covers the safety
// boundary for the Markdown path: even when a tool_result/tool_use carries text
// that looks like env keys or real paths, those blocks are never rewritten.
func TestNormalizeAccountEnv_MarkdownEnvDoesNotTouchToolBlocks(t *testing.T) {
	payload := []byte(`{
		"system": [{"type": "text", "text": "# Environment\n - Primary working directory: /tmp/prodsess1\n - Platform: linux\n"}],
		"messages": [
			{"role": "assistant", "content": [
				{"type": "tool_use", "id": "t1", "name": "Bash", "input": {"command": "pwd && uname", "cwd": "/tmp/prodsess1"}}
			]},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "t1", "content": "/tmp/prodsess1\nPlatform: linux\nOS Version: Linux 6.8.0-111-generic"}
			]}
		]
	}`)
	auth := authForEnv("acct-tool.json")
	out := NormalizeAccountEnv(payload, auth, "", macBaselineCfg())

	// System Markdown env IS normalized.
	sys := gjson.GetBytes(out, "system.0.text").String()
	if strings.Contains(sys, "/tmp/prodsess1") || strings.Contains(sys, "Platform: linux") {
		t.Fatalf("system Markdown env should be normalized: %q", sys)
	}
	// tool_use input must be byte-identical.
	if gjson.GetBytes(out, "messages.0.content.0.input.cwd").String() != "/tmp/prodsess1" {
		t.Fatalf("tool_use input must not be rewritten: %s", out)
	}
	// tool_result content must be byte-identical (real paths/OS untouched).
	tr := gjson.GetBytes(out, "messages.1.content.0.content").String()
	if !strings.Contains(tr, "/tmp/prodsess1") || !strings.Contains(tr, "Platform: linux") {
		t.Fatalf("tool_result content must not be rewritten: %q", tr)
	}
}

// TestNormalizeAccountEnv_AutoMemoryHeadingPathNormalized covers the leak observed
// in the measured outbound body: the "# auto memory" sentence lives under its OWN
// Markdown heading (separate from "# Environment"), so the env-block rewriter never
// reaches it and the real memory path used to leak unchanged. After the fix the
// embedded path is collapsed onto the per-account canonical memory directory.
func TestNormalizeAccountEnv_AutoMemoryHeadingPathNormalized(t *testing.T) {
	text := strings.Join([]string{
		"You are Claude Code.",
		"",
		"# Environment",
		"You have been invoked in the following environment:",
		" - Primary working directory: /Users/realuser/Project/app",
		" - Platform: darwin",
		" - OS Version: Darwin 24.6.0",
		"",
		"# auto memory",
		"",
		"You have a persistent, file-based memory system at `/Users/realuser/.claude/projects/-Users-realuser-Project-app/memory/`. This directory already exists.",
	}, "\n")
	payload := systemTextPayload(t, text)
	auth := authForEnv("acct-memory.json")
	out := NormalizeAccountEnv(payload, auth, "k", macBaselineCfg())

	got := gjson.GetBytes(out, "system.0.text").String()
	if strings.Contains(got, "/Users/realuser") {
		t.Fatalf("real memory/cwd path leaked: %q", got)
	}
	canonicalMem := accountCanonicalMemoryPath(AccountCanonicalCwd(auth, "k"))
	if !strings.Contains(got, "memory system at `"+canonicalMem+"`") {
		t.Fatalf("expected canonical memory path %q in %q", canonicalMem, got)
	}
}

// TestNormalizeAccountEnv_AutoMemoryInsideSystemReminder covers the same memory
// sentence when it is wrapped in a <system-reminder> span. The path must still be
// normalized, and it must match regardless of whether the env-block rewriter also
// touched the span.
func TestNormalizeAccountEnv_AutoMemoryInsideSystemReminder(t *testing.T) {
	text := strings.Join([]string{
		"<system-reminder>",
		"You have a persistent, file-based memory system at `/home/dev/.claude/projects/-home-dev-svc/memory/`. Write to it directly.",
		"</system-reminder>",
	}, "\n")
	payload := systemTextPayload(t, text)
	auth := authForEnv("acct-memory-sr.json")
	out := NormalizeAccountEnv(payload, auth, "", macBaselineCfg())

	got := gjson.GetBytes(out, "system.0.text").String()
	if strings.Contains(got, "/home/dev") {
		t.Fatalf("real memory path leaked: %q", got)
	}
	canonicalMem := accountCanonicalMemoryPath(AccountCanonicalCwd(auth, ""))
	if !strings.Contains(got, canonicalMem) {
		t.Fatalf("expected canonical memory path %q in %q", canonicalMem, got)
	}
}

// TestNormalizeAccountEnv_AutoMemoryIsolatedTmpRoot covers the test-isolation
// shape where the HOME is an isolated /tmp dir: the path ROOT is /tmp (not a home prefix) so a
// /Users|/home sweep would miss it, but the projects subdirectory STILL encodes
// the real user name ("-Users-corylin-..."). The key-anchored memory normalizer
// must collapse the whole backtick path regardless of its root.
func TestNormalizeAccountEnv_AutoMemoryIsolatedTmpRoot(t *testing.T) {
	text := strings.Join([]string{
		"# auto memory",
		"",
		"You have a persistent, file-based memory system at `/tmp/claude-iso-gcecT2/.claude/projects/-Users-corylin-Project-ai-cliproxy-stack/memory/`. This directory already exists.",
	}, "\n")
	payload := systemTextPayload(t, text)
	auth := authForEnv("acct-memory-tmp.json")
	out := NormalizeAccountEnv(payload, auth, "", macBaselineCfg())

	got := gjson.GetBytes(out, "system.0.text").String()
	if strings.Contains(got, "corylin") || strings.Contains(got, "claude-iso-gcecT2") {
		t.Fatalf("real user/iso path leaked: %q", got)
	}
	canonicalMem := accountCanonicalMemoryPath(AccountCanonicalCwd(auth, ""))
	if !strings.Contains(got, canonicalMem) {
		t.Fatalf("expected canonical memory path %q in %q", canonicalMem, got)
	}
}

// TestNormalizeAccountEnv_MemoryNormalizationDoesNotTouchToolBlocks guards the
// scope discipline: a tool_use input arg and a tool_result content that happen to
// contain the literal "memory system at `...`" phrase are NOT environment text and
// must pass through byte-for-byte. Only system / plain-text message blocks are
// visited by the normalizer, so these never reach the memory pattern.
func TestNormalizeAccountEnv_MemoryNormalizationDoesNotTouchToolBlocks(t *testing.T) {
	payload := []byte(`{
		"messages": [
			{"role": "assistant", "content": [
				{"type": "tool_use", "id": "t1", "name": "Read", "input": {"path": "/Users/realuser/.claude/projects/-Users-realuser-x/memory/notes.md"}}
			]},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "t1", "content": "file-based memory system at ` + "`" + `/Users/realuser/.claude/projects/-Users-realuser-x/memory/` + "`" + `"}
			]}
		]
	}`)
	auth := authForEnv("acct-memory-tool.json")
	out := NormalizeAccountEnv(payload, auth, "", macBaselineCfg())

	if gjson.GetBytes(out, "messages.0.content.0.input.path").String() != "/Users/realuser/.claude/projects/-Users-realuser-x/memory/notes.md" {
		t.Fatalf("tool_use input must not be rewritten: %s", out)
	}
	tr := gjson.GetBytes(out, "messages.1.content.0.content").String()
	if !strings.Contains(tr, "/Users/realuser/.claude/projects/-Users-realuser-x/memory/") {
		t.Fatalf("tool_result content must not be rewritten: %q", tr)
	}
}
