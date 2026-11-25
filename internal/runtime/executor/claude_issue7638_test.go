package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

const ordinaryClaudeMessageScopeModel = "claude-opus-5-5"

func TestOrdinaryClaudeIndependentPublicMessages(t *testing.T) {
	const first = `{
		"max_tokens":32,
		"tools":[
			{"type":"web_search_20250305","name":"web_search"},
			{"name":"fixture_lookup","input_schema":{"type":"object","properties":{"topic":{"type":"string"}}}}
		],
		"messages":[
			{"role":"user","content":[{"type":"text","text":"first request only"}]},
			{"role":"assistant","content":[
				{"type":"server_tool_use","id":"srvtoolu_fixture_search","name":"web_search","input":{"query":"fixture"}},
				{"type":"web_search_tool_result","tool_use_id":"srvtoolu_fixture_search","content":[]},
				{"type":"tool_use","id":"toolu_fixture_lookup","name":"fixture_lookup","input":{"topic":"explicit lookup topic"}}
			]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"toolu_fixture_lookup","content":[{"type":"text","text":"explicit tool output"}]},
				{"type":"text","text":"ordinary user text"}
			]}
		]
	}`
	const second = `{"max_tokens":32,"messages":[{"role":"user","content":[{"type":"text","text":"second request only"}]}]}`
	priorContent := []string{"first request only", "srvtoolu_fixture_search", "toolu_fixture_lookup", "explicit lookup topic", "explicit tool output", "ordinary user text", "message reply 1"}
	for _, mode := range []string{"json", "stream"} {
		t.Run(mode, func(t *testing.T) {
			stream := mode == "stream"
			var sent [][]byte
			ctx, auth := ordinaryClaudeMessageScopeFixture(t, "/v1/messages", func(b []byte) (string, string) {
				sent = append(sent, b)
				reply := fmt.Sprintf("message reply %d", len(sent))
				return ordinaryClaudeMessageScopeReply(stream, reply)
			})
			base := NewClaudeExecutor(&config.Config{})
			e := base.ForAPIKey().(*ClaudeExecutor)
			ordinaryClaudeMessageScopeExecute(t, ctx, e, auth, stream, first, "message reply 1")
			secondReply := ordinaryClaudeMessageScopeExecute(t, ctx, e, auth, stream, second, "message reply 2")
			if len(sent) != 2 {
				t.Fatalf("upstream calls = %d, want 2", len(sent))
			}
			for i, body := range sent {
				if ojson.GetBytesNoCopy(body, "stream").Bool() != stream {
					t.Fatalf("request %d has unexpected stream flag: %s", i+1, body)
				}
				for _, message := range ojson.GetBytesNoCopy(body, "messages").Array() {
					if content := message.Get("content"); content.IsArray() && len(content.Array()) == 0 {
						t.Fatalf("request %d sent an empty message content array: %s", i+1, body)
					}
				}
			}
			assistant := ojson.GetBytesNoCopy(sent[0], "messages.1")
			if blocks := assistant.Get("content").Array(); len(blocks) != 3 {
				t.Fatalf("assistant sent %d tool-related blocks, want 3: %s", len(blocks), assistant.Raw)
			}
			if results := assistant.Get("content.1.content"); !results.IsArray() || len(results.Array()) != 0 {
				t.Errorf("search result content = %s, want the explicit empty result array", results.Raw)
			}
			for path, want := range map[string]string{
				"messages.0.content.0.text":           "first request only",
				"messages.1.role":                     "assistant",
				"messages.1.content.0.type":           "server_tool_use",
				"messages.1.content.0.id":             "srvtoolu_fixture_search",
				"messages.1.content.0.name":           "web_search",
				"messages.1.content.0.input.query":    "fixture",
				"messages.1.content.1.type":           "web_search_tool_result",
				"messages.1.content.1.tool_use_id":    "srvtoolu_fixture_search",
				"messages.1.content.2.type":           "tool_use",
				"messages.1.content.2.id":             "toolu_fixture_lookup",
				"messages.1.content.2.name":           "fixture_lookup",
				"messages.1.content.2.input.topic":    "explicit lookup topic",
				"messages.2.role":                     "user",
				"messages.2.content.0.type":           "tool_result",
				"messages.2.content.0.tool_use_id":    "toolu_fixture_lookup",
				"messages.2.content.0.content.0.type": "text",
				"messages.2.content.0.content.0.text": "explicit tool output",
				"messages.2.content.1.text":           "ordinary user text",
			} {
				if got := ojson.GetBytesNoCopy(sent[0], path).String(); got != want {
					t.Errorf("first request %s = %q, want %q", path, got, want)
				}
			}
			if got := ojson.GetBytesNoCopy(sent[1], "messages").Array(); len(got) != 1 ||
				got[0].Get("content.0.text").String() != "second request only" {
				t.Fatalf("second request messages = %s, want the one explicit user message", sent[1])
			}
			for _, prior := range priorContent {
				if strings.Contains(string(sent[1]), prior) {
					t.Errorf("second wire request included %q from the earlier request or response", prior)
				}
				if strings.Contains(secondReply, prior) {
					t.Errorf("second response echoed %q from the earlier request or response", prior)
				}
			}
		})
	}
}

