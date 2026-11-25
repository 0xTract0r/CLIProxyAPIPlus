package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/MoeForge/moego/json"
	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	_ "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator/builtin"
)

func TestIssue7000AntigravityClaudeStructuredOutputUsesPromptAndAutoTools(t *testing.T) {
	wantSchema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"title":       map[string]any{"type": "string"},
			"description": map[string]any{"type": "string"},
		},
		"required":             []any{"title", "description"},
		"additionalProperties": false,
	}

	for _, model := range []string{"claude-opus-5-5-high", "claude-sonnet-5-5-high"} {
		for _, tc := range []struct {
			name    string
			format  sdktranslator.Format
			payload string
			tools   string
		}{
			{
				name:   "responses",
				format: sdktranslator.FormatOpenAIResponse,
				payload: `{
					"input":[{"role":"user","content":[{"type":"input_text","text":"Provide title and description for: fix bug"}]}],
					"text":{"format":{"type":"json_schema","strict":true,"name":"codex_output_schema","schema":{
						"type":"object",
						"properties":{"title":{"type":"string"},"description":{"type":"string"}},
						"required":["title","description"],
						"additionalProperties":false
					}}}
				}`,
				tools: `[{"type":"function","name":"get_time","description":"Get the current time.","parameters":{"type":"object","properties":{"timezone":{"type":"string"}},"required":["timezone"]}}]`,
			},
			{
				name:   "chat",
				format: sdktranslator.FormatOpenAI,
				payload: `{
					"messages":[{"role":"user","content":"Provide title and description for: fix bug"}],
					"response_format":{"type":"json_schema","json_schema":{"strict":true,"name":"codex_output_schema","schema":{
						"type":"object",
						"properties":{"title":{"type":"string"},"description":{"type":"string"}},
						"required":["title","description"],
						"additionalProperties":false
					}}}
				}`,
				tools: `[{"type":"function","function":{"name":"get_time","description":"Get the current time.","parameters":{"type":"object","properties":{"timezone":{"type":"string"}},"required":["timezone"]}}}]`,
			},
		} {
			for _, withTools := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/tools=%t", model, tc.name, withTools), func(t *testing.T) {
					payloadDoc := ojson.NewDoc([]byte(tc.payload))
					if errSet := payloadDoc.Set("model", model); errSet != nil {
						t.Fatalf("set model: %v", errSet)
					}
					if withTools {
						if errSet := payloadDoc.SetRawBytes("tools", []byte(tc.tools)); errSet != nil {
							t.Fatalf("set tools: %v", errSet)
						}
						if errSet := payloadDoc.Set("tool_choice", "auto"); errSet != nil {
							t.Fatalf("set tool_choice: %v", errSet)
						}
					}
					payload := payloadDoc.Bytes()

					encoded := captureOrdinaryStructuredRequest(t, model, payload, tc.format)
					request := ojson.GetBytes(encoded, "request")

					if mode := request.Get("toolConfig.functionCallingConfig.mode").String(); mode != "" && mode != "AUTO" {
						t.Errorf("outbound function calling mode = %q, want AUTO or omitted; Claude 5.5 rejects forced tool use", mode)
					}
					for _, field := range []string{"responseSchema", "responseJsonSchema", "responseMimeType"} {
						if got := request.Get("generationConfig." + field); got.Exists() {
							t.Errorf("outbound generationConfig.%s = %s, want omitted for prompt-based structured output", field, got.Raw)
						}
					}

					var systemText strings.Builder
					for _, part := range request.Get("systemInstruction.parts").Array() {
						systemText.WriteString(part.Get("text").String())
						systemText.WriteByte('\n')
					}
					foundSchema := false
					prompt := systemText.String()
					// Decode embedded JSON without depending on instruction wording or schema key order.
					for offset, char := range prompt {
						if char != '{' {
							continue
						}
						var schema map[string]any
						if errDecode := json.NewDecoder(strings.NewReader(prompt[offset:])).Decode(&schema); errDecode == nil && reflect.DeepEqual(schema, wantSchema) {
							foundSchema = true
							break
						}
					}
					if !foundSchema {
						t.Errorf("system instruction must preserve the title/description schema, required fields, and additionalProperties:false; got %q", prompt)
					}

					if withTools {
						declarations := request.Get("tools.0.functionDeclarations").Array()
						if len(declarations) != 1 {
							t.Fatalf("outbound function declaration count = %d, want 1 real tool; tools = %s", len(declarations), request.Get("tools").Raw)
						}
						declaration := declarations[0]
						if declaration.Get("name").String() != "get_time" || declaration.Get("description").String() != "Get the current time." {
							t.Errorf("ordinary tool name or description changed: %s", declaration.Raw)
						}
						// The active translator uses parametersJsonSchema for OpenAI's
						// JSON Schema. Retain the original schema assertions in that field.
						schema := declaration.Get("parametersJsonSchema")
						if !schema.Exists() {
							schema = declaration.Get("parameters")
						}
						for path, want := range map[string]string{
							"type": "object", "properties.timezone.type": "string", "required.0": "timezone",
						} {
							if got := schema.Get(path).String(); got != want {
								t.Errorf("real tool schema %s = %q, want %q", path, got, want)
							}
						}
					}
				})
			}
		}
	}
}

func captureOrdinaryStructuredRequest(t *testing.T, model string, payload []byte, source sdktranslator.Format) []byte {
	t.Helper()
	body, err := captureOrdinaryStructuredRequestWithOriginal(t, model, payload, nil, source)
	if err != nil || len(body) == 0 {
		t.Fatalf("official request could not be captured: %v", err)
	}
	return body
}

func captureOrdinaryStructuredRequestWithOriginal(t *testing.T, model string, payload, original []byte, source sdktranslator.Format) ([]byte, error) {
	t.Helper()
	var body []byte
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", ordinaryAntigravityRT(func(req *http.Request) (*http.Response, error) {
		var err error
		body, err = io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		response := `{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"fixture"}]},"finishReason":"STOP"}]}}`
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(strings.NewReader(response)), Request: req}, nil
	}))
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": "https://fixture.invalid"},
		Metadata: map[string]any{"access_token": "test-access", "project_id": "test-project"}}
	opts := cliproxyexecutor.Options{SourceFormat: source, ResponseFormat: sdktranslator.FormatAntigravity, OriginalRequest: original}
	_, err := NewAntigravityExecutor(&config.Config{}).Execute(ctx, auth, cliproxyexecutor.Request{Model: model, Payload: payload}, opts)
	return body, err
}
