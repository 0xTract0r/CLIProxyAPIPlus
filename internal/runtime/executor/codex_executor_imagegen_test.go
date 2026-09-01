package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestCodexExecutorExecuteResponsesLiteHeaderDoesNotInjectImageGenerationTool(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, errRead := io.ReadAll(r.Body)
		if errRead != nil {
			t.Fatalf("read request body: %v", errRead)
		}
		gotBody = body
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":0,\"output_tokens\":0,\"total_tokens\":0}}}\n\n"))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		ProxyURL: "direct",
		Attributes: map[string]string{
			"api_key":   "test",
			"base_url":  server.URL,
			"plan_type": "pro",
		},
	}
	headers := make(http.Header)
	headers.Set("X-OpenAI-Internal-Codex-Responses-Lite", "true")

	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.6-sol",
		Payload: []byte(`{"model":"gpt-5.6-sol","input":"hello"}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Headers:      headers,
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if tools := gjson.GetBytes(gotBody, "tools"); tools.Exists() {
		t.Fatalf("unexpected tools in responses-lite upstream payload: %s", tools.Raw)
	}
	parallelToolCalls := gjson.GetBytes(gotBody, "parallel_tool_calls")
	if !parallelToolCalls.Exists() || parallelToolCalls.Bool() {
		t.Fatalf("responses-lite parallel_tool_calls should be false: %s", gotBody)
	}
}

func TestCodexExecutorExecuteStreamResponsesLiteHeaderForcesParallelToolCallsFalse(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, errRead := io.ReadAll(r.Body)
		if errRead != nil {
			t.Fatalf("read request body: %v", errRead)
		}
		gotBody = body
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":0,\"output_tokens\":0,\"total_tokens\":0}}}\n\n"))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		ProxyURL: "direct",
		Attributes: map[string]string{
			"api_key":   "test",
			"base_url":  server.URL,
			"plan_type": "pro",
		},
	}
	headers := make(http.Header)
	headers.Set(codexResponsesLiteHeader, "true")

	result, errExecute := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.6-luna",
		Payload: []byte(`{"model":"gpt-5.6-luna","input":"hello"}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Headers:      headers,
	})
	if errExecute != nil {
		t.Fatalf("ExecuteStream() error = %v", errExecute)
	}
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error = %v", chunk.Err)
		}
	}

	parallelToolCalls := gjson.GetBytes(gotBody, "parallel_tool_calls")
	if !parallelToolCalls.Exists() || parallelToolCalls.Bool() {
		t.Fatalf("responses-lite parallel_tool_calls should be false: %s", gotBody)
	}
}

func TestEnsureImageGenerationTool_ResponsesLiteMetadataDoesNotInjectTool(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6-sol","client_metadata":{"ws_request_header_x_openai_internal_codex_responses_lite":"true"},"input":[{"role":"user","content":"hello"}]}`)
	result := ensureImageGenerationTool(body, "gpt-5.6-sol", nil, nil)

	if string(result) != string(body) {
		t.Fatalf("expected responses-lite body to be unchanged, got %s", string(result))
	}
	if gjson.GetBytes(result, "tools").Exists() {
		t.Fatalf("expected no injected tools for responses-lite request, got %s", gjson.GetBytes(result, "tools").Raw)
	}
}

func TestEnsureImageGenerationTool_ResponsesLiteBooleanMetadataDoesNotInjectTool(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6-sol","client_metadata":{"ws_request_header_x_openai_internal_codex_responses_lite":true},"input":"hello"}`)
	result := ensureImageGenerationTool(body, "gpt-5.6-sol", nil, nil)

	if string(result) != string(body) {
		t.Fatalf("expected responses-lite body to be unchanged, got %s", string(result))
	}
}

func TestEnsureImageGenerationTool_ResponsesLiteHeaderDoesNotInjectTool(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6-sol","input":"hello"}`)
	headers := make(http.Header)
	headers.Set("X-OpenAI-Internal-Codex-Responses-Lite", "true")
	result := ensureImageGenerationTool(body, "gpt-5.6-sol", nil, headers)

	if string(result) != string(body) {
		t.Fatalf("expected responses-lite body to be unchanged, got %s", string(result))
	}
}

