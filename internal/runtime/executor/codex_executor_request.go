package executor

import (
	"bytes"
	"context"
	"fmt"
	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"math"
	"net/http"
	"strconv"
	"strings"
)

const codexDefaultImageToolModel = "gpt-image-2"
const codexResponsesLiteHeader = "X-OpenAI-Internal-Codex-Responses-Lite"

var dataTag = []byte("data:")

const codexMinOutputTokens = 16

var imageGenToolArrayJSON = []byte(`[{"type":"image_generation","output_format":"png"}]`)
var webSearchToolJSON = []byte(`{"type":"web_search"}`)
var webSearchToolArrayJSON = []byte(`[{"type":"web_search"}]`)

func translateCodexRequestPair(from, to sdktranslator.Format, model string, originalPayload, payload []byte, stream bool, preserveEmptyThinkingBlocks ...bool) ([]byte, []byte) {
	original, body, _, _ := translateCodexRequestPairWithUpdateIntent(from, to, model, originalPayload, payload, stream, preserveEmptyThinkingBlocks...)
	return original, body
}

func translateCodexRequestPairWithUpdateIntent(from, to sdktranslator.Format, model string, originalPayload, payload []byte, stream bool, preserveEmptyThinkingBlocks ...bool) ([]byte, []byte, bool, error) {
	return translateCodexRequestPairWithUpdateIntentContext(context.Background(), from, to, model, originalPayload, payload, stream, preserveEmptyThinkingBlocks...)
}

func translateCodexRequestPairWithUpdateIntentContext(ctx context.Context, from, to sdktranslator.Format, model string, originalPayload, payload []byte, stream bool, preserveEmptyThinkingBlocks ...bool) ([]byte, []byte, bool, error) {
	isCompat := len(preserveEmptyThinkingBlocks) > 0 && preserveEmptyThinkingBlocks[0]
	if ctx == nil {
		ctx = context.Background()
	}
	translate := func(raw []byte) ([]byte, bool, error) {
		if isCompat && from == sdktranslator.FormatClaude && to == sdktranslator.FormatCodex {
			body, err := helps.TranslateRequestReturningError(ctx, nil, nil, from, to, model, raw, stream, true)
			return body, false, err
		}
		translated := sdktranslator.TranslateRequestEnvelope(ctx, from, to, sdktranslator.RequestEnvelope{Format: from, Model: model, Stream: stream, Body: raw})
		return translated.Body, translated.ConfigurationUpdatesChanged, translated.Err
	}
	if bytes.Equal(originalPayload, payload) {
		body, changed, err := translate(payload)
		return body, body, changed, err
	}
	originalTranslated, _, _ := translate(originalPayload)
	body, changed, err := translate(payload)
	return originalTranslated, body, changed, err
}

func normalizeCodexOutputBudgetDoc(doc *ojson.Doc) {
	if doc == nil {
		return
	}
	translated := doc.Get("max_output_tokens")
	if translated.Type != ojson.Number {
		return
	}
	tokens, errFloat := strconv.ParseFloat(translated.Raw, 64)
	if errFloat != nil || tokens < 1 || tokens >= codexMinOutputTokens || tokens != math.Trunc(tokens) {
		return
	}
	_ = doc.Set("max_output_tokens", codexMinOutputTokens)
}

func decodeCodexUpstreamResponse(resp *http.Response) error {
	if resp == nil || resp.Body == nil {
		return nil
	}
	decoded, errDecode := helps.DecodeResponseBody(resp.Body, resp.Header)
	if errDecode != nil {
		return fmt.Errorf("codex executor: decode upstream response: %w", errDecode)
	}
	resp.Body = decoded
	resp.Header.Del("Content-Encoding")
	resp.Header.Del("Content-Length")
	resp.ContentLength = -1
	return nil
}

func normalizeCodexInstructions(body []byte, nativeRequest ...bool) []byte {
	if len(nativeRequest) > 0 && nativeRequest[0] {
		return body
	}
	doc := ojson.NewDoc(body)
	if !normalizeCodexInstructionsDoc(doc, nativeRequest...) {
		return body
	}
	return doc.Bytes()
}

