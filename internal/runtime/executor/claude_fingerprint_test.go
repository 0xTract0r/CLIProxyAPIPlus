package executor

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/tidwall/sjson"
)

// Golden vectors captured account-free from genuine claude-cli 2.1.220 (Stage C
// real-machine validation). Each body's FIRST non-meta user message text, hashed
// as SHA256(salt + msg[4] + msg[7] + msg[20] + version)[:3] with UTF-16 code-unit
// indexing, reproduces the build segment the genuine client embedded in its
// system[0] x-anthropic-billing-header (cc_version=2.1.220.<build>) byte-for-byte.
//
// The literals below are the exact first user message texts from the capture
// artifacts (messages-run1-build204.body and run2/messages.body); only chars at
// indices 4/7/20 (all inside "<session>\n...") affect the build, but the full
// text is preserved for fidelity. The em-dash (U+2014) is verbatim from the
// captured prompt.
const (
	genuineFirstUserMsgBuild204 = "<session>\nabcdefghijklmnopqrstuvwxyz0123456789\n</session>\n\nWrite the title in the predominant language of the session — a stray word or code token in another language doesn't change it. Ignore the language of the examples above."
	genuineFirstUserMsgBuild784 = "<session>\nhello-world-DELTA-1234567890\n</session>\n\nWrite the title in the predominant language of the session — a stray word or code token in another language doesn't change it. Ignore the language of the examples above."
	// The genuine system[0] billing-header text of the build-204 capture. Hashing
	// THIS (the previous, buggy "first system text block" source) must NOT
	// reproduce 204 — proving the field is the first user message, not system[].
	genuineSystem0Build204 = "x-anthropic-billing-header: cc_version=2.1.220.204; cc_entrypoint=sdk-cli;"
	genuineCaptureVersion  = "2.1.220"

	// Multi-turn golden (Stage C, genuine claude-cli 2.1.220, cc_version build
	// d49). On the real main-conversation path the cli folds its leading isMeta
	// <system-reminder> into the FIRST genuine user message as a prepended text
	// block[0]; the wire messages[0].content is therefore
	// [block0=<system-reminder>…(~13KB), block1=this genuine turn-1 text], and the
	// cli hashes block1, NOT block0. This is the exact byte-for-byte block1 text
	// captured in req-001/002/003.body (msgs[0].content[1].text). Hashing it
	// reproduces the captured cc_version=2.1.220.d49; hashing block0 would yield
	// 746 (the bug the firstNonMetaUserMessageText reminder-skip fixes).
	genuineMultiTurnBlock1Buildd49 = "ZULU_FIRST_TURN_MARKER alpha bravo charlie delta echo foxtrot, this is the genuine first user turn of a multi turn probe; reply only ok"

	// Single-clean-message golden (Stage C, genuine claude-cli 2.1.220, cc_version
	// build 842) — the turn-0 title-generation body req-000.body, a single user
	// message whose only content block is genuine text (no folded reminder). Its
	// build must stay 842 under the reminder-skip change: a single non-reminder
	// block is returned unchanged, so this is the regression anchor proving the
	// fix does not perturb the pre-existing single-block path.
	genuineFirstUserMsgBuild842 = "<session>\nZULU_FIRST_TURN_MARKER alpha bravo charlie delta echo foxtrot, this is the genuine first user turn of a multi turn probe; reply only ok\n</session>\n\nWrite the title in the predominant language of the session — a stray word or code token in another language doesn't change it. Ignore the language of the examples above."
)

