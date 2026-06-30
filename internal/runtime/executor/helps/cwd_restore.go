package helps

import (
	"bytes"
	"context"
	"strings"
	"sync"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Response-side fake-root → real-root restoration (requirement ⑦, restore half).
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

// RestoreCwdInBytes is the []byte form of RestoreCwdInString. It is used by the
// codex response paths (whose fake roots are fixed literals appearing anywhere in
// the OpenAI-responses payload, mirroring replaceCodexIdentityResponsePayload's
// whole-payload swap).
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
		restored := RestoreCwdInString(pairs, raw)
		if restored != raw {
			path := "content." + index.String() + ".input"
			body, _ = sjson.SetRawBytes(body, path, []byte(restored))
		}
		return true
	})
	return body
}
