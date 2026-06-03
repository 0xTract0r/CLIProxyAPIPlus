package executor

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/tidwall/gjson"
)

func TestEnsureImageGenerationTool_NoTools(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","input":"draw a cat"}`)
	result := ensureImageGenerationTool(body, "gpt-5.4")

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
	result := ensureImageGenerationTool(body, "gpt-5.4")

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
	result := ensureImageGenerationTool(body, "gpt-5.4")

	tools := gjson.GetBytes(result, "tools")
	arr := tools.Array()
	if len(arr) != 2 {
		t.Fatalf("expected 2 tools (no duplicate), got %d", len(arr))
	}
	if arr[0].Get("output_format").String() != "webp" {
		t.Fatalf("expected original output_format=webp preserved, got %s", arr[0].Get("output_format").String())
	}
}

func TestEnsureImageGenerationTool_EmptyToolsArray(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","tools":[]}`)
	result := ensureImageGenerationTool(body, "gpt-5.4")

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
	result := ensureImageGenerationTool(body, "gpt-5.4")

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
	result := ensureImageGenerationTool(body, "gpt-5.3-codex-spark")

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

func TestApplyImageGenerationPolicy_StripMode(t *testing.T) {
	cfg := &config.Config{}
	cfg.DisableImageGeneration = "strip"
	body := []byte(`{"model":"gpt-5.4","tools":[{"type":"image_generation","model":"gpt-image-2"}]}`)
	result := applyImageGenerationPolicy(cfg, body, "gpt-5.4")

	if gjson.GetBytes(result, "tools").Exists() {
		t.Fatalf("expected strip mode to remove image_generation tool, got %s", gjson.GetBytes(result, "tools").Raw)
	}
}

func TestApplyImageGenerationPolicy_StripModeCaseInsensitive(t *testing.T) {
	cfg := &config.Config{}
	cfg.DisableImageGeneration = "STRIP"
	body := []byte(`{"model":"gpt-5.4","tools":[{"type":"image_generation"}]}`)
	result := applyImageGenerationPolicy(cfg, body, "gpt-5.4")

	if gjson.GetBytes(result, "tools").Exists() {
		t.Fatalf("expected STRIP (case-insensitive) to strip tool, got %s", gjson.GetBytes(result, "tools").Raw)
	}
}

func TestApplyImageGenerationPolicy_DefaultOffInjects(t *testing.T) {
	// 默认（""）保持现有注入行为，不剥离。
	cfg := &config.Config{}
	body := []byte(`{"model":"gpt-5.4","input":"draw a cat"}`)
	result := applyImageGenerationPolicy(cfg, body, "gpt-5.4")

	tools := gjson.GetBytes(result, "tools")
	arr := tools.Array()
	if len(arr) != 1 || arr[0].Get("type").String() != "image_generation" {
		t.Fatalf("expected default mode to inject image_generation, got %s", tools.Raw)
	}
}

func TestApplyImageGenerationPolicy_ExplicitOffInjects(t *testing.T) {
	cfg := &config.Config{}
	cfg.DisableImageGeneration = "off"
	body := []byte(`{"model":"gpt-5.4","tools":[{"type":"function","name":"f1"}]}`)
	result := applyImageGenerationPolicy(cfg, body, "gpt-5.4")

	tools := gjson.GetBytes(result, "tools")
	arr := tools.Array()
	if len(arr) != 2 {
		t.Fatalf("expected off mode to inject (2 tools), got %d: %s", len(arr), tools.Raw)
	}
	if arr[1].Get("type").String() != "image_generation" {
		t.Fatalf("expected image_generation injected, got %s", tools.Raw)
	}
}

func TestApplyImageGenerationPolicy_NilCfgInjects(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","input":"draw a cat"}`)
	result := applyImageGenerationPolicy(nil, body, "gpt-5.4")

	if !gjson.GetBytes(result, "tools").Exists() {
		t.Fatalf("expected nil cfg to fall back to injection, got %s", string(result))
	}
}
