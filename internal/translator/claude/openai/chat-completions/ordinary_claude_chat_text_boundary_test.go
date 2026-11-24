package chat_completions

import (
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
)

func TestOrdinaryClaudeBufferedChatTextBoundary(t *testing.T) {
	const model = "claude-fixture-a"
	t.Run("legacy_deltas_follow_arrival_order", func(t *testing.T) {
		stream := []byte(`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"A"}}

data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"B"}}

data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"C"}}

data: {"type":"message_stop"}
`)
		got := ConvertClaudeResponseToOpenAINonStream(t.Context(), model, nil, nil, stream, nil)
		if text := ojson.GetBytes(got, "choices.0.message.content").String(); text != "ABC" {
			t.Fatalf("content = %q, want ABC", text)
		}
	})

	t.Run("metadata_offsets_follow_final_utf8_block_boundaries", func(t *testing.T) {
		stream := []byte(`data: {"type":"message_start","message":{"model":"claude-fixture-a","usage":{"input_tokens":3}}}

data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":"a"}}

data: {"type":"content_block_start","index":2,"content_block":{"type":"fallback","from":{"model":"claude-fixture-a","label":"first"},"to":{"model":"claude-fixture-b"},"trigger":{"type":"service_unavailable"}}}

data: {"type":"content_block_stop","index":2}

data: {"type":"content_block_start","index":3,"content_block":{"type":"text","text":"后"}}

data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"界"}}

data: {"type":"content_block_stop","index":0}

data: {"type":"content_block_stop","index":1}

data: {"type":"content_block_stop","index":3}

data: {"type":"content_block_start","index":4,"content_block":{"type":"fallback","from":{"model":"claude-fixture-b"},"to":{"model":"claude-fixture-c","label":"last"}}}

data: {"type":"content_block_stop","index":4}

data: {"type":"content_block_start","index":5,"content_block":{"type":"text","text":"end"}}

data: {"type":"content_block_stop","index":5}

data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":4}}

data: {"type":"message_stop"}
`)
		out := ConvertClaudeResponseToOpenAINonStream(t.Context(), model, nil, nil, stream, nil)
		message := ojson.GetBytes(out, "choices.0.message")
		if text := message.Get("content").String(); text != "界a后end" {
			t.Errorf("content = %q, want 界a后end", text)
		}

		hops := message.Get("fallbacks").Array()
		if len(hops) != 2 {
			t.Fatalf("fallbacks = %s, want 2 entries", message.Get("fallbacks").Raw)
		}
		for i, wantPrefix := range []string{"界a", "界a后"} {
			offset := hops[i].Get("content_offset")
			if !offset.Exists() || offset.Int() != int64(len(wantPrefix)) {
				t.Errorf("fallback %d byte offset = %s, want %d (after %q)", i, offset.Raw, len(wantPrefix), wantPrefix)
			}
		}
		if hops[0].Get("from.label").String() != "first" || hops[0].Get("trigger.type").String() != "service_unavailable" ||
			hops[1].Get("from.model").String() != "claude-fixture-b" || hops[1].Get("to.label").String() != "last" ||
			message.Get("fallback").Raw != hops[1].Raw ||
			ojson.GetBytes(out, "model").String() != "claude-fixture-c" ||
			ojson.GetBytes(out, "usage.total_tokens").Int() != 7 ||
			ojson.GetBytes(out, "choices.0.finish_reason").String() != "stop" {
			t.Fatalf("response metadata changed: %s", out)
		}
	})
}

func TestOrdinaryClaudeStreamingChatTextStarts(t *testing.T) {
	const model = "claude-fixture-a"
	for _, tc := range []struct {
		name  string
		start string
		delta string
		want  string
	}{
		{"start_only", "  你好 ", "", "  你好 "},
		{"start_and_delta", "hel", "lo", "hello"},
		{"empty_start_and_delta", "", "hello", "hello"},
		{"identical_fragments", "go ", "go ", "go go "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var state any
			if len(ConvertClaudeResponseToOpenAI(t.Context(), model, nil, nil,
				[]byte(`data: {"type":"message_start","message":{"id":"msg_fixture","model":"claude-fixture-a"}}`), &state)) != 1 {
				t.Fatal("expected assistant role chunk")
			}
			start := `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":"` + tc.start + `"}}`
			startChunks := ConvertClaudeResponseToOpenAI(t.Context(), model, nil, nil, []byte(start), &state)
			if tc.start == "" && len(startChunks) != 0 {
				t.Fatalf("empty start emitted %d chunks", len(startChunks))
			}
			if tc.start != "" && len(startChunks) != 1 {
				t.Errorf("nonempty start emitted %d chunks, want 1", len(startChunks))
			}
			chunks := startChunks
			if tc.delta != "" {
				delta := `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"` + tc.delta + `"}}`
				chunks = append(chunks, ConvertClaudeResponseToOpenAI(t.Context(), model, nil, nil, []byte(delta), &state)...)
			}
			var text strings.Builder
			for _, chunk := range chunks {
				if ojson.GetBytes(chunk, "object").String() != "chat.completion.chunk" ||
					ojson.GetBytes(chunk, "id").String() != "msg_fixture" ||
					ojson.GetBytes(chunk, "model").String() != model {
					t.Fatalf("invalid Chat chunk metadata: %s", chunk)
				}
				text.WriteString(ojson.GetBytes(chunk, "choices.0.delta.content").String())
			}
			if text.String() != tc.want {
				t.Errorf("incremental content = %q, want %q", text.String(), tc.want)
			}
		})
	}
}
