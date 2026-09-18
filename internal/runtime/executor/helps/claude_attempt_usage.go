package helps

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"strings"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

const claudeAttemptBodyLimit = 8 << 20
const claudeAttemptEventLimit = 256 << 10

func claudeAttemptInfo(req *http.Request) cliproxyexecutor.HTTPAttemptInfo {
	info := cliproxyexecutor.HTTPAttemptInfo{Provider: "claude"}
	if req.URL != nil {
		info.CountOnly = strings.HasSuffix(req.URL.Path, "/count_tokens")
	}
	if info.CountOnly {
		info.EstimateKnown = true
	}
	if req.GetBody == nil {
		return info
	}
	body, err := req.GetBody()
	if err != nil {
		return info
	}
	data, err := io.ReadAll(io.LimitReader(body, claudeAttemptBodyLimit+1))
	_ = body.Close()
	if err != nil || len(data) > claudeAttemptBodyLimit {
		return info
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(data, &object) != nil {
		return info
	}
	_ = json.Unmarshal(object["model"], &info.Model)
	if info.CountOnly {
		return info
	}
	maxOutput, ok := claudeAttemptInteger(object["max_tokens"])
	if !ok || maxOutput <= 0 || maxOutput > math.MaxInt64-int64(len(data)) {
		return info
	}
	if !claudeAttemptTextInput(object) {
		return info
	}
	// Counting every serialized byte (including schema/formatting overhead)
	// is intentionally conservative, rather than a characters/4 heuristic.
	info.EstimatedTokens, info.EstimateKnown = int64(len(data))+maxOutput, true
	return info
}

func claudeAttemptTextInput(body map[string]json.RawMessage) bool {
	for field := range body {
		switch field {
		case "model", "system", "messages", "tools", "max_tokens", "stream", "temperature", "top_p", "top_k", "stop_sequences", "metadata", "tool_choice", "thinking", "output_config", "service_tier":
		default:
			return false
		}
	}
	if system, exists := body["system"]; exists && !claudeAttemptContent(system, true) {
		return false
	}
	var messages []map[string]json.RawMessage
	if json.Unmarshal(body["messages"], &messages) != nil || len(messages) == 0 {
		return false
	}
	for _, message := range messages {
		var role string
		if json.Unmarshal(message["role"], &role) != nil || (role != "user" && role != "assistant" && role != "system") || !claudeAttemptContent(message["content"], false) {
			return false
		}
	}
	if tools, exists := body["tools"]; exists {
		var definitions []map[string]json.RawMessage
		if json.Unmarshal(tools, &definitions) != nil || definitions == nil {
			return false
		}
		for _, tool := range definitions {
			var kind string
			if raw, ok := tool["type"]; ok && (json.Unmarshal(raw, &kind) != nil || kind != "custom") {
				return false
			}
			var schema map[string]json.RawMessage
			if json.Unmarshal(tool["input_schema"], &schema) != nil || schema == nil {
				return false
			}
		}
	}
	return true
}

func claudeAttemptContent(raw json.RawMessage, textOnly bool) bool {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return false
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return true
	}
	var blocks []map[string]json.RawMessage
	if json.Unmarshal(raw, &blocks) != nil || blocks == nil {
		return false
	}
	for _, block := range blocks {
		var kind string
		if json.Unmarshal(block["type"], &kind) != nil {
			return false
		}
		switch kind {
		case "text", "thinking":
			field := "text"
			if kind == "thinking" {
				field = "thinking"
			}
			if textOnly && kind != "text" {
				return false
			}
			if value := bytes.TrimSpace(block[field]); len(value) == 0 || value[0] != '"' || json.Unmarshal(value, &text) != nil {
				return false
			}
		case "tool_use":
			if textOnly || !json.Valid(block["input"]) {
				return false
			}
		case "tool_result":
			if textOnly || !claudeAttemptContent(block["content"], false) {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func claudeAttemptInteger(raw json.RawMessage) (int64, bool) {
	var number json.Number
	if len(raw) == 0 || raw[0] == '"' || json.Unmarshal(raw, &number) != nil {
		return 0, false
	}
	n, err := number.Int64()
	return n, err == nil && n >= 0
}

// claudeAttemptUsage is independent of reporting/translation. SSE usage fields
// are cumulative snapshots: present fields replace earlier values, never sum.
type claudeAttemptUsage struct {
	count, stream           bool
	input, output, write    int64
	inputKnown, outputKnown bool
	finalOutputKnown        bool
	terminal, invalid       bool
	buffer, event           []byte
	dropping                bool
}

func (u *claudeAttemptUsage) merge(raw json.RawMessage) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		u.invalid = true
		return
	}
	for _, field := range []string{"input_tokens", "output_tokens", "cache_creation_input_tokens"} {
		value, exists := fields[field]
		if !exists {
			continue
		}
		n, ok := claudeAttemptInteger(value)
		if !ok {
			u.invalid = true
			continue
		}
		switch field {
		case "input_tokens":
			if u.inputKnown && n < u.input {
				u.invalid = true
			} else {
				u.input = n
			}
			u.inputKnown = true
		case "output_tokens":
			if u.outputKnown && n < u.output {
				u.invalid = true
			} else {
				u.output = n
			}
			u.outputKnown = true
		case "cache_creation_input_tokens":
			if n < u.write {
				u.invalid = true
			} else {
				u.write = n
			}
		}
	}
}

func (u *claudeAttemptUsage) consumeEvent() {
	if len(u.event) == 0 {
		return
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(u.event, &fields) != nil {
		u.invalid = true
		u.event = nil
		return
	}
	u.event = nil
	var kind string
	_ = json.Unmarshal(fields["type"], &kind)
	switch kind {
	case "message_start":
		var message map[string]json.RawMessage
		if json.Unmarshal(fields["message"], &message) == nil {
			if usage, ok := message["usage"]; ok {
				u.merge(usage)
			}
		}
	case "message_delta":
		// message_start output is only an initial cumulative value. The last
		// delta must supply output explicitly before terminal usage can refund.
		u.finalOutputKnown = false
		if usage, ok := fields["usage"]; ok {
			var totals map[string]json.RawMessage
			if json.Unmarshal(usage, &totals) == nil {
				_, u.finalOutputKnown = claudeAttemptInteger(totals["output_tokens"])
			}
			u.merge(usage)
		}
	case "message_stop":
		u.terminal = true
	case "error":
		u.invalid = true
	}
}

func (u *claudeAttemptUsage) feed(data []byte) {
	if !u.stream {
		if len(u.buffer)+len(data) > claudeAttemptBodyLimit {
			u.invalid = true
			u.buffer = nil
			u.dropping = true
		}
		if !u.dropping {
			u.buffer = append(u.buffer, data...)
		}
		return
	}
	for len(data) > 0 {
		index := bytes.IndexByte(data, '\n')
		chunk := data
		if index >= 0 {
			chunk = data[:index]
		}
		if len(u.buffer)+len(chunk) > claudeAttemptEventLimit {
			u.invalid = true
			u.buffer = nil
			u.dropping = true
		}
		if !u.dropping {
			u.buffer = append(u.buffer, chunk...)
		}
		if index < 0 {
			return
		}
		if !u.dropping {
			line := bytes.TrimSuffix(u.buffer, []byte{'\r'})
			if len(line) == 0 {
				u.consumeEvent()
			} else if bytes.HasPrefix(line, []byte("data:")) {
				part := bytes.TrimPrefix(line, []byte("data:"))
				part = bytes.TrimPrefix(part, []byte{' '})
				if len(u.event)+len(part)+1 > claudeAttemptEventLimit {
					u.invalid = true
					u.event = nil
				} else {
					if len(u.event) > 0 {
						u.event = append(u.event, '\n')
					}
					u.event = append(u.event, part...)
				}
			}
		}
		u.buffer = nil
		u.dropping = false
		data = data[index+1:]
	}
}

func (u *claudeAttemptUsage) result(eof bool) (bool, int64) {
	if u.stream {
		if eof && len(u.buffer) > 0 {
			u.feed([]byte{'\n'})
		}
		if eof {
			u.consumeEvent()
		}
	} else if eof && !u.dropping {
		var object map[string]json.RawMessage
		if json.Unmarshal(u.buffer, &object) == nil {
			if u.count {
				_, ok := claudeAttemptInteger(object["input_tokens"])
				u.terminal = ok
				u.inputKnown = ok
				u.outputKnown = ok
			} else {
				var kind string
				_ = json.Unmarshal(object["type"], &kind)
				u.terminal = kind == "message"
				if raw, ok := object["usage"]; ok {
					u.merge(raw)
				}
			}
		} else {
			u.invalid = true
		}
	}
	if u.count {
		return u.terminal && !u.invalid && eof, 0
	}
	tokens := u.input
	for _, n := range []int64{u.output, u.write} {
		if n > math.MaxInt64-tokens {
			u.invalid = true
			tokens = math.MaxInt64
			break
		}
		tokens += n
	}
	return u.terminal && u.inputKnown && u.outputKnown && (!u.stream || u.finalOutputKnown) && !u.invalid, tokens
}