func TestEnsureImageGenerationTool_ResponsesLiteFalseMetadataStillInjectsTool(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6-sol","client_metadata":{"ws_request_header_x_openai_internal_codex_responses_lite":"false"},"input":"hello"}`)
	result := ensureImageGenerationTool(body, "gpt-5.6-sol", nil, nil)

	if got := gjson.GetBytes(result, "tools.0.type").String(); got != "image_generation" {
		t.Fatalf("tools.0.type = %q, want image_generation; body=%s", got, result)
	}
}

func TestEnsureImageGenerationTool_NoTools(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","input":"draw a cat"}`)
	result := ensureImageGenerationTool(body, "gpt-5.4", nil, nil)

	tools := gjson.GetBytes(result, "tools")
	if !tools.IsArray() {
		t.Fatalf("expected tools array, got %v", tools.Type)
	}
	arr := tools.Array()
	if len(arr) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(arr))
	}
	if arr[0].Get("type").String() != "image_generation" {
		t.Fatalf("expected type=image_generation, got %s", arr[0].Get("type").String())
	}
	if arr[0].Get("output_format").String() != "png" {
		t.Fatalf("expected output_format=png, got %s", arr[0].Get("output_format").String())
	}
}

func TestEnsureImageGenerationTool_ExistingToolsWithoutImageGen(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","tools":[{"type":"function","name":"get_weather","parameters":{}}]}`)
	result := ensureImageGenerationTool(body, "gpt-5.4", nil, nil)

	tools := gjson.GetBytes(result, "tools")
	arr := tools.Array()
	if len(arr) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(arr))
	}
	if arr[0].Get("type").String() != "function" {
		t.Fatalf("expected first tool type=function, got %s", arr[0].Get("type").String())
	}
	if arr[1].Get("type").String() != "image_generation" {
		t.Fatalf("expected second tool type=image_generation, got %s", arr[1].Get("type").String())
	}
}

func TestEnsureImageGenerationTool_AlreadyPresent(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","tools":[{"type":"image_generation","output_format":"webp"},{"type":"function","name":"f1"}]}`)
	result := ensureImageGenerationTool(body, "gpt-5.4", nil, nil)

	tools := gjson.GetBytes(result, "tools")
	arr := tools.Array()
	if len(arr) != 2 {
		t.Fatalf("expected 2 tools (no duplicate), got %d", len(arr))
	}
	if arr[0].Get("output_format").String() != "webp" {
		t.Fatalf("expected original output_format=webp preserved, got %s", arr[0].Get("output_format").String())
	}
}

func TestEnsureImageGenerationTool_ImageGenNamespaceDoesNotInjectTool(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","tools":[{"type":"namespace","name":"image_gen","tools":[{"type":"function","name":"imagegen","parameters":{}}]}]}`)
	result := ensureImageGenerationTool(body, "gpt-5.4", nil, nil)

	if string(result) != string(body) {
		t.Fatalf("expected body to be unchanged, got %s", string(result))
	}
}

func TestEnsureImageGenerationTool_FlattenedImageGenFunctionDoesNotInjectTool(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","tools":[{"type":"function","name":"image_gen.imagegen","parameters":{}}]}`)
	result := ensureImageGenerationTool(body, "gpt-5.4", nil, nil)

	if string(result) != string(body) {
		t.Fatalf("expected body to be unchanged, got %s", string(result))
	}
}

func TestEnsureImageGenerationTool_SimilarNamespaceStillInjectsTool(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","tools":[{"type":"namespace","name":"image_tools","tools":[{"type":"function","name":"imagegen","parameters":{}}]}]}`)
	result := ensureImageGenerationTool(body, "gpt-5.4", nil, nil)

	tools := gjson.GetBytes(result, "tools").Array()
	if len(tools) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(tools))
	}
	if tools[1].Get("type").String() != "image_generation" {
		t.Fatalf("expected second tool type=image_generation, got %s", tools[1].Get("type").String())
	}
}

