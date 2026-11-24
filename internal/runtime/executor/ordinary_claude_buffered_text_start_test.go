package executor

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
)

func TestOrdinaryClaudeBufferedChatTextStarts(t *testing.T) {
	type textEvent struct {
		kind  string
		index int
		text  string
	}
	for _, tc := range []struct {
		name   string
		events []textEvent
		want   string
	}{
		{
			name: "start_only",
			events: []textEvent{
				{"start", 0, "  hello 世界  "},
				{"stop", 0, ""},
			},
			want: "  hello 世界  ",
		},
		{
			name: "start_and_delta",
			events: []textEvent{
				{"start", 0, "hel"},
				{"delta", 0, "lo"},
				{"stop", 0, ""},
			},
			want: "hello",
		},
		{
			name: "empty_start_and_delta",
			events: []textEvent{
				{"start", 0, ""},
				{"delta", 0, "hello"},
				{"stop", 0, ""},
			},
			want: "hello",
		},
		{
			name: "indexed_blocks_interleave",
			events: []textEvent{
				{"start", 0, ""},
				{"start", 1, "right:"},
				{"delta", 1, "B"},
				{"delta", 0, "left:A "},
				{"stop", 0, ""},
				{"stop", 1, ""},
			},
			want: "left:A right:B",
		},
		{
			name: "identical_start_and_delta_are_both_content",
			events: []textEvent{
				{"start", 0, "go "},
				{"delta", 0, "go "},
				{"stop", 0, ""},
			},
			want: "go go ",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body strings.Builder
			body.WriteString("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_ordinary\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-3-5-sonnet-20241022\",\"usage\":{\"input_tokens\":2,\"output_tokens\":0}}}\n\n")
			for _, event := range tc.events {
				eventType := "content_block_" + event.kind
				text, err := json.Marshal(event.text)
				if err != nil {
					t.Fatal(err)
				}
				var fields string
				switch event.kind {
				case "start":
					fields = fmt.Sprintf(`,"content_block":{"type":"text","text":%s}`, text)
				case "delta":
					fields = fmt.Sprintf(`,"delta":{"type":"text_delta","text":%s}`, text)
				}
				fmt.Fprintf(&body, "event: %s\ndata: {\"type\":\"%s\",\"index\":%d%s}\n\n", eventType, eventType, event.index, fields)
			}
			body.WriteString("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
			resp, err := executeOpenAIChatCompletionThroughClaude(t, body.String())
			if err != nil {
				t.Fatal(err)
			}
			if got := ojson.GetBytes(resp.Payload, "object").String(); got != "chat.completion" {
				t.Fatalf("response object = %q, want chat.completion", got)
			}
			if got := ojson.GetBytes(resp.Payload, "choices.0.message.content").String(); got != tc.want {
				t.Fatalf("response content = %q, want %q", got, tc.want)
			}
		})
	}
}
