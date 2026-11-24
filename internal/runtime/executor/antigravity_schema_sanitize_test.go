package executor

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/MoeForge/moego/ojson"
)

func TestAntigravityOrdinarySchemaKeepsSupportedDeclarationsAndHistory(t *testing.T) {
	const payload = `{"request":{"contents":[{"role":"user","parts":[{"text":"plain propertyNames text"}]},{"role":"model","parts":[{"functionCall":{"name":"find","id":"call","args":{"propertyNames":"ordinary data"}}}]},{"role":"user","parts":[{"functionResponse":{"name":"find","id":"call","response":{"propertyNames":"ordinary result"}}}]}],"tools":[{"functionDeclarations":[{"name":"find","parametersJsonSchema":{"type":"object","properties":{"filter":{"anyOf":[{"type":"string"},{"type":"null"}]},"extra":true},"required":["filter"],"additionalProperties":false}}]}],"generationConfig":{"responseJsonSchema":{"type":"object","properties":{"items":{"type":"array","items":{"type":"string"}}},"required":["items"],"additionalProperties":false}}}}`
	wire := captureOrdinaryNativeAntigravityRequest(t, "gemini-3.7-flash", payload)
	for _, path := range []string{
		"request.tools.0.functionDeclarations.0.parametersJsonSchema",
		"request.generationConfig.responseJsonSchema",
		"request.contents.0.parts.0.text",
		"request.contents.1.parts.0.functionCall.args",
		"request.contents.2.parts.0.functionResponse.response",
	} {
		var before, after any
		expected := ojson.GetBytesNoCopy([]byte(payload), path).Raw
		actual := ojson.GetBytesNoCopy(wire, path).Raw
		if json.Unmarshal([]byte(expected), &before) != nil || json.Unmarshal([]byte(actual), &after) != nil || !reflect.DeepEqual(before, after) {
			t.Errorf("ordinary schema or history changed at %s", path)
		}
	}
}
