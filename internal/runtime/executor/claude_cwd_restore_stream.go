package executor

import (
	"bytes"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	translatorcommon "github.com/router-for-me/CLIProxyAPI/v7/internal/translator/common"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// claudeCwdStreamRestorer restores the fake→real working directory inside the
// path arguments of Anthropic streaming tool_use blocks, the response-side half
// of the account cwd normalization (requirement ⑦).
//
// On the wire a tool_use block's arguments arrive as a sequence of
// content_block_delta / input_json_delta.partial_json fragments between a
// content_block_start (type "tool_use") and a content_block_stop. The fake root
// string can be split across two partial_json fragments, so a per-line
// ReplaceAll would miss boundary-split occurrences. This restorer therefore
// buffers all partial_json fragments of a tool_use block (keyed by the block's
// content_block.index so several concurrent blocks never interleave), and at
// content_block_stop re-emits a single input_json_delta carrying the reassembled,
// fake→real-restored arguments.
//
// Scope discipline: ONLY input_json_delta fragments of tool_use blocks are
// buffered and restored. text_delta (conversational output) and every other event
// pass through byte-for-byte unchanged.
//
// It mirrors the claudeInvokeRepairer frame interface (ProcessLine / Flush over
// SSE lines) but is independent: it must run for ALL clients (not just claude-cli
// with tools) and supports multiple concurrent content_block.index buffers, so it
// deliberately does not reuse the repairer's single-text-block enable gate.
type claudeCwdStreamRestorer struct {
	pairs []helps.CwdRestorePair
	// frame accumulates the raw SSE lines of the current event (terminated by a
	// blank line), mirroring claudeInvokeRepairer's framing.
	frame [][]byte
	// toolUseIndex marks which content_block.index values are tool_use blocks
	// whose input_json_delta fragments must be buffered.
	toolUseIndex map[int64]bool
	// buffers holds the accumulated partial_json per tool_use content_block.index.
	buffers map[int64]*bytes.Buffer
}

// newClaudeCwdStreamRestorer returns a restorer for the given captured mappings.
// When there are no mappings it returns nil; a nil restorer is a transparent
// pass-through (ProcessLine emits the line unchanged), so callers can wire it in
// unconditionally without branching on the gate.
func newClaudeCwdStreamRestorer(pairs []helps.CwdRestorePair) *claudeCwdStreamRestorer {
	if len(pairs) == 0 {
		return nil
	}
	return &claudeCwdStreamRestorer{
		pairs:        pairs,
		toolUseIndex: make(map[int64]bool),
		buffers:      make(map[int64]*bytes.Buffer),
	}
}

// ProcessLine consumes one SSE line and returns zero or more fully-formed SSE
// event chunks (each newline-terminated). A nil restorer passes the line through.
func (r *claudeCwdStreamRestorer) ProcessLine(line []byte) [][]byte {
	if r == nil {
		return [][]byte{appendLineNewline(line)}
	}
	r.frame = append(r.frame, bytes.Clone(line))
	if len(line) != 0 {
		return nil
	}
	frame := r.frame
	r.frame = nil
	return r.processFrame(frame)
}

// Flush emits any trailing buffered frame (a stream that ended without a final
// blank line). A nil restorer has nothing to flush.
func (r *claudeCwdStreamRestorer) Flush() [][]byte {
	if r == nil || len(r.frame) == 0 {
		return nil
	}
	frame := r.frame
	r.frame = nil
	return r.processFrame(frame)
}

func (r *claudeCwdStreamRestorer) processFrame(frame [][]byte) [][]byte {
	data, ok := claudeFrameData(frame)
	if !ok {
		return [][]byte{encodeClaudeFrame(frame)}
	}
	root := gjson.ParseBytes(data)
	switch root.Get("type").String() {
	case "content_block_start":
		index := root.Get("index").Int()
		if root.Get("content_block.type").String() == "tool_use" {
			r.toolUseIndex[index] = true
			r.buffers[index] = &bytes.Buffer{}
		}
		return [][]byte{encodeClaudeFrame(frame)}

	case "content_block_delta":
		index := root.Get("index").Int()
		if !r.toolUseIndex[index] || root.Get("delta.type").String() != "input_json_delta" {
			return [][]byte{encodeClaudeFrame(frame)}
		}
		// Buffer this tool_use block's argument fragment and suppress the original
		// delta; the reassembled, restored arguments are re-emitted at stop.
		buf := r.buffers[index]
		if buf == nil {
			buf = &bytes.Buffer{}
			r.buffers[index] = buf
		}
		buf.WriteString(root.Get("delta.partial_json").String())
		return nil

	case "content_block_stop":
		index := root.Get("index").Int()
		if !r.toolUseIndex[index] {
			return [][]byte{encodeClaudeFrame(frame)}
		}
		out := r.emitRestoredToolUse(frame, index)
		delete(r.toolUseIndex, index)
		delete(r.buffers, index)
		return out

	default:
		return [][]byte{encodeClaudeFrame(frame)}
	}
}

// emitRestoredToolUse builds the re-emitted [delta, stop] frames for a tool_use
// block: one input_json_delta carrying the reassembled fake→real-restored
// arguments, followed by the original content_block_stop frame.
func (r *claudeCwdStreamRestorer) emitRestoredToolUse(stopFrame [][]byte, index int64) [][]byte {
	var args string
	if buf := r.buffers[index]; buf != nil {
		args = buf.String()
	}
	out := make([][]byte, 0, 2)
	if args != "" {
		restored := helps.RestoreCwdInString(r.pairs, args)
		delta := []byte(`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":""}}`)
		delta, _ = sjson.SetBytes(delta, "index", index)
		delta, _ = sjson.SetBytes(delta, "delta.partial_json", restored)
		// Re-emit as a full SSE event (event:+data:+blank line), matching the
		// upstream content_block_delta framing.
		out = append(out, translatorcommon.AppendSSEEventBytes(nil, "content_block_delta", delta, 2))
	}
	out = append(out, encodeClaudeFrame(stopFrame))
	return out
}

// claudeChunkLines splits a newline-joined SSE chunk produced by the cwd
// restorer back into individual lines (without the trailing newline) so the
// result can be re-fed to a line-oriented downstream (the invoke repairer or the
// stream translator). A trailing empty element from the final newline is dropped;
// internal blank lines (SSE event terminators) are preserved.
func claudeChunkLines(chunk []byte) [][]byte {
	parts := bytes.Split(chunk, []byte("\n"))
	if n := len(parts); n > 0 && len(parts[n-1]) == 0 {
		parts = parts[:n-1]
	}
	return parts
}

// restoreClaudeStreamCwdBlob runs a complete buffered SSE blob (multiple events
// separated by newlines) through a claudeCwdStreamRestorer and returns the
// reassembled blob with tool_use path arguments restored. Used by the non-stream
// Execute path when from != to buffers the whole upstream stream before
// translating it to a non-stream response. Returns data unchanged when there are
// no captured mappings.
func restoreClaudeStreamCwdBlob(pairs []helps.CwdRestorePair, data []byte) []byte {
	restorer := newClaudeCwdStreamRestorer(pairs)
	if restorer == nil {
		return data
	}
	lines := bytes.Split(data, []byte("\n"))
	var out []byte
	for _, line := range lines {
		for _, chunk := range restorer.ProcessLine(line) {
			out = append(out, chunk...)
		}
	}
	for _, chunk := range restorer.Flush() {
		out = append(out, chunk...)
	}
	return out
}
