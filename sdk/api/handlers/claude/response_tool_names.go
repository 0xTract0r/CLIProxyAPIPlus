package claude

import (
	"bytes"
	"io"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const maxClaudeToolNameEventLine = 64 << 10

// claudeDeclaredToolNames restores only explicitly declared, unambiguous tool
// names in complete tool_use start events. The ID and input stay untouched.
type claudeDeclaredToolNames struct {
	names       map[string]string
	pending     []byte
	passthrough bool
}

func newClaudeDeclaredToolNames(request []byte) *claudeDeclaredToolNames {
	names := make(map[string]string)
	for _, tool := range gjson.GetBytes(request, "tools").Array() {
		name := tool.Get("name")
		if name.Type != gjson.String || name.Str == "" {
			continue
		}
		fold := strings.ToLower(name.Str)
		if previous, exists := names[fold]; exists && previous != name.Str {
			names[fold] = ""
		} else {
			names[fold] = name.Str
		}
	}
	if len(names) == 0 {
		return nil
	}
	return &claudeDeclaredToolNames{names: names}
}

func (t *claudeDeclaredToolNames) rewriteLine(line []byte) []byte {
	data, ok := bytes.CutPrefix(line, []byte("data:"))
	if !ok {
		return line
	}
	payload := bytes.TrimSpace(data)
	if !gjson.ValidBytes(payload) {
		return line
	}
	root := gjson.ParseBytes(payload)
	if root.Get("type").Str != "content_block_start" || root.Get("content_block.type").Str != "tool_use" {
		return line
	}
	name := root.Get("content_block.name")
	declared := t.names[strings.ToLower(name.Str)]
	if name.Type != gjson.String || declared == "" || declared == name.Str {
		return line
	}
	changed, err := sjson.SetBytes(payload, "content_block.name", declared)
	if err != nil {
		return line
	}
	start := bytes.Index(line, payload)
	return bytes.Join([][]byte{line[:start], changed, line[start+len(payload):]}, nil)
}

func (t *claudeDeclaredToolNames) rewriteChunk(chunk []byte) []byte {
	if t == nil || len(chunk) == 0 {
		return chunk
	}
	var out bytes.Buffer
	for len(chunk) > 0 {
		end := bytes.IndexByte(chunk, '\n')
		if end < 0 {
			if !t.passthrough && len(t.pending)+len(chunk) <= maxClaudeToolNameEventLine &&
				(len(t.pending) > 0 || bytes.HasPrefix(chunk, []byte("data:")) || bytes.HasPrefix([]byte("data:"), chunk)) {
				t.pending = append(t.pending, chunk...)
				break
			}
			out.Write(t.pending)
			t.pending = nil
			out.Write(chunk)
			t.passthrough = true
			break
		}
		line := chunk[:end]
		chunk = chunk[end+1:]
		if !t.passthrough && len(t.pending)+len(line) <= maxClaudeToolNameEventLine {
			if len(t.pending) > 0 {
				line = append(t.pending, line...)
			}
			line = t.rewriteLine(line)
		} else {
			out.Write(t.pending)
		}
		t.pending = nil
		t.passthrough = false
		out.Write(line)
		out.WriteByte('\n')
	}
	return out.Bytes()
}

func (t *claudeDeclaredToolNames) flush(w io.Writer) {
	if t != nil && len(t.pending) > 0 {
		_, _ = w.Write(t.pending)
		t.pending = nil
	}
}