// TestComputeFingerprint_GoldenVectorsFromGenuineCapture pins the build algorithm
// (field + indexing + salt) against two independent genuine 2.1.220 triples.
func TestComputeFingerprint_GoldenVectorsFromGenuineCapture(t *testing.T) {
	if got := computeFingerprint(genuineFirstUserMsgBuild204, genuineCaptureVersion); got != "204" {
		t.Fatalf("golden run1: computeFingerprint(firstUserMsg, %q) = %q, want %q", genuineCaptureVersion, got, "204")
	}
	if got := computeFingerprint(genuineFirstUserMsgBuild784, genuineCaptureVersion); got != "784" {
		t.Fatalf("golden run2: computeFingerprint(firstUserMsg, %q) = %q, want %q", genuineCaptureVersion, got, "784")
	}
	// Multi-turn golden: the genuine turn-1 text folded to messages[0].content[1]
	// (block1) reproduces the captured cc_version build d49.
	if got := computeFingerprint(genuineMultiTurnBlock1Buildd49, genuineCaptureVersion); got != "d49" {
		t.Fatalf("golden multi-turn: computeFingerprint(block1, %q) = %q, want %q", genuineCaptureVersion, got, "d49")
	}
	// Single-clean-message golden: the turn-0 title-gen body reproduces build 842.
	if got := computeFingerprint(genuineFirstUserMsgBuild842, genuineCaptureVersion); got != "842" {
		t.Fatalf("golden single-block: computeFingerprint(firstUserMsg, %q) = %q, want %q", genuineCaptureVersion, got, "842")
	}

	// Negative: the first SYSTEM text block (the old, buggy source) must not
	// reproduce the genuine build. This is why hashing system[] was wrong.
	if got := computeFingerprint(genuineSystem0Build204, genuineCaptureVersion); got == "204" {
		t.Fatalf("system[0] text must NOT reproduce the genuine build 204, but it did: %q", got)
	}
}

// TestComputeFingerprint_ExtractedFromFirstUserMessageReproducesBuild proves the
// end-to-end field wiring: extracting the first non-meta user message from a
// genuine-shaped body (via firstNonMetaUserMessageText) and hashing it reproduces
// the build the genuine client put in its own billing header.
func TestComputeFingerprint_ExtractedFromFirstUserMessageReproducesBuild(t *testing.T) {
	body := []byte(`{"model":"claude-haiku-4-5-20251001"}`)
	var err error
	body, err = sjson.SetBytes(body, "system.0.text", genuineSystem0Build204)
	if err != nil {
		t.Fatalf("build body: %v", err)
	}
	body, err = sjson.SetBytes(body, "system.1.text", "You are a Claude agent, built on Anthropic's Claude Agent SDK.")
	if err != nil {
		t.Fatalf("build body: %v", err)
	}
	body, err = sjson.SetBytes(body, "messages.0.role", "user")
	if err != nil {
		t.Fatalf("build body: %v", err)
	}
	body, err = sjson.SetBytes(body, "messages.0.content.0.type", "text")
	if err != nil {
		t.Fatalf("build body: %v", err)
	}
	body, err = sjson.SetBytes(body, "messages.0.content.0.text", genuineFirstUserMsgBuild204)
	if err != nil {
		t.Fatalf("build body: %v", err)
	}

	msg, ok := firstNonMetaUserMessageText(body)
	if !ok {
		t.Fatal("expected a first user message")
	}
	if msg != genuineFirstUserMsgBuild204 {
		t.Fatalf("firstNonMetaUserMessageText = %q, want the genuine first user message", msg)
	}
	if got := computeFingerprint(msg, genuineCaptureVersion); got != "204" {
		t.Fatalf("build over extracted first user message = %q, want %q", got, "204")
	}
}

// runeFingerprint reproduces the OLD, buggy code-point ([]rune) indexing so the
// UTF-16 test can prove the implementation diverges from it.
func runeFingerprint(messageText, version string) string {
	runes := []rune(messageText)
	var sb strings.Builder
	for _, idx := range []int{4, 7, 20} {
		if idx < len(runes) {
			sb.WriteRune(runes[idx])
		} else {
			sb.WriteRune('0')
		}
	}
	h := sha256.Sum256([]byte(fingerprintSalt + sb.String() + version))
	return hex.EncodeToString(h[:])[:3]
}

