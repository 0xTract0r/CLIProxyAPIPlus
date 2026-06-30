package executor

import (
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/tidwall/gjson"
)

// feedClaudeStreamRestorer runs a sequence of raw SSE lines through the restorer
// and returns the concatenated output blob.
func feedClaudeStreamRestorer(r *claudeCwdStreamRestorer, lines []string) string {
	var out []byte
	for _, line := range lines {
		for _, chunk := range r.ProcessLine([]byte(line)) {
			out = append(out, chunk...)
		}
	}
	for _, chunk := range r.Flush() {
		out = append(out, chunk...)
	}
	return string(out)
}

// TestClaudeCwdStreamRestorer_SplitPartialJSONReassembledAndRestored is the
// claude streaming red line: a fake-rooted path inside a tool_use block's
// input_json_delta is split across two partial_json fragments (so a naive
// per-line ReplaceAll would miss it). The restorer must reassemble the fragments,
// restore fake→real, and re-emit the complete arguments, while leaving text_delta
// (the conversational body) byte-for-byte unchanged.
func TestClaudeCwdStreamRestorer_SplitPartialJSONReassembledAndRestored(t *testing.T) {
	fake := "/Users/agent/workspace-deadbeef"
	real := "/Users/alice/Project/app"
	pairs := []helps.CwdRestorePair{{Fake: fake, Real: real}}

	// The fake root "/Users/agent/workspace-deadbeef" is cut between the two
	// input_json_delta fragments ("/Users/agent/works" + "pace-deadbeef/main.go").
	lines := []string{
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"I will read ` + fake + `/main.go"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"Read","input":{}}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"file_path\":\"/Users/agent/works"}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"pace-deadbeef/main.go\"}"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":1}`,
		``,
	}

	out := feedClaudeStreamRestorer(newClaudeCwdStreamRestorer(pairs), lines)

	// The reassembled tool_use arguments must parse to the REAL path. Match on the
	// parsed JSON (the SSE data line backslash-escapes the inner JSON quotes, so a
	// raw substring of the un-escaped form would not appear).
	args := collectToolUseArg(out, "file_path")
	if args != real+"/main.go" {
		t.Fatalf("tool_use file_path not restored: got %q want %q\nfull:\n%s", args, real+"/main.go", out)
	}

	// The text_delta (conversational body) must be untouched: the fake root that
	// appears in narrative text stays verbatim.
	if got := collectTextDelta(out); got != "I will read "+fake+"/main.go" {
		t.Fatalf("text_delta was modified (red line violation): %q\nfull:\n%s", got, out)
	}
}

// collectToolUseArg returns the value of the named key from the (last)
// reassembled input_json_delta in the SSE blob, parsing through the SSE + nested
// JSON escaping.
func collectToolUseArg(out, key string) string {
	var val string
	for _, raw := range strings.Split(out, "\n") {
		raw = strings.TrimSpace(raw)
		if !strings.HasPrefix(raw, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(raw, "data:"))
		root := gjson.Parse(payload)
		if root.Get("type").String() != "content_block_delta" || root.Get("delta.type").String() != "input_json_delta" {
			continue
		}
		pj := root.Get("delta.partial_json").String()
		if pj == "" {
			continue
		}
		if v := gjson.Get(pj, key); v.Exists() {
			val = v.String()
		}
	}
	return val
}

// collectTextDelta returns the concatenation of all text_delta texts in the blob.
func collectTextDelta(out string) string {
	var b strings.Builder
	for _, raw := range strings.Split(out, "\n") {
		raw = strings.TrimSpace(raw)
		if !strings.HasPrefix(raw, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(raw, "data:"))
		root := gjson.Parse(payload)
		if root.Get("type").String() == "content_block_delta" && root.Get("delta.type").String() == "text_delta" {
			b.WriteString(root.Get("delta.text").String())
		}
	}
	return b.String()
}

