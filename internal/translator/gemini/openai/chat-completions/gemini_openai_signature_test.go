package chat_completions

import (
	"testing"

	"github.com/MoeForge/moego/ojson"
)

const capturedGeminiToolCallThoughtSignature = "EjQKMgEMOdbHO0Gd+c9Mxk4ELwPGbpCEcp2mFfYYLix2UVtBH3fL8GECc4+JITVnHF4qZDsA"

func TestOrdinaryOpenAIToolCallsKeepArgumentsAndResults(t *testing.T) {
	const input = `{"messages":[
	  {"role":"assistant","tool_calls":[
	    {"id":"call-a","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"Paris\"}"}},
	    {"id":"call-b","type":"function","function":{"name":"weather","arguments":"{\"days\":2}"}}
	  ]},
	  {"role":"tool","tool_call_id":"call-b","content":"result-b"},
	  {"role":"tool","tool_call_id":"call-a","content":"result-a"}
	]}`
	for _, tc := range []struct {
		name     string
		explicit bool
	}{
		{"missing", false},
		{"explicit", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := ojson.NewDocString(input)
			const opaque = "caller-supplied-opaque-fixture"
			if tc.explicit {
				_ = request.Set("messages.0.tool_calls.0.extra_content.google.thought_signature", opaque)
			}
			output, err := ConvertOpenAIRequestToGemini("gemini-3.5-flash", request.Bytes(), false)
			if err != nil {
				t.Fatal(err)
			}
			calls := ojson.GetBytes(output, "contents.0.parts").Array()
			results := ojson.GetBytes(output, "contents.1.parts").Array()
			if len(calls) != 2 || len(results) != 2 {
				t.Fatal("translation dropped a tool call or tool result")
			}
			if calls[0].Get("functionCall.name").String() != "lookup" ||
				calls[0].Get("functionCall.args.q").String() != "Paris" ||
				calls[1].Get("functionCall.name").String() != "weather" ||
				calls[1].Get("functionCall.args.days").Int() != 2 {
				t.Fatal("translation changed tool names, order, or arguments")
			}
			if field := calls[0].Get("thoughtSignature"); field.Exists() != tc.explicit ||
				(tc.explicit && field.String() != opaque) || calls[1].Get("thoughtSignature").Exists() {
				t.Fatal("tool metadata changed, was added for missing input, or moved to another call")
			}
			if results[0].Get("functionResponse.name").String() != "lookup" ||
				results[0].Get("functionResponse.response.result").String() != `"result-a"` ||
				results[1].Get("functionResponse.name").String() != "weather" ||
				results[1].Get("functionResponse.response.result").String() != `"result-b"` {
				t.Fatal("tool results did not stay associated with their own calls")
			}
		})
	}
}
