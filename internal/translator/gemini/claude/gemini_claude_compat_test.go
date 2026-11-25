package claude

import (
	"testing"

	"github.com/MoeForge/moego/ojson"
)

const capturedGeminiThinkingSignature = "EjQKMgEMOdbHO0Gd+c9Mxk4ELwPGbpCEcp2mFfYYLix2UVtBH3fL8GECc4+JITVnHF4qZDsA"

func TestOrdinaryGeminiClaudeKeepsUnsignedThinkingAndTool(t *testing.T) {
	raw := []byte(`{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"ordinary thought","signature":""},{"type":"tool_use","id":"fixture","name":"lookup","input":{"key":"value"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"fixture","content":"fixture result"}]}]}`)
	for _, tc := range []struct {
		name         string
		convert      func(string, []byte, bool) ([]byte, error)
		keepThinking bool
	}{
		{"default", ConvertClaudeRequestToGemini, false},
		{"compat", ConvertClaudeRequestToGeminiWithCompat, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := tc.convert("deepseek-v4", raw, false)
			if err != nil {
				t.Fatal(err)
			}
			parts := ojson.GetBytes(out, "contents.0.parts").Array()
			if tc.keepThinking {
				if len(parts) != 2 || !parts[0].Get("thought").Bool() || parts[0].Get("text").String() != "ordinary thought" || parts[0].Get("thoughtSignature").Exists() {
					t.Fatal("compat conversion changed the explicit unsigned thought")
				}
				parts = parts[1:]
			}
			if len(parts) != 1 || parts[0].Get("functionCall.name").String() != "lookup" || parts[0].Get("functionCall.args.key").String() != "value" || parts[0].Get("thoughtSignature").Exists() {
				t.Fatal("conversion changed the ordinary tool call or added an unprovided signature")
			}
			result := ojson.GetBytes(out, "contents.1.parts.0.functionResponse")
			if result.Get("name").String() != "lookup" || result.Get("response.result").String() != "fixture result" {
				t.Fatal("conversion lost the matching tool result")
			}
		})
	}
}