// TestClaudeCwdStreamRestorer_FlushDrainsTruncatedToolUse is the F1 truncation red
// line: a tool_use block emits several input_json_delta fragments but the stream
// ends (upstream cut / client disconnect / EOF) BEFORE content_block_stop. Flush()
// must re-emit the buffered, fake→real-restored arguments plus a synthetic
// content_block_stop instead of silently dropping the tool call's input.
func TestClaudeCwdStreamRestorer_FlushDrainsTruncatedToolUse(t *testing.T) {
	fake := "/Users/agent/workspace-deadbeef"
	real := "/Users/alice/Project/app"
	pairs := []helps.CwdRestorePair{{Fake: fake, Real: real}}

	// A tool_use block opens and buffers its argument fragments, but NO
	// content_block_stop arrives before the stream ends.
	lines := []string{
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"Read","input":{}}}`,
		``,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"file_path\":\"` + fake + `/main"}}`,
		``,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":".go\"}"}}`,
		``,
		// stream ends here: no content_block_stop.
	}

	out := feedClaudeStreamRestorer(newClaudeCwdStreamRestorer(pairs), lines)

	// The buffered, restored arguments must have been emitted by Flush (not lost).
	arg := collectToolUseArg(out, "file_path")
	if arg != real+"/main.go" {
		t.Fatalf("truncated tool_use args lost or unrestored: got %q want %q\nfull:\n%s", arg, real+"/main.go", out)
	}
	// Flush must also emit a synthetic content_block_stop so the block is well-formed.
	if !strings.Contains(out, `"type":"content_block_stop"`) {
		t.Fatalf("Flush did not emit a synthetic content_block_stop for the truncated block:\n%s", out)
	}
	// No fake root may survive anywhere in the output.
	if strings.Contains(out, fake) {
		t.Fatalf("fake root survived truncated flush:\n%s", out)
	}
}

// TestClaudeCwdStreamRestorer_BackslashRealCwdStaysValidJSON is the F2 escaping red
// line for the streaming path: when the real cwd contains a backslash (Windows
// C:\Users\bob), the re-emitted input_json_delta must remain VALID JSON (the
// backslash must be JSON-escaped, not injected raw) and decode to the real path.
func TestClaudeCwdStreamRestorer_BackslashRealCwdStaysValidJSON(t *testing.T) {
	fake := "/Users/agent/workspace-deadbeef"
	real := `C:\Users\bob` // backslashes: a literal swap would corrupt the JSON.
	pairs := []helps.CwdRestorePair{{Fake: fake, Real: real}}

	lines := []string{
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"Read","input":{}}}`,
		``,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"file_path\":\"` + fake + `/main.go\"}"}}`,
		``,
		`data: {"type":"content_block_stop","index":1}`,
		``,
	}

	out := feedClaudeStreamRestorer(newClaudeCwdStreamRestorer(pairs), lines)

	// Locate the re-emitted input_json_delta and assert its partial_json is valid JSON
	// (so the backslash was escaped, not injected raw).
	var pj string
	for _, raw := range strings.Split(out, "\n") {
		raw = strings.TrimSpace(raw)
		if !strings.HasPrefix(raw, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(raw, "data:"))
		root := gjson.Parse(payload)
		if root.Get("type").String() == "content_block_delta" && root.Get("delta.type").String() == "input_json_delta" {
			if v := root.Get("delta.partial_json").String(); v != "" {
				pj = v
			}
		}
	}
	if pj == "" {
		t.Fatalf("no reassembled input_json_delta emitted:\n%s", out)
	}
	if !gjson.Valid(pj) {
		t.Fatalf("re-emitted partial_json is not valid JSON (backslash injected raw): %q", pj)
	}
	got := gjson.Get(pj, "file_path").String()
	want := `C:\Users\bob/main.go`
	if got != want {
		t.Fatalf("file_path not restored to real backslash path: got %q want %q", got, want)
	}
}

// TestClaudeCwdStreamRestorer_NoPairsPassthrough verifies the gate-off path: a
// nil restorer (no captured mappings) forwards every line unchanged.
func TestClaudeCwdStreamRestorer_NoPairsPassthrough(t *testing.T) {
	lines := []string{
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"file_path\":\"/Users/agent/workspace-x/main.go\"}"}}`,
		``,
	}
	var restorer *claudeCwdStreamRestorer // nil: nothing captured
	out := feedClaudeStreamRestorer(restorer, lines)
	want := lines[0] + "\n" + lines[1] + "\n" + "\n"
	if out != want {
		t.Fatalf("nil restorer must pass through unchanged:\n got %q\nwant %q", out, want)
	}
}

// TestClaudeCwdStreamRestorer_ConcurrentToolIndexBuffersIndependent verifies the
// red line that multiple concurrent content_block.index tool_use blocks each keep
// an independent buffer and do not cross-contaminate.
func TestClaudeCwdStreamRestorer_ConcurrentToolIndexBuffersIndependent(t *testing.T) {
	fake := "/Users/agent/workspace-deadbeef"
	real := "/Users/alice/Project/app"
	pairs := []helps.CwdRestorePair{{Fake: fake, Real: real}}

	// Two tool_use blocks (index 1 and 2) whose input_json_delta fragments are
	// interleaved on the wire. Each must reassemble from its own buffer.
	lines := []string{
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"a","name":"Read","input":{}}}`,
		``,
		`data: {"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"b","name":"Read","input":{}}}`,
		``,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"p\":\"` + fake + `/a"}}`,
		``,
		`data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"p\":\"` + fake + `/b"}}`,
		``,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":".go\"}"}}`,
		``,
		`data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":".go\"}"}}`,
		``,
		`data: {"type":"content_block_stop","index":1}`,
		``,
		`data: {"type":"content_block_stop","index":2}`,
		``,
	}

	out := feedClaudeStreamRestorer(newClaudeCwdStreamRestorer(pairs), lines)
	if strings.Contains(out, fake) {
		t.Fatalf("fake root survived (buffers crossed?):\n%s", out)
	}
	got := collectAllToolUseArgs(out, "p")
	if !got[real+"/a.go"] {
		t.Fatalf("index-1 args not correctly reassembled/restored: %v\n%s", got, out)
	}
	if !got[real+"/b.go"] {
		t.Fatalf("index-2 args not correctly reassembled/restored: %v\n%s", got, out)
	}
}

// collectAllToolUseArgs returns the set of values for the named key across every
// reassembled input_json_delta in the blob.
func collectAllToolUseArgs(out, key string) map[string]bool {
	set := map[string]bool{}
	for _, raw := range strings.Split(out, "\n") {
		raw = strings.TrimSpace(raw)
		if !strings.HasPrefix(raw, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(raw, "data:"))
		root := gjson.Parse(payload)
		if root.Get("type").String() != "content_block_delta" || root.Get("delta.type").String() != "input_json_delta" {
			continue
		}
		pj := root.Get("delta.partial_json").String()
		if v := gjson.Get(pj, key); v.Exists() {
			set[v.String()] = true
		}
	}
	return set
}