func normalizeCodexInstructionsDoc(doc *ojson.Doc, nativeRequest ...bool) bool {
	if doc == nil || (len(nativeRequest) > 0 && nativeRequest[0]) {
		return false
	}
	instructions := doc.Get("instructions")
	if !instructions.Exists() || instructions.Type == ojson.Null {
		return doc.Set("instructions", "") == nil
	}
	return false
}

func isCodexResponsesLiteRequestDoc(doc *ojson.Doc, headers http.Header) bool {
	return helps.CodexResponsesLiteRequestDoc(doc, headers)
}

func isCodexResponsesLiteModel(model string) bool {
	return helps.CodexModelUsesResponsesLite(model)
}

func isCodexWebSearchToolType(toolType string) bool {
	switch strings.TrimSpace(toolType) {
	case "web_search", "web_search_preview", "web_search_preview_2025_03_11":
		return true
	default:
		return false
	}
}

func ensureWebSearchToolForHistory(body []byte, headers http.Header) []byte {
	if !bytes.Contains(body, []byte("web_search_call")) {
		return body
	}
	doc := ojson.NewDoc(body)
	if !ensureWebSearchToolForHistoryDoc(doc, headers) {
		return body
	}
	return doc.Bytes()
}

func ensureWebSearchToolForHistoryDoc(doc *ojson.Doc, headers http.Header) bool {
	if doc == nil {
		return false
	}
	input := doc.Get("input")
	if !input.IsArray() || !strings.Contains(input.Raw, "web_search_call") {
		return false
	}
	hasWebSearchCall := false
	input.ForEach(func(_, item ojson.Result) bool {
		if strings.TrimSpace(item.Get("type").String()) == "web_search_call" {
			hasWebSearchCall = true
			return false
		}
		return true
	})
	if !hasWebSearchCall {
		return false
	}

	tools := doc.Get("tools")
	if tools.IsArray() {
		for _, t := range tools.Array() {
			if isCodexWebSearchToolType(t.Get("type").String()) {
				return false
			}
		}
	}

	if isCodexResponsesLiteRequestDoc(doc, headers) {
		items := input.Array()
		rebuilt := make([]string, 0, len(items))
		for _, item := range items {
			if strings.TrimSpace(item.Get("type").String()) == "web_search_call" {
				continue
			}
			rebuilt = append(rebuilt, item.Raw)
		}
		return doc.SetRawBytes("input", []byte("["+strings.Join(rebuilt, ",")+"]")) == nil
	}

	if !tools.Exists() || !tools.IsArray() {
		return doc.SetRawBytes("tools", webSearchToolArrayJSON) == nil
	}
	return doc.SetRawBytes("tools.-1", webSearchToolJSON) == nil
}

func isCodexFreePlanAuth(auth *cliproxyauth.Auth) bool {
	if auth == nil || auth.Attributes == nil {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(auth.Attributes["plan_type"]), "free")
}

func isImageGenerationFunctionTool(tool ojson.Result) bool {
	switch tool.Get("type").String() {
	case "function":
		return tool.Get("name").String() == "image_gen.imagegen"
	case "namespace":
		if tool.Get("name").String() != "image_gen" {
			return false
		}
		tools := tool.Get("tools")
		if !tools.IsArray() {
			return false
		}
		for _, nestedTool := range tools.Array() {
			if nestedTool.Get("type").String() == "function" && nestedTool.Get("name").String() == "imagegen" {
				return true
			}
		}
	}
	return false
}

func codexToolsAlreadyContainImageGenDoc(doc *ojson.Doc) bool {
	if doc == nil {
		return false
	}
	tools := doc.Get("tools")
	if !tools.Exists() || !tools.IsArray() {
		return false
	}
	for _, t := range tools.Array() {
		if t.Get("type").String() == "image_generation" || isImageGenerationFunctionTool(t) {
			return true
		}
	}
	return false
}

