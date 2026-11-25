package helps

import (
	"testing"

	"github.com/MoeForge/moego/ojson"
	x "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	tr "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestCopilotParallelParentsAcrossRoutes(t *testing.T) {
	links := NewCopilotTurnLinks()
	opts := x.Options{Metadata: map[string]any{x.CanonicalSessionIDMetadataKey: "session"}}
	parent := func(resp string, calls string) CopilotTurn {
		turn := links.Resolve("account", CopilotTurnInput{Initiator: "user"}, opts, tr.FormatOpenAIResponse, ojson.Result{})
		links.Observe(turn, tr.FormatOpenAIResponse, ojson.Parse(`{"id":"`+resp+`","output":[`+calls+`]}`))
		return turn
	}
	one := parent("resp1", `{"call_id":"a"},{"call_id":"b"}`)
	two := parent("resp2", `{"call_id":"c"}`)
	batch := func(f tr.Format, ids []string, prev string) string {
		d := ojson.NewDocString(`{}`)
		var results []map[string]string
		for _, id := range ids {
			switch f {
			case tr.FormatOpenAI:
				results = append(results, map[string]string{"role": "tool", "tool_call_id": id})
			case tr.FormatOpenAIResponse:
				results = append(results, map[string]string{"type": "function_call_output", "call_id": id})
			case tr.FormatClaude:
				results = append(results, map[string]string{"type": "tool_result", "tool_use_id": id})
			}
		}
		switch f {
		case tr.FormatOpenAIResponse:
			_ = d.Set("input", results)
			if prev != "" {
				_ = d.Set("previous_response_id", prev)
			}
		case tr.FormatClaude:
			_ = d.Set("messages", []any{map[string]any{"role": "user", "content": results}})
		default:
			_ = d.Set("messages", results)
		}
		return d.String()
	}
	formats := []tr.Format{tr.FormatOpenAI, tr.FormatClaude, tr.FormatOpenAIResponse}
	for _, from := range formats {
		for _, to := range formats {
			for _, tc := range []struct {
				ids   []string
				prev  string
				reuse bool
			}{{[]string{"b", "a"}, "", true}, {[]string{"a", "c"}, "", false}, {[]string{"c", "a"}, "", false}, {[]string{"unknown", "b"}, "", false}, {[]string{"b", "b"}, "", false}, {[]string{""}, "", false}, {[]string{"c"}, "resp1", false}, {[]string{"unknown", "a"}, "resp1", true}} {
				if tc.prev != "" && to != tr.FormatOpenAIResponse {
					continue
				}
				src := batch(from, tc.ids, tc.prev)
				input := ParseCopilotTurnInput([]byte(src), from)
				got := links.Resolve("account", input, opts, to, ojson.Parse(batch(to, tc.ids, tc.prev)))
				if (got == one) != tc.reuse || got == two {
					t.Errorf("%s -> %s ids=%v previous=%s reused unexpected parent", from, to, tc.ids, tc.prev)
				}
			}
		}
	}
}

func TestCopilotRecycledChatIDsRequireCompletePairs(t *testing.T) {
	const assistant = `{"role":"assistant","tool_calls":[{"id":"tool:0","function":{"name":"sample","arguments":"{}"}}]}`
	const result = `{"role":"tool","tool_call_id":"tool:0","content":"ok"}`
	const user = `{"role":"user","content":"again"}`
	for _, tc := range []struct {
		tail string
		want bool
	}{{assistant + "," + result, true}, {assistant, false}, {assistant + "," + user + "," + result, false}, {assistant + "," + result + "," + result, false}} {
		doc := ojson.NewDocString(`{"messages":[` + assistant + "," + result + "," + user + "," + tc.tail + "]}")
		input := ParseCopilotTurnInput(doc.Bytes(), tr.FormatOpenAI)
		input.UniqueChatToolIDs(doc, false)
		if (doc.Get("messages.3.tool_calls.0.id").String() == "tool:1") != tc.want {
			t.Fatal("incorrect call ID rewrite")
		}
		if tc.want {
			if doc.Get("messages.4.tool_call_id").String() != "tool:1" {
				t.Fatal("unpaired result")
			}
			ids, ok := input.wireCalls(tr.FormatOpenAI, doc.Get("messages"))
			if !ok || len(ids) != 1 || ids[0] != "tool:0" {
				t.Fatal("lost issued ID for parent lookup")
			}
			_ = doc.Set("messages.4.tool_call_id", "tool:0")
			if _, ok = input.wireCalls(tr.FormatOpenAI, doc.Get("messages")); ok {
				t.Fatal("accepted substituted result")
			}
		}
	}
}

func TestCopilotChatKeepsExplicitCurrentRequestContent(t *testing.T) {
	doc := ojson.NewDocString(`{"messages":[
		{"role":"assistant","reasoning_text":"visible trace","reasoning_opaque":"caller-opaque","tool_calls":[
			{"id":"a","function":{"name":"look","arguments":"{}"},"extra_content":{"google":{"thought_signature":"caller-opaque-tool"}}},
			{"id":"b","function":{"name":"read","arguments":"{}"}}
		]},
		{"role":"tool","tool_call_id":"a","content":"first result"},
		{"role":"tool","tool_call_id":"b","content":"second result"}
	]}`)
	var input CopilotTurnInput
	input.UniqueChatToolIDs(doc, false)
	for path, want := range map[string]string{
		"messages.0.reasoning_text":                                      "visible trace",
		"messages.0.reasoning_opaque":                                    "caller-opaque",
		"messages.0.tool_calls.0.extra_content.google.thought_signature": "caller-opaque-tool",
		"messages.0.tool_calls.1.function.name":                          "read",
		"messages.1.content":                                             "first result",
		"messages.2.content":                                             "second result",
	} {
		if got := doc.Get(path).String(); got != want {
			t.Errorf("%s = %q, want %q", path, got, want)
		}
	}
	if doc.Get("messages.0.tool_calls.1.extra_content").Exists() {
		t.Fatal("unsigned sibling received extra content")
	}
	for i, path := range []string{"messages.1.tool_call_id", "messages.2.tool_call_id"} {
		callPath := []string{"messages.0.tool_calls.0.id", "messages.0.tool_calls.1.id"}[i]
		if got, want := doc.Get(path).String(), doc.Get(callPath).String(); got == "" || got != want {
			t.Errorf("%s does not match its explicit tool call", path)
		}
	}
}
