package executor

import (
	"bytes"
	"fmt"
	"net/url"
	"strings"

	"github.com/MoeForge/moego/json"
	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
)

const (
	antigravityCountTokensPath = "/v1internal:countTokens"
	antigravityStreamPath      = "/v1internal:streamGenerateContent"
	antigravityGeneratePath    = "/v1internal:generateContent"
	antigravityAuthType        = "antigravity"
)

type AntigravityExecutor struct{ cfg *config.Config }

func NewAntigravityExecutor(cfg *config.Config) *AntigravityExecutor {
	return &AntigravityExecutor{cfg: cfg}
}
func (*AntigravityExecutor) Identifier() string         { return antigravityAuthType }
func (e *AntigravityExecutor) SupportsApplyPatch() bool { return e != nil }
func ResetAntigravityTransports()                       { helps.ResetProxyTransports() }
func ensureAntigravityGeminiLeadingUserContent(modelName string, payload []byte) []byte {
	if strings.Contains(strings.ToLower(modelName), "claude") {
		return payload
	}
	return helps.EnsureGeminiLeadingUserContent(payload, "request.contents")
}

func ensureAntigravityGeminiLeadingUserContentDoc(modelName string, doc *ojson.Doc) bool {
	if doc == nil || strings.Contains(strings.ToLower(modelName), "claude") {
		return false
	}
	return helps.EnsureGeminiLeadingUserContentDoc(doc, "request.contents")
}

// ensureAntigravityGeminiTrailingUserContent appends a synthetic empty user turn
// if the final turn is a model turn. Claude targets are left unchanged because
// the adapter rejects empty text parts.
func ensureAntigravityGeminiTrailingUserContent(modelName string, payload []byte) []byte {
	if strings.Contains(strings.ToLower(modelName), "claude") {
		return payload
	}
	return helps.EnsureGeminiTrailingUserContent(payload, "request.contents")
}

func ensureAntigravityGeminiTrailingUserContentDoc(modelName string, doc *ojson.Doc) bool {
	if doc == nil || strings.Contains(strings.ToLower(modelName), "claude") {
		return false
	}
	return helps.EnsureGeminiTrailingUserContentDoc(doc, "request.contents")
}

// ensureAntigravityGeminiBoundaryUserContent normalizes both leading and trailing
// turns for Gemini targets. A trailing reasoning-only model turn is dropped first
// so the request does not rely only on a synthetic empty user turn (#6530).
// Claude targets are left unchanged.
func ensureAntigravityGeminiBoundaryUserContent(modelName string, payload []byte) []byte {
	if strings.Contains(strings.ToLower(modelName), "claude") {
		return payload
	}
	payload = helps.DropGeminiTrailingThoughtOnlyContent(payload, "request.contents")
	return helps.EnsureGeminiBoundaryUserContent(payload, "request.contents")
}

func ensureAntigravityGeminiBoundaryUserContentDoc(modelName string, doc *ojson.Doc) bool {
	if doc == nil || strings.Contains(strings.ToLower(modelName), "claude") {
		return false
	}
	changed := helps.DropGeminiTrailingThoughtOnlyContentDoc(doc, "request.contents")
	if helps.EnsureGeminiBoundaryUserContentDoc(doc, "request.contents") {
		changed = true
	}
	return changed
}

type antigravityContentEdit struct {
	index       int64
	path        string
	start       int
	end         int
	replacement []byte
}

func normalizeAntigravityGeminiFunctionResponseRolesDoc(doc *ojson.Doc) bool {
	if doc == nil {
		return false
	}
	contents := doc.Get("request.contents")
	if !contents.IsArray() {
		return false
	}
	wrapped := []byte(`{"request":{"contents":` + contents.Raw + `}}`)
	normalized := normalizeAntigravityGeminiFunctionResponseRoles(wrapped)
	if bytes.Equal(normalized, wrapped) {
		return false
	}
	updatedContents := util.GetGJSONBytesNoCopy(normalized, "request.contents")
	if !updatedContents.IsArray() || updatedContents.Raw == contents.Raw {
		return false
	}
	return doc.SetRawBytes("request.contents", []byte(updatedContents.Raw)) == nil
}