// TestComputeFingerprint_UTF16CodeUnitIndexingNotCodePoints proves the build is
// indexed by UTF-16 code units (JS str[i]) rather than Unicode code points (Go
// []rune). A non-BMP char (U+1F600, one code point = two UTF-16 units) placed
// before index 20 shifts every later UTF-16 index by one relative to []rune
// indexing, so the two schemes select different chars at 4/7/20.
//
// Expected UTF-16 build "3ed" was derived the JS way (Node:
// crypto.sha256(salt + str[4]+str[7]+str[20] + ver).slice(0,3)); the test is
// self-contained (constants only) and additionally asserts it differs from the
// []rune result.
func TestComputeFingerprint_UTF16CodeUnitIndexingNotCodePoints(t *testing.T) {
	// U+1F600 occupies UTF-16 units [0,1]; 'a' starts at unit 2, so units[4]='c',
	// units[7]='f', units[20]='s'. As code points the emoji is one rune, so
	// runes[4]='d', runes[7]='g', runes[20]='t' — a different selection.
	msg := "\U0001F600abcdefghijklmnopqrstuvwxyz0123456789"

	const wantUTF16 = "3ed"
	if got := computeFingerprint(msg, genuineCaptureVersion); got != wantUTF16 {
		t.Fatalf("UTF-16 build = %q, want %q (implementation must index UTF-16 code units)", got, wantUTF16)
	}
	if got := runeFingerprint(msg, genuineCaptureVersion); got == wantUTF16 {
		t.Fatalf("code-point ([]rune) indexing coincidentally matched UTF-16 (%q); pick a divergent fixture", got)
	}
}

// TestComputeFingerprint_LoneSurrogateEncodedAsReplacementChar covers the subtle
// case where a build index lands ON one half of a surrogate pair. JS str[i]
// returns a lone surrogate string; Node's utf8 hashing substitutes U+FFFD
// (0xEF 0xBF 0xBD). "abcd" + U+1F600 puts the high surrogate at UTF-16 unit 4, so
// msg[4] is a lone surrogate while msg[7]='f' and msg[20]='s'.
//
// Expected "e25" derived the JS way (confirmed sha256('\uD83D')==sha256('�')).
func TestComputeFingerprint_LoneSurrogateEncodedAsReplacementChar(t *testing.T) {
	msg := "abcd\U0001F600efghijklmnopqrstuvwxyz"

	const wantUTF16 = "e25"
	if got := computeFingerprint(msg, genuineCaptureVersion); got != wantUTF16 {
		t.Fatalf("lone-surrogate build = %q, want %q (a lone surrogate must hash as U+FFFD)", got, wantUTF16)
	}
	// Sanity: jsUTF16CodeUnitAt on the high-surrogate unit returns U+FFFD.
	units := utf16.Encode([]rune(msg))
	if got := jsUTF16CodeUnitAt(units, 4); got != "�" {
		t.Fatalf("jsUTF16CodeUnitAt on a lone surrogate = %q, want U+FFFD", got)
	}
}

// TestComputeFingerprint_OutOfRangeIndexIsLiteralZero pins the empirically
// confirmed out-of-range fallback: a missing index contributes the literal '0'.
func TestComputeFingerprint_OutOfRangeIndexIsLiteralZero(t *testing.T) {
	// "abc" (len 3): indices 4/7/20 are all out of range → all '0'. "" is likewise
	// all out of range, so the two must produce the identical build.
	if a, b := computeFingerprint("abc", genuineCaptureVersion), computeFingerprint("", genuineCaptureVersion); a != b {
		t.Fatalf("all-out-of-range messages must hash identically (both '000'): %q != %q", a, b)
	}
	// It must equal an explicit salt + "000" + version hash.
	h := sha256.Sum256([]byte(fingerprintSalt + "000" + genuineCaptureVersion))
	if want, got := hex.EncodeToString(h[:])[:3], computeFingerprint("abc", genuineCaptureVersion); got != want {
		t.Fatalf("out-of-range build = %q, want salt+\"000\"+version hash %q", got, want)
	}
	// A char at index 4 (in range) must change the result away from the all-'0' case.
	if computeFingerprint("abcde", genuineCaptureVersion) == computeFingerprint("abc", genuineCaptureVersion) {
		t.Fatal("index 4 in range must contribute a non-'0' char and change the build")
	}
}

