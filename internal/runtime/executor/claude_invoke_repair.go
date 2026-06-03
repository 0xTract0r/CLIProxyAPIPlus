package executor

import (
	"bytes"
	"encoding/json"
	"html"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	translatorcommon "github.com/router-for-me/CLIProxyAPI/v6/internal/translator/common"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type claudeInvokeRepairer struct {
	enabled          bool
	allowedTools     map[string]bool
	frame            [][]byte
	textBlockIndex   int
	collectingInvoke bool
	invokeBuffer     strings.Builder
	repairedInvoke   *claudeTextInvoke
	nextToolIndex    int
}

type claudeTextInvoke struct {
	Name  string
	Input map[string]any
}

var (
	claudeInvokeTagRE       = regexp.MustCompile(`(?s)^\s*<invoke\s+name=["']([^"']+)["']\s*>(.*?)</invoke>\s*$`)
	claudeInvokeParameterRE = regexp.MustCompile(`(?s)<parameter\s+name=["']([^"']+)["']\s*>(.*?)</parameter>`)
)

func newClaudeInvokeRepairer(headers http.Header, requestBody []byte) *claudeInvokeRepairer {
	allowed := claudeRequestToolNames(requestBody)
	enabled := isClaudeCodeClientHeaders(headers) && len(allowed) > 0
	return &claudeInvokeRepairer{
		enabled:        enabled,
		allowedTools:   allowed,
		textBlockIndex: -1,
		nextToolIndex:  -1,
	}
}

func isClaudeCodeClientHeaders(headers http.Header) bool {
	if len(headers) == 0 {
		return false
	}
	ua := strings.ToLower(strings.TrimSpace(headers.Get("User-Agent")))
	return strings.HasPrefix(ua, "claude-cli/")
}

func claudeRequestToolNames(rawJSON []byte) map[string]bool {
	tools := gjson.GetBytes(rawJSON, "tools")
	if !tools.IsArray() {
		return nil
	}
	names := make(map[string]bool)
	tools.ForEach(func(_, tool gjson.Result) bool {
		name := strings.TrimSpace(tool.Get("name").String())
		if name != "" {
			names[name] = true
		}
		return true
	})
	return names
}

func (r *claudeInvokeRepairer) ProcessLine(line []byte) [][]byte {
	if r == nil || !r.enabled {
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

func (r *claudeInvokeRepairer) Flush() [][]byte {
	if r == nil || !r.enabled || len(r.frame) == 0 {
		return nil
	}
	frame := r.frame
	r.frame = nil
	return r.processFrame(frame)
}

func (r *claudeInvokeRepairer) processFrame(frame [][]byte) [][]byte {
	data, ok := claudeFrameData(frame)
	if !ok {
		return [][]byte{encodeClaudeFrame(frame)}
	}
	root := gjson.ParseBytes(data)
	eventType := root.Get("type").String()
	switch eventType {
	case "content_block_start":
		contentType := root.Get("content_block.type").String()
		if contentType == "text" {
			r.textBlockIndex = int(root.Get("index").Int())
			r.nextToolIndex = r.textBlockIndex + 1
		}
		return [][]byte{encodeClaudeFrame(frame)}

	case "content_block_delta":
		if int(root.Get("index").Int()) != r.textBlockIndex {
			return [][]byte{encodeClaudeFrame(frame)}
		}
		delta := root.Get("delta")
		if delta.Get("type").String() != "text_delta" {
			return [][]byte{encodeClaudeFrame(frame)}
		}
		text := delta.Get("text").String()
		if r.collectingInvoke {
			r.invokeBuffer.WriteString(text)
			return nil
		}
		invokeStart := strings.Index(text, "<invoke")
		if invokeStart < 0 {
			return [][]byte{encodeClaudeFrame(frame)}
		}
		prefix := text[:invokeStart]
		r.collectingInvoke = true
		r.invokeBuffer.Reset()
		r.invokeBuffer.WriteString(text[invokeStart:])
		if prefix == "" {
			return nil
		}
		updated, err := sjson.SetBytes([]byte(root.Raw), "delta.text", prefix)
		if err != nil {
			return nil
		}
		return [][]byte{replaceClaudeFrameData(frame, updated)}

	case "content_block_stop":
		if int(root.Get("index").Int()) != r.textBlockIndex || !r.collectingInvoke {
			return [][]byte{encodeClaudeFrame(frame)}
		}
		invoke, complete, valid := parseClaudeTextInvoke(r.invokeBuffer.String())
		if complete && valid && r.allowedTools[invoke.Name] {
			r.collectingInvoke = false
			r.invokeBuffer.Reset()
			r.repairedInvoke = &invoke
			out := [][]byte{encodeClaudeFrame(frame)}
			out = append(out, r.buildToolUseFrames(&invoke)...)
			return out
		}
		out := r.flushBufferedInvokeAsText(root)
		out = append(out, encodeClaudeFrame(frame))
		return out

	case "message_delta":
		if r.repairedInvoke == nil {
			return [][]byte{encodeClaudeFrame(frame)}
		}
		updated, err := sjson.SetBytes(data, "delta.stop_reason", "tool_use")
		if err != nil {
			return [][]byte{encodeClaudeFrame(frame)}
		}
		return [][]byte{replaceClaudeFrameData(frame, updated)}

	default:
		return [][]byte{encodeClaudeFrame(frame)}
	}
}

func parseClaudeTextInvoke(raw string) (claudeTextInvoke, bool, bool) {
	if !strings.Contains(raw, "</invoke>") {
		return claudeTextInvoke{}, false, false
	}
	matches := claudeInvokeTagRE.FindStringSubmatch(raw)
	if len(matches) != 3 {
		return claudeTextInvoke{}, true, false
	}
	name := strings.TrimSpace(html.UnescapeString(matches[1]))
	if name == "" {
		return claudeTextInvoke{}, true, false
	}
	input := make(map[string]any)
	for _, match := range claudeInvokeParameterRE.FindAllStringSubmatch(matches[2], -1) {
		if len(match) != 3 {
			continue
		}
		key := strings.TrimSpace(html.UnescapeString(match[1]))
		if key == "" {
			continue
		}
		value := html.UnescapeString(match[2])
		switch key {
		case "dangerouslyDisableSandbox", "run_in_background":
			if parsed, err := strconv.ParseBool(strings.TrimSpace(value)); err == nil {
				input[key] = parsed
			} else {
				input[key] = value
			}
		default:
			input[key] = value
		}
	}
	return claudeTextInvoke{Name: name, Input: input}, true, true
}

func (r *claudeInvokeRepairer) flushBufferedInvokeAsText(root gjson.Result) [][]byte {
	text := r.invokeBuffer.String()
	r.collectingInvoke = false
	r.invokeBuffer.Reset()
	if text == "" {
		return nil
	}
	updated := []byte(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":""}}`)
	updated, _ = sjson.SetBytes(updated, "index", root.Get("index").Int())
	updated, _ = sjson.SetBytes(updated, "delta.text", text)
	return [][]byte{translatorcommon.AppendSSEEventBytes(nil, "content_block_delta", updated, 2)}
}

func (r *claudeInvokeRepairer) buildToolUseFrames(invoke *claudeTextInvoke) [][]byte {
	if invoke == nil {
		return nil
	}
	index := r.nextToolIndex
	if index < 0 {
		index = r.textBlockIndex + 1
	}
	toolID := "toolu_repaired_" + strconv.Itoa(index)
	inputJSON, err := json.Marshal(invoke.Input)
	if err != nil {
		inputJSON = []byte(`{}`)
	}

	start := []byte(`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"","name":"","input":{},"caller":{"type":"direct"}}}`)
	start, _ = sjson.SetBytes(start, "index", index)
	start, _ = sjson.SetBytes(start, "content_block.id", toolID)
	start, _ = sjson.SetBytes(start, "content_block.name", invoke.Name)

	delta := []byte(`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":""}}`)
	delta, _ = sjson.SetBytes(delta, "index", index)
	delta, _ = sjson.SetBytes(delta, "delta.partial_json", string(inputJSON))

	stop := []byte(`{"type":"content_block_stop","index":0}`)
	stop, _ = sjson.SetBytes(stop, "index", index)

	return [][]byte{
		translatorcommon.AppendSSEEventBytes(nil, "content_block_start", start, 2),
		translatorcommon.AppendSSEEventBytes(nil, "content_block_delta", delta, 2),
		translatorcommon.AppendSSEEventBytes(nil, "content_block_stop", stop, 2),
	}
}

func claudeFrameData(frame [][]byte) ([]byte, bool) {
	for _, line := range frame {
		if bytes.HasPrefix(line, []byte("data:")) {
			return bytes.TrimSpace(line[len("data:"):]), true
		}
	}
	return nil, false
}

func replaceClaudeFrameData(frame [][]byte, data []byte) []byte {
	out := make([]byte, 0, len(data)+64)
	for _, line := range frame {
		if bytes.HasPrefix(line, []byte("data:")) {
			out = append(out, "data: "...)
			out = append(out, data...)
			out = append(out, '\n')
			continue
		}
		out = append(out, line...)
		out = append(out, '\n')
	}
	return out
}

func encodeClaudeFrame(frame [][]byte) []byte {
	out := make([]byte, 0, 128)
	for _, line := range frame {
		out = append(out, line...)
		out = append(out, '\n')
	}
	return out
}

func appendLineNewline(line []byte) []byte {
	out := make([]byte, len(line)+1)
	copy(out, line)
	out[len(line)] = '\n'
	return out
}