// normalizeAntigravityGeminiFunctionResponseRoles edits each response turn in
// isolation, then splices all changed turns into the request with one body copy.
// Applying SJSON once per field made large histories scale with history size
// multiplied by the number of tool turns.
func normalizeAntigravityGeminiFunctionResponseRoles(rawJSON []byte) []byte {
	rawJSON = repairAntigravityGeminiFunctionResponseNames(rawJSON)
	contents := util.GetGJSONBytesNoCopy(rawJSON, "request.contents")
	if !contents.IsArray() {
		return rawJSON
	}
	type functionRef struct {
		id   string
		name string
	}

	edits := make([]antigravityContentEdit, 0)
	var pending []functionRef
	validOffsets := true
	contents.ForEach(func(contentIndex, content ojson.Result) bool {
		parts := content.Get("parts")
		if !parts.IsArray() {
			pending = nil
			return true
		}

		var calls, responses []functionRef
		var responseParts, otherParts []json.RawMessage
		partCount := 0
		hasOtherPart := false
		parts.ForEach(func(_, part ojson.Result) bool {
			partCount++
			switch {
			case part.Get("functionCall").Exists():
				calls = append(calls, functionRef{id: part.Get("functionCall.id").String(), name: part.Get("functionCall.name").String()})
			case part.Get("functionResponse").Exists():
				responses = append(responses, functionRef{id: part.Get("functionResponse.id").String(), name: part.Get("functionResponse.name").String()})
				responseParts = append(responseParts, json.RawMessage(part.Raw))
			default:
				hasOtherPart = true
				otherParts = append(otherParts, json.RawMessage(part.Raw))
			}
			return true
		})
		if partCount == 0 {
			pending = nil
			return true
		}
		if len(calls) > 0 && len(responses) == 0 {
			pending = calls
			return true
		}
		if len(responses) == 0 {
			if hasOtherPart {
				pending = nil
			}
			return true
		}
		if len(calls) > 0 {
			pending = nil
			return true
		}

		var contentDoc *ojson.Doc
		contentChanged := false
		if len(pending) > 0 && len(responses) > 0 {
			ordered := make([]json.RawMessage, 0, partCount)
			used := make([]bool, len(responses))
			for _, call := range pending {
				for responseIndex, response := range responses {
					if used[responseIndex] {
						continue
					}
					if (call.id != "" && response.id == call.id) || (call.id == "" && call.name != "" && response.name == call.name) {
						used[responseIndex] = true
						ordered = append(ordered, responseParts[responseIndex])
						break
					}
				}
			}
			for responseIndex := range responses {
				if !used[responseIndex] {
					ordered = append(ordered, responseParts[responseIndex])
				}
			}
			if len(ordered) == len(responseParts) {
				ordered = append(ordered, otherParts...)
				encoded, errMarshal := json.Marshal(ordered)
				if errMarshal == nil && !bytes.Equal(encoded, []byte(parts.Raw)) {
					contentDoc = ojson.NewDoc([]byte(content.Raw))
					if errSet := contentDoc.SetRawBytes("parts", encoded); errSet == nil {
						contentChanged = true
					}
				}
			}
		}
		pending = nil
		if !hasOtherPart && content.Get("role").String() != "model" {
			if contentDoc == nil {
				contentDoc = ojson.NewDoc([]byte(content.Raw))
			}
			if errSet := contentDoc.Set("role", "model"); errSet == nil {
				contentChanged = true
			}
		}
		if !contentChanged || contentDoc == nil {
			return true
		}
		contentJSON := contentDoc.Bytes()

		start := content.Index
		end := start + len(content.Raw)
		if start < 0 || end < start || end > len(rawJSON) || !bytes.Equal(rawJSON[start:end], []byte(content.Raw)) {
			validOffsets = false
		}
		edits = append(edits, antigravityContentEdit{
			index:       contentIndex.Int(),
			start:       start,
			end:         end,
			replacement: contentJSON,
		})
		return true
	})
	return applyAntigravityIndexedEdits(rawJSON, edits, validOffsets)
}

// applyAntigravityIndexedEdits splices collected JSON fragments into the original
// request with one body copy. Applying SJSON once per field made large histories
// scale with history size multiplied by the number of edits.
func applyAntigravityIndexedEdits(rawJSON []byte, edits []antigravityContentEdit, validOffsets bool) []byte {
	if len(edits) == 0 {
		return rawJSON
	}
	if !validOffsets || !ojson.ValidBytes(rawJSON) {
		return applyAntigravityContentEditsWithSJSON(rawJSON, edits)
	}

	finalSize := len(rawJSON)
	cursor := 0
	for _, edit := range edits {
		if edit.start < cursor {
			return applyAntigravityContentEditsWithSJSON(rawJSON, edits)
		}
		finalSize += len(edit.replacement) - (edit.end - edit.start)
		if finalSize < 0 {
			return applyAntigravityContentEditsWithSJSON(rawJSON, edits)
		}
		cursor = edit.end
	}
	out := make([]byte, 0, finalSize)
	cursor = 0
	for _, edit := range edits {
		out = append(out, rawJSON[cursor:edit.start]...)
		out = append(out, edit.replacement...)
		cursor = edit.end
	}
	return append(out, rawJSON[cursor:]...)
}

