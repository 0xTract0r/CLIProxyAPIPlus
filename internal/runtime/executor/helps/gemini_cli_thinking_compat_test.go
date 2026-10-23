package helps_test

import (
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	_ "github.com/router-for-me/CLIProxyAPI/v8/internal/thinking/provider/antigravity"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
	"testing"
)

func TestGeminiCLIToAntigravityThinkingSummaryCompatibility(t *testing.T) {
	const model = "fork-gemini-cli-summary-model"
	registry.GetGlobalRegistry().RegisterClient(t.Name(), "antigravity", []*registry.ModelInfo{{ID: model, Thinking: &registry.ThinkingSupport{Min: 0, Max: 32768, ZeroAllowed: true, DynamicAllowed: true}}})
	defer registry.GetGlobalRegistry().UnregisterClient(t.Name())
	for _, tc := range []struct{ name, from, suffix, thinking, want string }{
		{"suffix enables legacy summary", "gemini-cli", "(8192)", "", "true"},
		{"body budget enables legacy summary", "gemini-cli", "", `,"generationConfig":{"thinkingConfig":{"thinkingBudget":8192}}`, "true"},
		{"explicit false wins suffix", "gemini-cli", "(8192)", `,"generationConfig":{"thinkingConfig":{"includeThoughts":false}}`, "false"},
		{"explicit snake false wins suffix", "gemini-cli", "(8192)", `,"generationConfig":{"thinkingConfig":{"include_thoughts":false}}`, "false"},
		{"body disabled suppresses summary", "gemini-cli", "(8192)", `,"generationConfig":{"thinkingConfig":{"thinkingBudget":0}}`, "false"},
		{"suffix disabled suppresses summary", "gemini-cli", "(none)", "", "absent"},
		{"no budget preserves absence", "gemini-cli", "", "", "absent"},
		{"native antigravity preserves absence", "antigravity", "(8192)", "", "absent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The translator must preserve the signed history while the canonical
			// thinking pipeline changes only generation controls.
			payload := []byte(`{"request":{"contents":[{"role":"model","parts":[{"text":"private thought","thought":true,"thoughtSignature":"synthetic-signature"}]}]` + tc.thinking + `}}`)
			format := sdktranslator.FromString(tc.from)
			req := cliproxyexecutor.Request{Model: model + tc.suffix, Payload: payload}
			opts := cliproxyexecutor.Options{SourceFormat: format, OriginalRequest: payload}
			translated := sdktranslator.TranslateRequest(format, sdktranslator.FromString("antigravity"), model, payload, true)
			out, err := helps.ApplyRequestThinking(translated, req, opts, tc.from, "antigravity", "antigravity")
			if err != nil {
				t.Fatal(err)
			}
			summary := gjson.GetBytes(out, "request.generationConfig.thinkingConfig.includeThoughts")
			if tc.want == "absent" {
				if summary.Exists() {
					t.Fatalf("unexpected summary: %s", out)
				}
			} else if !summary.Exists() || summary.String() != tc.want {
				t.Fatalf("summary=%s want=%s body=%s", summary.Raw, tc.want, out)
			}
			if gjson.GetBytes(out, "request.contents.0.parts.0.thoughtSignature").String() != "synthetic-signature" || !gjson.GetBytes(out, "request.contents.0.parts.0.thought").Bool() {
				t.Fatalf("signed history changed: %s", out)
			}
		})
	}
}