// TestFirstNonMetaUserMessageText covers the extraction contract used by both the
// cloaked billing injection and the real-path recompute.
func TestFirstNonMetaUserMessageText(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		wantText string
		wantOK   bool
	}{
		{"array content first text block", `{"messages":[{"role":"user","content":[{"type":"text","text":"hello world"}]}]}`, "hello world", true},
		{"string content", `{"messages":[{"role":"user","content":"plain text"}]}`, "plain text", true},
		{"skips leading assistant", `{"messages":[{"role":"assistant","content":"hi"},{"role":"user","content":"real"}]}`, "real", true},
		{"leading tool_result then text", `{"messages":[{"role":"user","content":[{"type":"tool_result","content":"r"},{"type":"text","text":"after tool"}]}]}`, "after tool", true},
		{"user with no text block", `{"messages":[{"role":"user","content":[{"type":"image","source":{}}]}]}`, "", true},
		{"no user message", `{"messages":[{"role":"assistant","content":"only me"}]}`, "", false},
		{"no messages array", `{"system":[]}`, "", false},
		// Reminder-skip: a folded <system-reminder> block[0] must be skipped in
		// favor of the genuine block[1] (the field genuine claude-cli hashes).
		{"array skips folded system-reminder block", `{"messages":[{"role":"user","content":[{"type":"text","text":"<system-reminder>\nctx\n</system-reminder>"},{"type":"text","text":"genuine after reminder"}]}]}`, "genuine after reminder", true},
		{"array skips reminder with leading whitespace", `{"messages":[{"role":"user","content":[{"type":"text","text":"  \n<system-reminder>ctx</system-reminder>"},{"type":"text","text":"genuine wsp"}]}]}`, "genuine wsp", true},
		// String content that is a folded reminder is skipped; the next user
		// message's genuine text is hashed instead.
		{"string content system-reminder skipped for next user", `{"messages":[{"role":"user","content":"<system-reminder>\nctx\n</system-reminder>"},{"role":"user","content":"real turn"}]}`, "real turn", true},
		// A user message whose only text blocks are reminders yields no text; scan
		// continues to the next user message.
		{"all reminder blocks then next user", `{"messages":[{"role":"user","content":[{"type":"text","text":"<system-reminder>a</system-reminder>"}]},{"role":"user","content":"second genuine"}]}`, "second genuine", true},
		// Only a reminder exists (no genuine user text anywhere): found stays true
		// (a user message existed) but the text is empty — preserves the
		// empty/no-cc_version edge behavior (no build over the reminder).
		{"only reminder user message returns empty but found", `{"messages":[{"role":"user","content":"<system-reminder>only</system-reminder>"}]}`, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotText, gotOK := firstNonMetaUserMessageText([]byte(tc.body))
			if gotOK != tc.wantOK {
				t.Fatalf("ok = %v, want %v", gotOK, tc.wantOK)
			}
			if gotText != tc.wantText {
				t.Fatalf("text = %q, want %q", gotText, tc.wantText)
			}
		})
	}
}

