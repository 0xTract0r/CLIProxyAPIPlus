package responses

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/MoeForge/moego/ojson"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestOrdinaryClaudeResponsesPreservesInitialText(t *testing.T) {
	type textBlock struct{ start, delta string }
	for _, tc := range []struct {
		name   string
		blocks []textBlock
		text   string
		deltas []string
	}{
		{"start and delta", []textBlock{{"hel", "lo"}}, "hello", []string{"hel", "lo"}},
		{"adjacent blocks and repeated text", []textBlock{{"go ", "go "}, {"", "世界"}, {"!", ""}}, "go go 世界!", []string{"go ", "go ", "世界", "!"}},
		{"empty start", []textBlock{{"", ""}}, "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chunks := [][]byte{[]byte(`data: {"type":"message_start","message":{"id":"msg_ordinary_fixture","model":"claude-test","usage":{"input_tokens":2,"output_tokens":0}}}`)}
			for index, block := range tc.blocks {
				chunks = append(chunks, fmt.Appendf(nil, `data: {"type":"content_block_start","index":%d,"content_block":{"type":"text","text":%q}}`, index, block.start))
				if block.delta != "" {
					chunks = append(chunks, fmt.Appendf(nil, `data: {"type":"content_block_delta","index":%d,"delta":{"type":"text_delta","text":%q}}`, index, block.delta))
				}
				chunks = append(chunks, fmt.Appendf(nil, `data: {"type":"content_block_stop","index":%d}`, index))
			}
			chunks = append(chunks, []byte(`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`), []byte(`data: {"type":"message_stop"}`))

			t.Run("stream", func(t *testing.T) {
				var id string
				var stages, deltas []string
				var sequence int64
				for _, chunk := range translateClaudeResponsesStreamThroughRegistry(chunks) {
					event, data := parseClaudeResponsesSSEEvent(t, chunk)
					if got := data.Get("sequence_number").Int(); got <= sequence {
						t.Fatalf("sequence_number=%d after %d", got, sequence)
					} else {
						sequence = got
					}
					switch event {
					case "response.output_item.added":
						id = data.Get("item.id").String()
						if id == "" || data.Get("output_index").Int() != 0 {
							t.Fatalf("invalid opened item: %s", data.Raw)
						}
					case "response.content_part.added", "response.output_text.delta", "response.output_text.done", "response.content_part.done":
						if id == "" || data.Get("item_id").String() != id || !data.Get("output_index").Exists() || data.Get("output_index").Int() != 0 || !data.Get("content_index").Exists() || data.Get("content_index").Int() != 0 {
							t.Fatalf("event %s changed item identity or index: %s", event, data.Raw)
						}
						switch event {
						case "response.content_part.added":
							if got := data.Get("part.text").String(); got != "" {
								t.Fatalf("added part text = %q, want empty; deltas carry the content", got)
							}
						case "response.output_text.delta":
							deltas = append(deltas, data.Get("delta").String())
						case "response.output_text.done":
							if got := data.Get("text").String(); got != tc.text {
								t.Errorf("done text=%q, want %q", got, tc.text)
							}
						case "response.content_part.done":
							if got := data.Get("part.text").String(); got != tc.text {
								t.Errorf("done part text=%q, want %q", got, tc.text)
							}
						}
					case "response.output_item.done":
						if data.Get("item.id").String() != id || data.Get("output_index").Int() != 0 || data.Get("item.content.0.text").String() != tc.text {
							t.Errorf("done item changed identity, index, or text: %s", data.Raw)
						}
					case "response.completed":
						assertOrdinaryResponsesOutput(t, data.Get("response"), id, tc.text)
					case "response.created", "response.in_progress":
						continue
					default:
						t.Fatalf("unexpected event for ordinary text: %s", event)
					}
					stages = append(stages, event)
				}
				wantStages := []string{"response.output_item.added", "response.content_part.added"}
				for range tc.deltas {
					wantStages = append(wantStages, "response.output_text.delta")
				}
				wantStages = append(wantStages, "response.output_text.done", "response.content_part.done", "response.output_item.done", "response.completed")
				if !slices.Equal(stages, wantStages) || !slices.Equal(deltas, tc.deltas) {
					t.Fatalf("stages=%v deltas=%q, want stages=%v deltas=%q", stages, deltas, wantStages, tc.deltas)
				}
			})

			t.Run("buffered", func(t *testing.T) {
				var state any
				payload := sdktranslator.TranslateNonStream(context.Background(), sdktranslator.FormatClaude, sdktranslator.FormatOpenAIResponse, "claude-test", nil, nil, bytes.Join(chunks, []byte("\n\n")), &state)
				assertOrdinaryResponsesOutput(t, ojson.ParseBytes(payload), "msg_msg_ordinary_fixture_0", tc.text)
			})
		})
	}
}