func TestOrdinaryClaudePayloadRulesOnPublicMessages(t *testing.T) {
	const first = `{"max_tokens":32,"messages":[{"role":"user","content":[{"type":"text","text":"caller input before rule"}]}]}`
	const second = `{"max_tokens":32,"messages":[{"role":"user","content":[{"type":"text","text":"fresh caller input"}]}]}`
	cfg := &config.Config{Payload: config.PayloadConfig{Override: []config.PayloadRule{{
		Models: []config.PayloadModelRule{{Name: ordinaryClaudeMessageScopeModel, Protocol: "claude"}},
		Params: map[string]any{
			"max_tokens": 999,
			"messages": []any{map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "configured replacement"},
			}}},
		},
	}}}}
	for _, mode := range []string{"json", "stream"} {
		t.Run(mode, func(t *testing.T) {
			stream := mode == "stream"
			var sent [][]byte
			ctx, auth := ordinaryClaudeMessageScopeFixture(t, "/v1/messages", func(b []byte) (string, string) {
				sent = append(sent, b)
				return ordinaryClaudeMessageScopeReply(stream, "ok")
			})
			base := NewClaudeExecutor(cfg)
			ordinaryClaudeMessageScopeExecute(t, ctx, base.ForAPIKey().(*ClaudeExecutor), auth, stream, first, "ok")
			ordinaryClaudeMessageScopeExecute(t, ctx, base.WithConfig(&config.Config{}), auth, stream, second, "ok")
			if len(sent) != 2 {
				t.Fatalf("upstream calls = %d, want 2", len(sent))
			}
			for i, want := range []struct {
				text    string
				tokens  int64
				exclude string
			}{
				{"configured replacement", 999, "caller input before rule"},
				{"fresh caller input", 32, "configured replacement"},
			} {
				body := sent[i]
				if messages := ojson.GetBytesNoCopy(body, "messages").Array(); len(messages) != 1 || messages[0].Get("content.0.text").String() != want.text ||
					ojson.GetBytesNoCopy(body, "max_tokens").Int() != want.tokens || strings.Contains(string(body), want.exclude) {
					t.Errorf("request %d did not use the current payload rule and current messages: %s", i+1, body)
				}
			}
		})
	}
}