// applyAntigravityContentEditsWithSJSON preserves the legacy path semantics if
// a GJSON result cannot be proven to point into the original request bytes.
func applyAntigravityContentEditsWithSJSON(rawJSON []byte, edits []antigravityContentEdit) []byte {
	doc := ojson.NewDoc(rawJSON)
	changed := false
	for _, edit := range edits {
		path := edit.path
		if path == "" {
			path = fmt.Sprintf("request.contents.%d", edit.index)
		}
		if errSet := doc.SetRawBytes(path, edit.replacement); errSet == nil {
			changed = true
		}
	}
	if !changed {
		return rawJSON
	}
	return doc.Bytes()
}

// repairAntigravityGeminiFunctionResponseNames copies missing or placeholder
// functionResponse names from the matching functionCall. Edits are applied to
// each part in isolation, then spliced into the request with one body copy.
func repairAntigravityGeminiFunctionResponseNames(rawJSON []byte) []byte {
	contents := util.GetGJSONBytesNoCopy(rawJSON, "request.contents")
	if !contents.IsArray() {
		return rawJSON
	}
	callIDToName := make(map[string]string)
	contents.ForEach(func(_, content ojson.Result) bool {
		parts := content.Get("parts")
		if !parts.IsArray() {
			return true
		}
		parts.ForEach(func(_, part ojson.Result) bool {
			fc := part.Get("functionCall")
			if fc.Exists() {
				id := strings.TrimSpace(fc.Get("id").String())
				name := strings.TrimSpace(fc.Get("name").String())
				if id != "" && name != "" && name != "unknown" {
					callIDToName[id] = name
				}
			}
			return true
		})
		return true
	})
	if len(callIDToName) == 0 {
		return rawJSON
	}

	edits := make([]antigravityContentEdit, 0)
	validOffsets := true
	contents.ForEach(func(contentIdx, content ojson.Result) bool {
		parts := content.Get("parts")
		if !parts.IsArray() {
			return true
		}
		parts.ForEach(func(partIdx, part ojson.Result) bool {
			fr := part.Get("functionResponse")
			if !fr.Exists() {
				return true
			}
			id := strings.TrimSpace(fr.Get("id").String())
			name := strings.TrimSpace(fr.Get("name").String())
			if id == "" || (name != "" && name != "unknown") {
				return true
			}
			realName, ok := callIDToName[id]
			if !ok {
				return true
			}
			updatedPart, errSet := ojson.SetBytes([]byte(part.Raw), "functionResponse.name", realName)
			if errSet != nil {
				return true
			}
			start := part.Index
			end := start + len(part.Raw)
			if start < 0 || end < start || end > len(rawJSON) || !bytes.Equal(rawJSON[start:end], []byte(part.Raw)) {
				validOffsets = false
			}
			edits = append(edits, antigravityContentEdit{
				index:       contentIdx.Int(),
				path:        fmt.Sprintf("request.contents.%d.parts.%d", contentIdx.Int(), partIdx.Int()),
				start:       start,
				end:         end,
				replacement: updatedPart,
			})
			return true
		})
		return true
	})
	return applyAntigravityIndexedEdits(rawJSON, edits, validOffsets)
}

func antigravitySameRedirectOrigin(a, b *url.URL) bool {
	if a == nil || b == nil || a.Hostname() == "" || b.Hostname() == "" {
		return false
	}
	return strings.EqualFold(a.Scheme, b.Scheme) &&
		strings.EqualFold(a.Hostname(), b.Hostname()) &&
		antigravityRedirectOriginPort(a) == antigravityRedirectOriginPort(b)
}

func antigravityRedirectOriginPort(u *url.URL) string {
	if u == nil {
		return ""
	}
	if port := u.Port(); port != "" {
		return port
	}
	switch strings.ToLower(u.Scheme) {
	case "http":
		return "80"
	case "https":
		return "443"
	default:
		return ""
	}
}