// TestFirstNonMetaUserMessageText_MultiTurnFoldedReminderReproducesBuild proves
// the end-to-end fix on the real main-conversation wire shape. claude-cli folds
// its leading isMeta <system-reminder> into messages[0] as a prepended block[0],
// so the wire content is [block0=<system-reminder>…, block1=genuine turn-1 text].
// firstNonMetaUserMessageText must skip block0 and return block1, and hashing the
// extracted text must reproduce the captured cc_version build d49 — NOT the build
// over the reminder block0 (the previous, buggy selection). A later user turn is
// present to confirm the first genuine text wins (find-first semantics).
func TestFirstNonMetaUserMessageText_MultiTurnFoldedReminderReproducesBuild(t *testing.T) {
	reminderBlock0 := "<system-reminder>\nAs you answer the user's questions, you can use the following context from the system:\n(folded meta context — the cli strips isMeta on the wire)\n</system-reminder>\n"

	body := []byte(`{"model":"claude-haiku-4-5-20251001"}`)
	set := func(path string, val interface{}) {
		var err error
		body, err = sjson.SetBytes(body, path, val)
		if err != nil {
			t.Fatalf("set %s: %v", path, err)
		}
	}
	// messages[0]: the folded first user turn (reminder block0 + genuine block1).
	set("messages.0.role", "user")
	set("messages.0.content.0.type", "text")
	set("messages.0.content.0.text", reminderBlock0)
	set("messages.0.content.1.type", "text")
	set("messages.0.content.1.text", genuineMultiTurnBlock1Buildd49)
	// A later assistant + user turn must NOT be selected: the first genuine text
	// wins, matching genuine claude-cli's find-first semantics.
	set("messages.1.role", "assistant")
	set("messages.1.content", "ok")
	set("messages.2.role", "user")
	set("messages.2.content.0.type", "text")
	set("messages.2.content.0.text", "SECOND_TURN_MUST_NOT_BE_SELECTED")

	msg, ok := firstNonMetaUserMessageText(body)
	if !ok {
		t.Fatal("expected a first user message")
	}
	if msg == reminderBlock0 {
		t.Fatal("firstNonMetaUserMessageText returned the folded <system-reminder> block0; it must skip it")
	}
	if msg != genuineMultiTurnBlock1Buildd49 {
		t.Fatalf("firstNonMetaUserMessageText = %q, want the genuine block1 text", msg)
	}
	if got := computeFingerprint(msg, genuineCaptureVersion); got != "d49" {
		t.Fatalf("build over extracted block1 = %q, want captured %q", got, "d49")
	}
	// Guard: hashing the reminder block0 (the old selection) does not reproduce
	// d49, so the skip is load-bearing.
	if got := computeFingerprint(reminderBlock0, genuineCaptureVersion); got == "d49" {
		t.Fatalf("reminder block0 must NOT reproduce build d49, but it did: %q", got)
	}
}

// TestFirstNonMetaUserMessageText_SingleCleanBlockGolden842Unchanged is the
// regression anchor: a single genuine user text block (no folded reminder, the
// turn-0 title-generation shape) is returned unchanged and reproduces the
// captured build 842. This proves the reminder-skip change does not perturb the
// pre-existing single-block path.
func TestFirstNonMetaUserMessageText_SingleCleanBlockGolden842Unchanged(t *testing.T) {
	body := []byte(`{"model":"claude-haiku-4-5-20251001"}`)
	var err error
	body, err = sjson.SetBytes(body, "messages.0.role", "user")
	if err != nil {
		t.Fatalf("build body: %v", err)
	}
	body, err = sjson.SetBytes(body, "messages.0.content.0.type", "text")
	if err != nil {
		t.Fatalf("build body: %v", err)
	}
	body, err = sjson.SetBytes(body, "messages.0.content.0.text", genuineFirstUserMsgBuild842)
	if err != nil {
		t.Fatalf("build body: %v", err)
	}

	msg, ok := firstNonMetaUserMessageText(body)
	if !ok {
		t.Fatal("expected a first user message")
	}
	if msg != genuineFirstUserMsgBuild842 {
		t.Fatalf("firstNonMetaUserMessageText = %q, want the genuine single-block text unchanged", msg)
	}
	if got := computeFingerprint(msg, genuineCaptureVersion); got != "842" {
		t.Fatalf("build over single clean block = %q, want captured %q", got, "842")
	}
}
