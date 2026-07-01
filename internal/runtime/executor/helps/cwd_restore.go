package helps

import (
	"bytes"
	"context"
	"strconv"
	"strings"
	"sync"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Response-side fake-root → real-root restoration (requirement ⑦, restore half).
//
// fork(anticorr): DORMANT. The restore half only matters when the outbound cwd
// normalization runs; since that is turned off in production (see LoadConfig +
// NormalizeAccountEnvEnabled), no collector is attached and these restore paths
// are effectively inert. Kept (not deleted) so re-enabling normalization also
// re-enables restore as a single reversible change.
//
// NormalizeAccountEnv / NormalizeCodexPaths rewrite the developer machine's real
// working directory (and the codex CODEX_HOME) in the OUTBOUND request to a
// per-account canonical "fake" root so several employees sharing one upstream
// account cannot be correlated by their on-disk paths. The model is then fed the
// fake root and, when it returns a tool call (Anthropic tool_use.input /
// OpenAI-responses function_call arguments), it composes ABSOLUTE paths rooted at
// the fake directory. The local agent executing that tool call then hits a
// non-existent fake path ("No such file or directory").
//
// The fix is symmetric to the outbound rewrite: at normalization time (the only
// place the real root is known — it is probed from the request body, not derivable
// from the account) we capture the fake→real mapping, then on the RESPONSE side we
// replace the fake root back with the real root inside tool-call path arguments.
// We never re-probe the real cwd on the response side.
//
// Scope discipline mirrors the outbound rewrite: only tool-call argument paths are
// restored. Conversational text (Anthropic text_delta / ordinary content) and
// tool_result content are never rewritten.

// CwdRestorePair is one fake→real root substitution captured during outbound
// normalization. Replacement is a literal substring swap (the fake root is a
// fixed, machine-neutral canonical path), so a path "<fake>/foo/bar" becomes
// "<real>/foo/bar".
type CwdRestorePair struct {
	Fake string
	Real string
}

// CwdRestoreCollector accumulates the fake→real mappings captured while
// normalizing a single request. It is attached to the request context so the
// normalize call site (inside applyCloaking / NormalizeCodexPaths) and the
// response handler share the same instance. It is safe for concurrent use; in
// practice normalization writes happen on the request goroutine before the
// response goroutine reads, but the mutex keeps it race-free regardless.
type CwdRestoreCollector struct {
	mu    sync.Mutex
	pairs []CwdRestorePair
}

// Add records one fake→real pair, ignoring empty or identity mappings and
// de-duplicating on the fake root (the fake root is per-account canonical, so a
// second real value for the same fake root would be ambiguous; first-seen wins,
// matching the "single canonical cwd per account" outbound semantics).
func (c *CwdRestoreCollector) Add(fake, real string) {
	if c == nil || fake == "" || real == "" || fake == real {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range c.pairs {
		if c.pairs[i].Fake == fake {
			return
		}
	}
	c.pairs = append(c.pairs, CwdRestorePair{Fake: fake, Real: real})
}

// Pairs returns a snapshot of the captured mappings. The result is safe to read
// without holding the collector lock.
func (c *CwdRestoreCollector) Pairs() []CwdRestorePair {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.pairs) == 0 {
		return nil
	}
	out := make([]CwdRestorePair, len(c.pairs))
	copy(out, c.pairs)
	return out
}

type cwdRestoreCollectorKey struct{}

// ContextWithCwdRestoreCollector attaches a fresh collector to ctx and returns
// both the derived context and the collector. The same pointer is reachable via
// CwdRestoreCollectorFromContext, so a downstream normalize call records into the
// instance the caller later reads on the response side.
func ContextWithCwdRestoreCollector(ctx context.Context) (context.Context, *CwdRestoreCollector) {
	if ctx == nil {
		ctx = context.Background()
	}
	if existing, ok := ctx.Value(cwdRestoreCollectorKey{}).(*CwdRestoreCollector); ok && existing != nil {
		return ctx, existing
	}
	collector := &CwdRestoreCollector{}
	return context.WithValue(ctx, cwdRestoreCollectorKey{}, collector), collector
}

// CwdRestoreCollectorFromContext returns the collector attached to ctx, or nil
// when none is present (e.g. normalization gate is off or the caller did not
// attach one). A nil collector's Add is a safe no-op.
func CwdRestoreCollectorFromContext(ctx context.Context) *CwdRestoreCollector {
	if ctx == nil {
		return nil
	}
	if c, ok := ctx.Value(cwdRestoreCollectorKey{}).(*CwdRestoreCollector); ok {
		return c
	}
	return nil
}

// RestoreCwdInString applies every captured fake→real substitution to s. Each
// fake root is a fixed canonical path, so a literal substring swap restores
// "<fake>/foo" to "<real>/foo". Returns s unchanged when there are no pairs or no
// fake root occurs in s.
func RestoreCwdInString(pairs []CwdRestorePair, s string) string {
	for _, p := range pairs {
		if p.Fake == "" || p.Real == "" || p.Fake == p.Real {
			continue
		}
		if strings.Contains(s, p.Fake) {
			s = strings.ReplaceAll(s, p.Fake, p.Real)
		}
	}
	return s
}

// RestoreCwdInBytes is the []byte form of RestoreCwdInString (a literal whole-buffer
// fake→real swap). It is retained as a low-level utility; the codex response path no
// longer uses it for tool-call argument restoration (that now goes through the
// structural, JSON-safe RestoreCodexFunctionCallCwdInResponse to avoid corrupting
// JSON when the real cwd contains backslashes/quotes/control chars).
func RestoreCwdInBytes(pairs []CwdRestorePair, b []byte) []byte {
	for _, p := range pairs {
		if p.Fake == "" || p.Real == "" || p.Fake == p.Real {
			continue
		}
		fake := []byte(p.Fake)
		if bytes.Contains(b, fake) {
			b = bytes.ReplaceAll(b, fake, []byte(p.Real))
		}
	}
	return b
}

// hasAnyFakeRoot reports whether s contains any captured fake root. Used to skip
// the (more expensive) structural restore when there is nothing to do.
func hasAnyFakeRoot(pairs []CwdRestorePair, s string) bool {
	for _, p := range pairs {
		if p.Fake == "" || p.Real == "" || p.Fake == p.Real {
			continue
		}
		if strings.Contains(s, p.Fake) {
			return true
		}
	}
	return false
}

// RestoreCwdInToolUseInputRaw restores fake→real cwd inside a tool-call input
// object given as a raw JSON string (Anthropic tool_use.input / OpenAI-responses
// function_call.arguments). Unlike the literal RestoreCwdInString swap, the
// substitution happens STRUCTURALLY: the object is parsed, every string value is
// decoded, fake→real is applied to the decoded value, then the value is written
// back with sjson.Set, which re-escapes JSON metacharacters. This keeps the result
// valid JSON even when the real cwd contains backslashes (Windows C:\Users\bob),
// quotes, or control characters — a literal text swap would otherwise inject those
// raw bytes and corrupt the JSON. Only string VALUES are rewritten; object keys
// are never touched. Returns raw unchanged when it is not a JSON object, when no
// fake root occurs, or when there are no pairs.
//
// Nested objects/arrays are walked recursively so a path argument at any depth is
// restored. The walk only descends into objects and arrays; numbers, bools, and
// null are left as-is.
func RestoreCwdInToolUseInputRaw(pairs []CwdRestorePair, raw string) string {
	if len(pairs) == 0 || raw == "" {
		return raw
	}
	if !hasAnyFakeRoot(pairs, raw) {
		return raw
	}
	parsed := gjson.Parse(raw)
	if !parsed.IsObject() && !parsed.IsArray() {
		return raw
	}
	out := raw
	out = restoreCwdInJSONValue(pairs, out, "", parsed)
	if !gjson.Valid(out) {
		// Defensive: never emit invalid JSON. If the structural rewrite somehow
		// produced an invalid document, fall back to the original raw (no restore)
		// rather than corrupting the downstream parser.
		return raw
	}
	return out
}

// restoreCwdInJSONValue walks one JSON value at sjson path basePath inside doc and
// returns doc with every descendant string value fake→real restored. basePath is
// "" for the document root. Only string leaves are rewritten (via sjson.Set, which
// escapes correctly); keys are never rewritten.
func restoreCwdInJSONValue(pairs []CwdRestorePair, doc, basePath string, value gjson.Result) string {
	switch {
	case value.IsObject():
		value.ForEach(func(key, child gjson.Result) bool {
			childPath := joinSjsonPath(basePath, escapeSjsonKey(key.String()))
			doc = restoreCwdInJSONValue(pairs, doc, childPath, gjson.Get(doc, childPath))
			return true
		})
	case value.IsArray():
		arr := value.Array()
		for i := range arr {
			childPath := joinSjsonPath(basePath, intToSjsonIndex(i))
			doc = restoreCwdInJSONValue(pairs, doc, childPath, gjson.Get(doc, childPath))
		}
	default:
		if value.Type == gjson.String {
			s := value.String()
			restored := RestoreCwdInString(pairs, s)
			if restored != s {
				// sjson.Set on a string value escapes JSON metacharacters in the
				// value, so backslashes/quotes/control chars in the real cwd are
				// encoded correctly and the document stays valid JSON.
				doc, _ = sjson.Set(doc, basePath, restored)
			}
		}
	}
	return doc
}

// joinSjsonPath joins a base sjson path with a child segment, handling the root
// (empty base) case.
func joinSjsonPath(base, child string) string {
	if base == "" {
		return child
	}
	return base + "." + child
}

// intToSjsonIndex renders an array index as its sjson path segment.
func intToSjsonIndex(i int) string {
	return strconv.Itoa(i)
}

// RestoreClaudeToolUseCwdInResponse restores fake→real cwd inside the path
// arguments of Anthropic non-stream tool_use blocks (content[].type=="tool_use",
// field "input"). Only the tool_use input object is rewritten; ordinary text
// content blocks and any other field are left byte-for-byte untouched, preserving
// the same scope discipline as the outbound rewrite. Returns body unchanged when
// there are no captured mappings or no tool_use block contains a fake root.
func RestoreClaudeToolUseCwdInResponse(pairs []CwdRestorePair, body []byte) []byte {
	if len(pairs) == 0 || len(body) == 0 || !gjson.ValidBytes(body) {
		return body
	}
	content := gjson.GetBytes(body, "content")
	if !content.Exists() || !content.IsArray() {
		return body
	}
	content.ForEach(func(index, part gjson.Result) bool {
		if part.Get("type").String() != "tool_use" {
			return true
		}
		input := part.Get("input")
		if !input.Exists() {
			return true
		}
		raw := input.Raw
		// Structural, JSON-safe restore: rewrite fake→real on the DECODED string
		// values inside the input object and re-escape on write-back, so a real cwd
		// containing backslashes/quotes/control chars cannot corrupt the JSON.
		restored := RestoreCwdInToolUseInputRaw(pairs, raw)
		if restored != raw {
			path := "content." + index.String() + ".input"
			body, _ = sjson.SetRawBytes(body, path, []byte(restored))
		}
		return true
	})
	return body
}

// codexFunctionCallTypes are the codex response item types whose "arguments"
// (a JSON-encoded string) carry tool-call path arguments to restore.
var codexFunctionCallTypes = map[string]bool{
	"function_call":    true,
	"custom_tool_call": true,
}

// RestoreCodexFunctionCallCwdInResponse restores fake→real cwd / CODEX_HOME inside
// the "arguments" of codex tool-call items, JSON-safely and scoped to tool-call
// arguments only (mirroring the claude tool_use.input discipline).
//
// It handles both response shapes the codex executor restores:
//   - a single streamed item at "item" (response.output_item.done), and
//   - the full "response.output" array (buffered response.completed).
//
// For each function_call / custom_tool_call item it parses the "arguments" string
// (itself a JSON object), structurally restores every string value, and writes the
// re-encoded arguments back with sjson (which re-escapes), so a real cwd containing
// backslashes/quotes/control chars cannot corrupt the JSON. Conversational text,
// reasoning, and tool outputs are NEVER rewritten here — that is the scope
// discipline the response-side restore must honor.
//
// When payload contains no recognizable function_call item (e.g. an SSE delta line
// or a non-JSON frame), it returns payload unchanged. Callers handle the literal
// fallback for unparseable payloads separately.
func RestoreCodexFunctionCallCwdInResponse(pairs []CwdRestorePair, payload []byte) ([]byte, bool) {
	if len(pairs) == 0 || len(payload) == 0 {
		return payload, false
	}
	body := payload
	// Streamed line frames are prefixed with "data: "; strip it so the JSON parses,
	// then re-attach on return.
	prefix := []byte(nil)
	if bytes.HasPrefix(body, []byte("data: ")) {
		prefix = []byte("data: ")
		body = body[len(prefix):]
	} else if bytes.HasPrefix(body, []byte("data:")) {
		prefix = []byte("data:")
		body = body[len(prefix):]
	}
	if !gjson.ValidBytes(body) {
		return payload, false
	}
	changed := false
	// Shape A: top-level single item (response.output_item.done).
	if item := gjson.GetBytes(body, "item"); item.Exists() {
		if newBody, ok := restoreCodexFunctionCallArgsAt(pairs, body, "item", item); ok {
			body = newBody
			changed = true
		}
	}
	// Shape B: full output array (response.completed).
	if output := gjson.GetBytes(body, "response.output"); output.IsArray() {
		output.ForEach(func(idx, item gjson.Result) bool {
			path := "response.output." + idx.String()
			if newBody, ok := restoreCodexFunctionCallArgsAt(pairs, body, path, item); ok {
				body = newBody
				changed = true
			}
			return true
		})
	}
	if !changed {
		return payload, false
	}
	if len(prefix) != 0 {
		out := make([]byte, 0, len(prefix)+len(body))
		out = append(out, prefix...)
		out = append(out, body...)
		return out, true
	}
	return body, true
}

// restoreCodexFunctionCallArgsAt restores the "arguments" of one item (at sjson
// path itemPath inside body) when it is a function_call / custom_tool_call. The
// arguments value is a JSON-encoded string; it is parsed, structurally restored,
// and written back with sjson.Set (which escapes the value). Returns the updated
// body and whether anything changed.
func restoreCodexFunctionCallArgsAt(pairs []CwdRestorePair, body []byte, itemPath string, item gjson.Result) ([]byte, bool) {
	if !codexFunctionCallTypes[strings.TrimSpace(item.Get("type").String())] {
		return body, false
	}
	args := item.Get("arguments")
	if args.Type != gjson.String {
		return body, false
	}
	argStr := args.String()
	if !hasAnyFakeRoot(pairs, argStr) {
		return body, false
	}
	restored := RestoreCwdInToolUseInputRaw(pairs, argStr)
	if restored == argStr {
		// Not a parseable JSON object (or nothing changed structurally); fall back to
		// a literal swap on the decoded string so a fake root is never left verbatim.
		// sjson.Set still re-escapes the value on write-back, keeping valid JSON.
		restored = RestoreCwdInString(pairs, argStr)
		if restored == argStr {
			return body, false
		}
	}
	newBody, err := sjson.SetBytes(body, itemPath+".arguments", restored)
	if err != nil {
		return body, false
	}
	return newBody, true
}