func TestEnsureImageGenerationTool_EmptyToolsArray(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","tools":[]}`)
	result := ensureImageGenerationTool(body, "gpt-5.4", nil, nil)

	tools := gjson.GetBytes(result, "tools")
	arr := tools.Array()
	if len(arr) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(arr))
	}
	if arr[0].Get("type").String() != "image_generation" {
		t.Fatalf("expected type=image_generation, got %s", arr[0].Get("type").String())
	}
}

func TestEnsureImageGenerationTool_WebSearchAndImageGen(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","tools":[{"type":"web_search"}]}`)
	result := ensureImageGenerationTool(body, "gpt-5.4", nil, nil)

	tools := gjson.GetBytes(result, "tools")
	arr := tools.Array()
	if len(arr) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(arr))
	}
	if arr[0].Get("type").String() != "web_search" {
		t.Fatalf("expected first tool type=web_search, got %s", arr[0].Get("type").String())
	}
	if arr[1].Get("type").String() != "image_generation" {
		t.Fatalf("expected second tool type=image_generation, got %s", arr[1].Get("type").String())
	}
}

func TestEnsureImageGenerationTool_GPT53CodexSparkDoesNotInjectTool(t *testing.T) {
	body := []byte(`{"model":"gpt-5.3-codex-spark","input":"draw a cat"}`)
	result := ensureImageGenerationTool(body, "gpt-5.3-codex-spark", nil, nil)

	if string(result) != string(body) {
		t.Fatalf("expected body to be unchanged, got %s", string(result))
	}
	if gjson.GetBytes(result, "tools").Exists() {
		t.Fatalf("expected no tools for gpt-5.3-codex-spark, got %s", gjson.GetBytes(result, "tools").Raw)
	}
}

func TestStripImageGenerationTool_OnlyImageGen_RemovesToolsField(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","tools":[{"type":"image_generation","output_format":"png"}]}`)
	result := stripImageGenerationTool(body)

	if gjson.GetBytes(result, "tools").Exists() {
		t.Fatalf("expected tools field removed, got %s", gjson.GetBytes(result, "tools").Raw)
	}
}

func TestStripImageGenerationTool_KeepsOtherTools(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","tools":[{"type":"function","name":"get_weather"},{"type":"image_generation","output_format":"png"},{"type":"web_search"}]}`)
	result := stripImageGenerationTool(body)

	tools := gjson.GetBytes(result, "tools")
	arr := tools.Array()
	if len(arr) != 2 {
		t.Fatalf("expected 2 tools remaining, got %d: %s", len(arr), tools.Raw)
	}
	for _, tool := range arr {
		if tool.Get("type").String() == "image_generation" {
			t.Fatalf("expected no image_generation tool remaining, got %s", tools.Raw)
		}
	}
	if arr[0].Get("type").String() != "function" || arr[1].Get("type").String() != "web_search" {
		t.Fatalf("expected function+web_search preserved in order, got %s", tools.Raw)
	}
}

func TestStripImageGenerationTool_RemovesFullCodexDefinition(t *testing.T) {
	// Codex 客户端自带的完整 image_generation 定义（带 gpt-image-2 model）也要被剥离。
	body := []byte(`{"model":"gpt-5.4","tools":[{"type":"image_generation","model":"gpt-image-2","moderation":"low","n":1,"output_compression":100,"output_format":"png"}]}`)
	result := stripImageGenerationTool(body)

	if gjson.GetBytes(result, "tools").Exists() {
		t.Fatalf("expected full codex image_generation definition stripped, got %s", gjson.GetBytes(result, "tools").Raw)
	}
}

func TestStripImageGenerationTool_MultipleImageGen(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","tools":[{"type":"image_generation"},{"type":"function","name":"f1"},{"type":"image_generation","model":"gpt-image-2"}]}`)
	result := stripImageGenerationTool(body)

	tools := gjson.GetBytes(result, "tools")
	arr := tools.Array()
	if len(arr) != 1 {
		t.Fatalf("expected 1 tool remaining, got %d: %s", len(arr), tools.Raw)
	}
	if arr[0].Get("type").String() != "function" {
		t.Fatalf("expected function preserved, got %s", tools.Raw)
	}
}

func TestStripImageGenerationTool_NoTools_SafeReturn(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","input":"draw a cat"}`)
	result := stripImageGenerationTool(body)

	if string(result) != string(body) {
		t.Fatalf("expected body unchanged when no tools, got %s", string(result))
	}
}

