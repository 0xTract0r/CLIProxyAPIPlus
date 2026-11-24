package executor

import (
	"bytes"
	"encoding/json"
	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	"io"
	"net/http"
	"strings"
)

func claudeToolChoiceForcesToolUseDoc(doc *ojson.Doc) bool {
	if doc == nil {
		return false
	}
	toolChoiceType := doc.Get("tool_choice.type").String()
	return toolChoiceType == "any" || toolChoiceType == "tool"
}

func disableThinkingIfToolChoiceForcedDoc(doc *ojson.Doc) bool {
	// "auto" is allowed with thinking, but "any" or "tool" (specific tool) are not
	if claudeToolChoiceForcesToolUseDoc(doc) {
		changed := false
		// Remove thinking configuration entirely to avoid API error
		if doc.Delete("thinking") == nil {
			changed = true
		}
		// Adaptive thinking may also set output_config.effort; remove it to avoid
		// leaking thinking controls when tool_choice forces tool use.
		if doc.Delete("output_config.effort") == nil {
			changed = true
		}
		if oc := doc.Get("output_config"); oc.Exists() && oc.IsObject() && len(oc.Map()) == 0 {
			if doc.Delete("output_config") == nil {
				changed = true
			}
		}
		return changed
	}
	return false
}

func claudeResponseContentEncoding(header http.Header) string {
	return strings.Join(header.Values("Content-Encoding"), ",")
}

func decodeResponseBody(body io.ReadCloser, contentEncoding string) (io.ReadCloser, error) {
	return helps.DecodeResponseBody(body, http.Header{"Content-Encoding": {contentEncoding}})
}

func ensureModelMaxTokensDoc(doc *ojson.Doc, modelID string) bool {
	if doc == nil || doc.Len() == 0 {
		return false
	}

	if maxTokens := doc.Get("max_tokens"); maxTokens.Exists() {
		return false
	}

	for _, provider := range registry.GetGlobalRegistry().GetModelProviders(strings.TrimSpace(modelID)) {
		if strings.EqualFold(provider, "claude") {
			maxTokens := defaultModelMaxTokens
			if info := registry.GetGlobalRegistry().GetModelInfo(strings.TrimSpace(modelID), "claude"); info != nil && info.MaxCompletionTokens > 0 {
				maxTokens = info.MaxCompletionTokens
			}
			return doc.Set("max_tokens", maxTokens) == nil
		}
	}

	return false
}

func rejectOrdinaryClaudeDuplicateMetadata(payload []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	open, err := decoder.Token()
	if err != nil || open != json.Delim('{') {
		return nil
	}
	seen := false
	var unused json.RawMessage
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return nil
		}
		if key == "metadata" {
			if seen {
				return statusErr{code: http.StatusBadRequest, msg: "duplicate metadata in Claude request", requestScoped: true}
			}
			seen = true
		}
		if err = decoder.Decode(&unused); err != nil {
			return nil
		}
	}
	return nil
}
