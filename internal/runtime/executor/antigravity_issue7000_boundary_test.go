package executor

import (
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/MoeForge/moego/json"
	"github.com/MoeForge/moego/ojson"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

const issue7000LiteralSchema = `{"type":"object","properties":{"anthropic":{"type":"string","enum":["anthropic","json_schema"],"const":"anthropic"},"response_schema":{"$ref":"#/$defs/anthropic"}},"$defs":{"anthropic":{"type":"string"}},"required":["anthropic"],"additionalProperties":false}`

func assertIssue7000PromptSchema(t *testing.T, instruction ojson.Result, schemaJSON string) {
	t.Helper()
	var prompt strings.Builder
	for _, part := range instruction.Get("parts").Array() {
		prompt.WriteString(part.Get("text").String())
		prompt.WriteByte('\n')
	}
	var want any
	if errUnmarshal := json.Unmarshal([]byte(schemaJSON), &want); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	for offset, char := range prompt.String() {
		if char != '{' {
			continue
		}
		var got any
		if errDecode := json.NewDecoder(strings.NewReader(prompt.String()[offset:])).Decode(&got); errDecode == nil && reflect.DeepEqual(got, want) {
			return
		}
	}
	t.Errorf("system instruction must contain the complete unmodified schema %s; got %q", schemaJSON, prompt.String())
}

func TestIssue7000AntigravityAliasesPreserveOtherFieldsAndToolChoice(t *testing.T) {
	for _, container := range []string{"generationConfig", "generation_config"} {
		for index, schemaKey := range []string{"responseSchema", "responseJsonSchema", "response_schema", "response_json_schema"} {
			for _, mode := range []string{"AUTO", "NONE", "ANY", "VALIDATED"} {
				t.Run(container+"/"+schemaKey+"/"+mode, func(t *testing.T) {
					payloadDoc := ojson.NewDoc([]byte(`{"request":{"generationConfig":{"temperature":0.25},"generation_config":{"stopSequences":["STOP"]},"systemInstruction":{"role":"system","parts":[{"text":"keep system"}]},"contents":[{"role":"user","parts":[{"text":"keep history"}]}],"tools":[{"functionDeclarations":[{"name":"lookup","parameters":{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}}]}]}}`))
					setRaw := func(path, value string) { _ = payloadDoc.SetRawBytes(path, []byte(value)) }
					setRaw("request."+container+"."+schemaKey, issue7000LiteralSchema)
					setRaw("request."+container+".responseMimeType", `"application/json"`)
					setRaw("request."+container+".response_mime_type", `"application/json"`)
					toolContainer := []string{"toolConfig", "tool_config"}[index%2]
					callingKey := []string{"functionCallingConfig", "function_calling_config"}[index/2]
					toolPath := "request." + toolContainer + "." + callingKey
					setRaw(toolPath, fmt.Sprintf(`{"mode":%q,"allowedFunctionNames":["lookup"],"allowed_function_names":["lookup"],"streamFunctionCallArguments":true}`, mode))
					systemKey := "systemInstruction"
					if container == "generation_config" {
						systemKey = "system_instruction"
						setRaw("request."+systemKey, payloadDoc.Get("request.systemInstruction").Raw)
						_ = payloadDoc.Delete("request.systemInstruction")
					}
					payload := append([]byte(nil), payloadDoc.Bytes()...)
					var encoded []byte
					if mode == "ANY" || mode == "VALIDATED" {
						body, err := captureOrdinaryStructuredRequestWithOriginal(t, "claude-opus-5-5-high", payload, nil, sdktranslator.FormatAntigravity)
						var status statusErr
						if len(body) != 0 || !errors.As(err, &status) || status.StatusCode() != http.StatusBadRequest || !strings.Contains(status.Error(), "required tool choice") {
							t.Fatalf("explicit mode %s: request was sent or unsupported combination was unclear: %v", mode, err)
						}
						// Only the effective request carries this adapter-generated mode.
						_ = payloadDoc.Delete(toolPath)
						encoded, err = captureOrdinaryStructuredRequestWithOriginal(t, "claude-opus-5-5-high", payload, payloadDoc.Bytes(), sdktranslator.FormatAntigravity)
						if err != nil || len(encoded) == 0 {
							t.Fatalf("adapter-generated mode was rejected: %v", err)
						}
					} else {
						encoded = captureOrdinaryStructuredRequest(t, "claude-opus-5-5-high", payload, sdktranslator.FormatAntigravity)
					}
					request := ojson.GetBytes(encoded, "request")
					assertIssue7000PromptSchema(t, request.Get(systemKey), issue7000LiteralSchema)
					for path, want := range map[string]string{
						"generationConfig.temperature": "0.25", "generation_config.stopSequences.0": "STOP",
						systemKey + ".parts.0.text": "keep system", "contents.0.parts.0.text": "keep history",
						"tools.0.functionDeclarations.0.name": "lookup",
					} {
						if got := request.Get(path).String(); got != want {
							t.Errorf("%s = %q, want %q", path, got, want)
						}
					}
					for _, configKey := range []string{"generationConfig", "generation_config"} {
						for _, key := range []string{"responseSchema", "responseJsonSchema", "response_schema", "response_json_schema", "responseMimeType", "response_mime_type"} {
							if request.Get(configKey + "." + key).Exists() {
								t.Errorf("native output trigger survived: %s.%s", configKey, key)
							}
						}
					}
					wantMode := mode
					if mode == "ANY" || mode == "VALIDATED" {
						wantMode = "AUTO"
						for _, key := range []string{"allowedFunctionNames", "allowed_function_names"} {
							if ojson.GetBytes(encoded, toolPath+"."+key).Exists() {
								t.Errorf("forced-tool allowlist survived AUTO downgrade: %s", key)
							}
						}
					}
					if got := ojson.GetBytes(encoded, toolPath+".mode").String(); got != wantMode {
						t.Errorf("tool choice = %q, want %q", got, wantMode)
					}
					if !ojson.GetBytes(encoded, toolPath+".streamFunctionCallArguments").Bool() {
						t.Error("unrelated tool configuration was removed")
					}
					if toolContainer == "tool_config" && request.Get("toolConfig").Exists() {
						t.Error("compatibility conversion introduced a conflicting camelCase tool config")
					}
				})
			}
		}
	}
}

func TestIssue7000AntigravityModelAndPlainRequestBoundaries(t *testing.T) {
	for _, tc := range []struct {
		model   string
		convert bool
	}{
		{"claude-opus-5-5", true}, {"claude-sonnet-5-5-high", true},
		{"claude-opus-5-5-low", true}, {"claude-opus-5-5-medium", true}, {"claude-opus-5-5-max", true},
		{"claude-sonnet-5-5-low", true}, {"claude-sonnet-5-5-medium", true}, {"claude-sonnet-5-5-max", true},
		{"claude-opus-5-5-reasoning", true}, {"claude-sonnet-5-5(high)", true},
		{"claude-opus-5-5-20261001", true}, {"claude-sonnet-5-5[1m]", true},
		{"claude-opus-5-5-20261001[1m](high)", true}, {"claude-opus-5-5-max-20261001[1m](max)", true},
		{"claude-opus-5-5-custom", false},
		{"claude-opus-5-50-high", false}, {"claude-sonnet-5-50", false},
		{"claude-opus-4-6", false}, {"claude-haiku-5-5", false},
		{"gemini-3.8-flash-high", false}, {"other/claude-opus-5-5-high", false},
	} {
		t.Run(tc.model, func(t *testing.T) {
			payload := []byte(`{"request":{"generationConfig":{"responseSchema":{"type":"object","properties":{"title":{"type":"string"}}},"responseMimeType":"application/json"}}}`)
			encoded := captureOrdinaryStructuredRequest(t, tc.model, payload, sdktranslator.FormatAntigravity)
			if got := ojson.GetBytes(encoded, "request.generationConfig.responseSchema").Exists(); got == tc.convert {
				t.Errorf("native schema presence = %t, conversion = %t", got, tc.convert)
			}
			if tc.convert {
				assertIssue7000PromptSchema(t, ojson.GetBytes(encoded, "request.systemInstruction"), `{"type":"object","properties":{"title":{"type":"string"}}}`)
				if mode := ojson.GetBytes(encoded, "request.toolConfig.functionCallingConfig.mode").String(); mode != "" && mode != "AUTO" {
					t.Errorf("functionCallingConfig.mode = %q, want AUTO or omitted when converted", mode)
				}
			}
		})
	}
	for _, configJSON := range []string{`{"temperature":0.4}`, `{"responseMimeType":"text/plain"}`} {
		t.Run("plain/"+configJSON, func(t *testing.T) {
			payload := []byte(`{"request":{"generationConfig":` + configJSON + `,"systemInstruction":{"parts":[{"text":"unchanged"}]},"toolConfig":{"functionCallingConfig":{"mode":"ANY","allowedFunctionNames":["lookup"]}}}}`)
			encoded := captureOrdinaryStructuredRequest(t, "claude-opus-5-5-high", payload, sdktranslator.FormatAntigravity)
			if got := ojson.GetBytes(encoded, "request.systemInstruction.parts.#").Int(); got != 1 {
				t.Errorf("plain request gained instructions: %s", encoded)
			}
			if got := ojson.GetBytes(encoded, "request.toolConfig.functionCallingConfig.mode").String(); got != "ANY" || ojson.GetBytes(encoded, "request.toolConfig.functionCallingConfig.allowedFunctionNames.0").String() != "lookup" {
				t.Errorf("plain request lost its explicit tool selection: %q", got)
			}
		})
	}
}

func TestIssue7000AntigravityExplicitOpenAIAndClaudeToolChoices(t *testing.T) {
	const model = "claude-opus-5-5-high"
	t.Run("OpenAI required with structured output is not downgraded", func(t *testing.T) {
		payload := []byte(`{"messages":[{"role":"user","content":"return a value"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}],"tool_choice":"required","response_format":{"type":"json_schema","json_schema":{"name":"fixture","schema":{"type":"object"}}}}`)
		body, err := captureOrdinaryStructuredRequestWithOriginal(t, model, payload, nil, sdktranslator.FormatOpenAI)
		var status statusErr
		if len(body) != 0 || !errors.As(err, &status) || status.StatusCode() != http.StatusBadRequest || !strings.Contains(status.Error(), "required tool choice") {
			t.Fatalf("explicit tool choice must not be replaced by AUTO: sent=%v error=%v", len(body) != 0, err)
		}
	})
	t.Run("Claude named tool without structured output retains the chosen name", func(t *testing.T) {
		payload := []byte(`{"max_tokens":32,"messages":[{"role":"user","content":"return a value"}],"tools":[{"name":"lookup","input_schema":{"type":"object"}}],"tool_choice":{"type":"tool","name":"lookup"}}`)
		body := captureOrdinaryStructuredRequest(t, model, payload, sdktranslator.FormatClaude)
		choice := ojson.GetBytesNoCopy(body, "request.toolConfig.functionCallingConfig")
		if choice.Get("mode").String() != "ANY" || choice.Get("allowedFunctionNames.0").String() != "lookup" {
			t.Fatalf("plain request lost its explicit named tool: %s", choice.Raw)
		}
	})
}