func TestApplyImageGenerationPolicy_AllModeStrips(t *testing.T) {
	cfg := &config.Config{}
	cfg.DisableImageGeneration = config.DisableImageGenerationAll
	body := []byte(`{"model":"gpt-5.4","tools":[{"type":"image_generation","model":"gpt-image-2"}]}`)
	result := applyImageGenerationPolicy(cfg, body, "gpt-5.4", nil, nil)

	if gjson.GetBytes(result, "tools").Exists() {
		t.Fatalf("expected all mode to remove image_generation tool, got %s", gjson.GetBytes(result, "tools").Raw)
	}
}

func TestApplyImageGenerationPolicy_ChatModeStrips(t *testing.T) {
	// chat is the fork's loaded-config default: the Codex completion path strips
	// image_generation while /v1/images endpoints stay enabled.
	cfg := &config.Config{}
	cfg.DisableImageGeneration = config.DisableImageGenerationChat
	body := []byte(`{"model":"gpt-5.4","tools":[{"type":"image_generation"}]}`)
	result := applyImageGenerationPolicy(cfg, body, "gpt-5.4", nil, nil)

	if gjson.GetBytes(result, "tools").Exists() {
		t.Fatalf("expected chat mode to strip tool, got %s", gjson.GetBytes(result, "tools").Raw)
	}
}

func TestApplyImageGenerationPolicy_ChatModeDoesNotInject(t *testing.T) {
	cfg := &config.Config{}
	cfg.DisableImageGeneration = config.DisableImageGenerationChat
	body := []byte(`{"model":"gpt-5.4","input":"draw a cat"}`)
	result := applyImageGenerationPolicy(cfg, body, "gpt-5.4", nil, nil)

	if gjson.GetBytes(result, "tools").Exists() {
		t.Fatalf("expected chat mode to inject nothing, got %s", gjson.GetBytes(result, "tools").Raw)
	}
}

func TestApplyImageGenerationPolicy_OffModeInjects(t *testing.T) {
	// Only an explicit disable-image-generation: false (Off) re-injects the tool,
	// for organization-verified accounts. The free-plan guard still skips free auths.
	cfg := &config.Config{}
	cfg.DisableImageGeneration = config.DisableImageGenerationOff
	body := []byte(`{"model":"gpt-5.4","tools":[{"type":"function","name":"f1"}]}`)
	result := applyImageGenerationPolicy(cfg, body, "gpt-5.4", nil, nil)

	tools := gjson.GetBytes(result, "tools")
	arr := tools.Array()
	if len(arr) != 2 {
		t.Fatalf("expected off mode to inject (2 tools), got %d: %s", len(arr), tools.Raw)
	}
	if arr[1].Get("type").String() != "image_generation" {
		t.Fatalf("expected off mode to inject image_generation, got %s", tools.Raw)
	}
}

func TestApplyImageGenerationPolicy_NilCfgStrips(t *testing.T) {
	// nil cfg 走默认 strip 行为，且不 panic（strip 不依赖 cfg）。
	body := []byte(`{"model":"gpt-5.4","tools":[{"type":"image_generation","model":"gpt-image-2"}]}`)
	result := applyImageGenerationPolicy(nil, body, "gpt-5.4", nil, nil)

	if gjson.GetBytes(result, "tools").Exists() {
		t.Fatalf("expected nil cfg to strip (default), got %s", string(result))
	}
}

func TestEnsureImageGenerationTool_FreeCodexAuthDoesNotInjectTool(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","input":"draw a cat"}`)
	freeAuth := &cliproxyauth.Auth{ProxyURL: "direct",
		Provider:   "codex",
		Attributes: map[string]string{"plan_type": "free"},
	}
	result := ensureImageGenerationTool(body, "gpt-5.4", freeAuth, nil)

	if string(result) != string(body) {
		t.Fatalf("expected body to be unchanged, got %s", string(result))
	}
	if gjson.GetBytes(result, "tools").Exists() {
		t.Fatalf("expected no tools for free codex auth, got %s", gjson.GetBytes(result, "tools").Raw)
	}
}