func resolveCodexGPTImage2BaseModel(cfg *config.Config) string {
	if cfg == nil {
		return codexOpenAIImagesMainModel
	}
	model := strings.TrimSpace(cfg.GPTImage2BaseModel)
	if model == "" {
		return codexOpenAIImagesMainModel
	}
	if !strings.HasPrefix(strings.ToLower(model), "gpt-") || isCodexResponsesLiteModel(model) {
		return codexOpenAIImagesMainModel
	}
	return model
}

func normalizeCodexImageGenerationToolsDoc(doc *ojson.Doc) bool {
	if doc == nil {
		return false
	}
	changed := false
	stripAtPath := func(toolsPath string) {
		tools := doc.Get(toolsPath)
		if !tools.IsArray() {
			return
		}
		for idx, tool := range tools.Array() {
			if strings.TrimSpace(tool.Get("type").String()) != "image_generation" {
				continue
			}
			if tool.Get("complexity").Exists() {
				if doc.Delete(fmt.Sprintf("%s.%d.complexity", toolsPath, idx)) == nil {
					changed = true
				}
			}
		}
	}
	stripAtPath("tools")
	input := doc.Get("input")
	if input.IsArray() {
		for idx, item := range input.Array() {
			if strings.TrimSpace(item.Get("type").String()) != "additional_tools" {
				continue
			}
			stripAtPath(fmt.Sprintf("input.%d.tools", idx))
		}
	}
	return changed
}

func normalizeCodexParallelToolCalls(body []byte, headers http.Header) []byte {
	doc := ojson.NewDoc(body)
	if !normalizeCodexParallelToolCallsDoc(doc, headers) {
		return body
	}
	return doc.Bytes()
}

func normalizeCodexParallelToolCallsDoc(doc *ojson.Doc, headers http.Header) bool {
	if doc == nil {
		return false
	}
	if isCodexResponsesLiteRequestDoc(doc, headers) {
		return helps.SetBoolIfDifferentDoc(doc, "parallel_tool_calls", false)
	}
	return normalizeCodexParallelToolCallsForToolsDoc(doc)
}

func normalizeCodexParallelToolCallsForTools(body []byte) []byte {
	doc := ojson.NewDoc(body)
	if !normalizeCodexParallelToolCallsForToolsDoc(doc) {
		return body
	}
	return doc.Bytes()
}

func normalizeCodexParallelToolCallsForToolsDoc(doc *ojson.Doc) bool {
	if doc == nil || !doc.Get("parallel_tool_calls").Exists() {
		return false
	}

	tools := doc.Get("tools")
	hasTools := tools.Exists() && tools.IsArray() && len(tools.Array()) > 0
	if hasTools {
		return false
	}

	return doc.Delete("parallel_tool_calls") == nil
}

func publishCodexImageToolUsage(ctx context.Context, reporter *helps.UsageReporter, body []byte, completedData []byte) {
	publishCodexImageToolUsageWithOutputItems(ctx, reporter, body, completedData, nil, nil)
}

func publishCodexImageToolUsageWithOutputItems(ctx context.Context, reporter *helps.UsageReporter, body []byte, completedData []byte, itemsByIndex map[int64][]byte, fallback [][]byte) {
	detail, ok := helps.ParseCodexImageToolUsageWithOutputItems(body, completedData, itemsByIndex, fallback)
	if !ok {
		return
	}
	reporter.EnsurePublished(ctx)
	reporter.PublishAdditionalModel(ctx, codexImageGenerationToolModel(body), detail)
}

func codexImageGenerationToolModel(body []byte) string {
	tools := ojson.GetBytesNoCopy(body, "tools")
	if tools.IsArray() {
		for _, tool := range tools.Array() {
			if tool.Get("type").String() != "image_generation" {
				continue
			}
			if model := strings.TrimSpace(tool.Get("model").String()); model != "" {
				return strings.Clone(model)
			}
			break
		}
	}
	return codexDefaultImageToolModel
}
