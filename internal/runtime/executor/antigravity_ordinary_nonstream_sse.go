package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/clienterror"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
)

// collectOrdinaryAntigravityResponse assembles native SSE returned by the
// same non-streaming request. Complete provider parts stay opaque and ordered.
func collectOrdinaryAntigravityResponse(ctx context.Context, wire []byte, headers http.Header) ([]byte, error) {
	wrapper := make(map[string]json.RawMessage)
	response := make(map[string]json.RawMessage)
	positions := make(map[int64]int)
	var candidates []map[string]json.RawMessage
	var parts [][]json.RawMessage
	var wrapped, complete, malformed bool
	var blocked string
	scanner := helps.NewCodexSSEScanner(ctx, bytes.NewReader(wire), 16<<20)
	for scanner.Scan() {
		line, dataLine := bytes.CutPrefix(scanner.Bytes(), []byte("data:"))
		if !dataLine {
			continue
		}
		data := bytes.TrimSpace(line)
		if bytes.Equal(data, []byte("[DONE]")) {
			complete = complete || !malformed
			break
		}
		var frame map[string]json.RawMessage
		if err := json.Unmarshal(data, &frame); err != nil || frame == nil {
			malformed = true
			continue
		}
		if err := ordinaryAntigravityInBandError(data, headers); err != nil {
			return nil, err
		}
		complete = complete || ordinaryAntigravityHasTerminalReason(data)
		if message, filtered := clienterror.GeminiFilterBlockMessage(data); filtered {
			blocked = message
			continue
		}
		if blocked != "" {
			if !ordinaryAntigravityHasOutput(data) {
				continue
			}
			blocked = ""
		}
		if inner, ok := frame["response"]; ok {
			wrapped = true
			delete(frame, "response")
			mergeOrdinaryAntigravityFrame(wrapper, frame, "usageMetadata")
			frame = nil
			if err := json.Unmarshal(inner, &frame); err != nil || frame == nil {
				malformed = true
				continue
			}
		}
		rawCandidates := frame["candidates"]
		delete(frame, "candidates")
		mergeOrdinaryAntigravityFrame(response, frame, "usageMetadata")
		for ordinal, picked := range ojson.GetBytesNoCopy(rawCandidates, "@this").Array() {
			id := int64(ordinal)
			if index := picked.Get("index"); index.Exists() {
				id = index.Int()
			}
			at, exists := positions[id]
			if !exists {
				at = len(candidates)
				positions[id] = at
				candidates = append(candidates, map[string]json.RawMessage{"index": json.RawMessage(strconv.FormatInt(id, 10))})
				parts = append(parts, nil)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal([]byte(picked.Raw), &fields); err != nil || fields == nil {
				return nil, statusErr{code: http.StatusBadGateway, msg: "invalid Antigravity SSE candidate"}
			}
			mergeOrdinaryAntigravityFrame(candidates[at], fields, "content")
			for _, part := range picked.Get("content.parts").Array() {
				parts[at] = append(parts[at], json.RawMessage(part.Raw))
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if blocked != "" {
		return nil, statusErr{code: http.StatusBadRequest, msg: blocked, requestScoped: true}
	}
	if !complete || len(candidates) == 0 {
		return nil, statusErr{code: http.StatusBadGateway, msg: "Antigravity stream ended before completion"}
	}
	for i, candidate := range candidates {
		if len(parts[i]) == 0 {
			continue
		}
		var content map[string]json.RawMessage
		if raw := candidate["content"]; len(raw) > 0 {
			_ = json.Unmarshal(raw, &content)
		}
		if content == nil {
			content = make(map[string]json.RawMessage)
		}
		content["parts"], _ = json.Marshal(parts[i])
		candidate["content"], _ = json.Marshal(content)
	}
	response["candidates"], _ = json.Marshal(candidates)
	if !wrapped {
		return json.Marshal(response)
	}
	wrapper["response"], _ = json.Marshal(response)
	return json.Marshal(wrapper)
}

// Later frames may supply only some keys in a usage or content object.
// Counts and other scalar values are kept as provided, never added together.
func mergeOrdinaryAntigravityFrame(dst, src map[string]json.RawMessage, partialObject string) {
	for key, value := range src {
		if key == partialObject && len(dst[key]) > 0 {
			var before, after map[string]json.RawMessage
			if json.Unmarshal(dst[key], &before) == nil && before != nil && json.Unmarshal(value, &after) == nil && after != nil {
				for name, updated := range after {
					before[name] = updated
				}
				if merged, err := json.Marshal(before); err == nil {
					value = merged
				}
			}
		}
		dst[key] = value
	}
}