func assertOrdinaryResponsesOutput(t *testing.T, response ojson.Result, id, text string) {
	t.Helper()
	item := response.Get("output.0")
	if id == "" || response.Get("status").String() != "completed" || len(response.Get("output").Array()) != 1 || item.Get("id").String() != id || len(item.Get("content").Array()) != 1 {
		t.Fatalf("invalid ordinary Responses object: %s", response.Raw)
	}
	if got := item.Get("content.0.text").String(); got != text {
		t.Errorf("completed text=%q, want %q", got, text)
	}
	if response.Get("usage.input_tokens").Int() != 2 || response.Get("usage.output_tokens").Int() != 3 {
		t.Errorf("unexpected usage: %s", response.Get("usage").Raw)
	}
}

func TestOrdinaryClaudeResponsesIgnoresNonStringInitialText(t *testing.T) {
	for _, value := range []struct{ name, json string }{
		{"number", "17"},
		{"boolean", "true"},
		{"object", `{"label":"ignored"}`},
		{"array", `["ignored"]`},
		{"null", "null"},
	} {
		t.Run(value.name, func(t *testing.T) {
			chunks := [][]byte{
				[]byte(`data: {"type":"message_start","message":{"id":"msg_type_guard","model":"claude-test"}}`),
				fmt.Appendf(nil, `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":%s}}`, value.json),
				[]byte(`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"left"}}`),
				[]byte(`data: {"type":"content_block_stop","index":0}`),
				[]byte(`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"call_guard","name":"lookup","input":{}}}`),
				[]byte(`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"q\":\"Paris\"}"}}`),
				[]byte(`data: {"type":"content_block_stop","index":1}`),
				fmt.Appendf(nil, `data: {"type":"content_block_start","index":2,"content_block":{"type":"text","text":%s}}`, value.json),
				[]byte(`data: {"type":"content_block_delta","index":2,"delta":{"type":"text_delta","text":"right"}}`),
				[]byte(`data: {"type":"content_block_stop","index":2}`),
				[]byte(`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}`),
				[]byte(`data: {"type":"message_stop"}`),
			}
			wantIDs := [...]string{"msg_msg_type_guard_0", "fc_call_guard", "msg_msg_type_guard_1"}
			assertOutput := func(t *testing.T, response ojson.Result) {
				t.Helper()
				items := response.Get("output").Array()
				if response.Get("status").String() != "completed" || len(items) != len(wantIDs) {
					t.Fatalf("unexpected terminal response: %s", response.Raw)
				}
				for i, id := range wantIDs {
					if got := items[i].Get("id").String(); got != id {
						t.Errorf("output index %d id=%q, want %q", i, got, id)
					}
				}
				if left, right := items[0].Get("content.0.text").String(), items[2].Get("content.0.text").String(); left != "left" || right != "right" {
					t.Errorf("non-string start polluted normal text: %q / %q", left, right)
				}
				tool := items[1]
				if tool.Get("type").String() != "function_call" || tool.Get("call_id").String() != "call_guard" || tool.Get("name").String() != "lookup" || tool.Get("arguments").String() != `{"q":"Paris"}` {
					t.Errorf("tool identity or arguments changed: %s", tool.Raw)
				}
			}
			t.Run("stream", func(t *testing.T) {
				var deltas []string
				var finished [3]int
				var terminal int
				for _, chunk := range translateClaudeResponsesStreamThroughRegistry(chunks) {
					event, data := parseClaudeResponsesSSEEvent(t, chunk)
					index := int(data.Get("output_index").Int())
					switch event {
					case "response.output_text.delta":
						if (index != 0 && index != 2) || data.Get("item_id").String() != wantIDs[index] || data.Get("content_index").Int() != 0 {
							t.Fatalf("delta identity changed: %s", data.Raw)
						}
						deltas = append(deltas, data.Get("delta").String())
					case "response.output_item.done":
						if index < 0 || index >= len(wantIDs) || data.Get("item.id").String() != wantIDs[index] {
							t.Fatalf("done item identity changed: %s", data.Raw)
						}
						finished[index]++
					case "response.completed":
						terminal++
						assertOutput(t, data.Get("response"))
					}
				}
				if !slices.Equal(deltas, []string{"left", "right"}) || finished != [3]int{1, 1, 1} || terminal != 1 {
					t.Errorf("unexpected text or completion events: deltas=%q finished=%v completed=%d", deltas, finished, terminal)
				}
			})
			t.Run("buffered", func(t *testing.T) {
				var state any
				payload := sdktranslator.TranslateNonStream(context.Background(), sdktranslator.FormatClaude, sdktranslator.FormatOpenAIResponse, "claude-test", nil, nil, bytes.Join(chunks, []byte("\n\n")), &state)
				assertOutput(t, ojson.ParseBytes(payload))
			})
		})
	}
}