func TestOrdinaryClaudePublicTokenCountUsesOnlyCurrentInput(t *testing.T) {
	var sent [][]byte
	ctx, auth := ordinaryClaudeMessageScopeFixture(t, "/v1/messages/count_tokens", func(b []byte) (string, string) {
		sent = append(sent, b)
		n := 11
		if len(sent) == 1 {
			n = 7
		}
		return "application/json", fmt.Sprintf(`{"input_tokens":%d}`, n)
	})
	e := NewClaudeExecutor(&config.Config{})
	for i, tc := range []struct {
		executor *ClaudeExecutor
		input    string
		count    int64
	}{
		{e, "first counting input", 7},
		{e, "independent counting input", 11},
		{NewClaudeExecutor(&config.Config{}), "independent counting input", 11},
	} {
		req := cliproxyexecutor.Request{
			Model:   ordinaryClaudeMessageScopeModel,
			Payload: []byte(fmt.Sprintf(`{"max_tokens":32,"messages":[{"role":"user","content":[{"type":"text","text":%q}]}]}`, tc.input)),
		}
		res, err := tc.executor.CountTokens(ctx, auth, req, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude})
		if err != nil || ojson.GetBytesNoCopy(res.Payload, "input_tokens").Int() != tc.count {
			t.Fatalf("count %d = %s, error = %v, want %d input tokens", i+1, res.Payload, err, tc.count)
		}
		if len(sent) != i+1 {
			t.Fatalf("upstream count calls = %d, want %d", len(sent), i+1)
		}
		body := sent[i]
		messages := ojson.GetBytesNoCopy(body, "messages").Array()
		if len(messages) != 1 || messages[0].Get("content.0.text").String() != tc.input ||
			(i > 0 && strings.Contains(string(body), "first counting input")) {
			t.Fatalf("count %d included messages outside its request: %s", i+1, body)
		}
	}
}

func ordinaryClaudeMessageScopeFixture(t *testing.T, path string, reply func([]byte) (string, string)) (context.Context, *cliproxyauth.Auth) {
	t.Helper()
	tr := canonicalClaudeTestTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPost || r.URL.Scheme != "https" || r.URL.Host != "api.anthropic.com" || r.URL.Path != path || r.Header.Get("x-api-key") != "fixture-message-scope" {
			return nil, fmt.Errorf("unexpected request to fake transport: %s %s", r.Method, r.URL)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		contentType, payload := reply(body)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {contentType}},
			Body:       io.NopCloser(strings.NewReader(payload)),
			Request:    r,
		}, nil
	})
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", tr)
	auth := &cliproxyauth.Auth{ID: "message-scope-fixture", Provider: "claude", Attributes: map[string]string{
		"api_key":  "fixture-message-scope",
		"base_url": "https://api.anthropic.com",
	}}
	return ctx, auth
}

func ordinaryClaudeMessageScopeExecute(t *testing.T, ctx context.Context, e *ClaudeExecutor, auth *cliproxyauth.Auth, stream bool, payload, want string) string {
	t.Helper()
	req := cliproxyexecutor.Request{Model: ordinaryClaudeMessageScopeModel, Payload: []byte(payload)}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, Stream: stream}
	if !stream {
		out, err := e.Execute(ctx, auth, req, opts)
		if err != nil || ojson.GetBytesNoCopy(out.Payload, "content.0.text").String() != want {
			t.Fatalf("JSON response = %s, error = %v, want %q", out.Payload, err, want)
		}
		return string(out.Payload)
	}
	out, err := e.ExecuteStream(ctx, auth, req, opts)
	if err != nil {
		t.Fatal(err)
	}
	var wire strings.Builder
	for chunk := range out.Chunks {
		if chunk.Err != nil {
			t.Fatal(chunk.Err)
		}
		wire.Write(chunk.Payload)
	}
	deltas, stops := 0, 0
	for _, line := range strings.Split(wire.String(), "\n") {
		data, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		event := ojson.Parse(strings.TrimSpace(data))
		if event.Get("type").String() == "content_block_delta" && event.Get("delta.text").String() == want {
			deltas++
		}
		if event.Get("type").String() == "message_stop" {
			stops++
		}
	}
	if deltas != 1 || stops != 1 {
		t.Fatalf("SSE must return one matching text delta and one terminal, got %d and %d: %s", deltas, stops, wire.String())
	}
	return wire.String()
}

func ordinaryClaudeMessageScopeReply(stream bool, reply string) (string, string) {
	if !stream {
		return "application/json", fmt.Sprintf(`{"id":"msg_fixture","type":"message","role":"assistant","model":%q,"content":[{"type":"text","text":%q}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`, ordinaryClaudeMessageScopeModel, reply)
	}
	return "text/event-stream",
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_fixture\",\"type\":\"message\",\"model\":\"" + ordinaryClaudeMessageScopeModel + "\",\"role\":\"assistant\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n" +
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
			fmt.Sprintf("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":%q}}\n\n", reply) +
			"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n" +
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
}
