package responses

import (
	"context"
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
)

func TestGeminiResponsesLateSignaturePreservesSummary(t *testing.T) {
	chunks := []string{
		`data: {"candidates":[{"content":{"parts":[{"text":"Let me think.","thought":true}]}}],"responseId":"issue-5513"}`,
		`data: {"candidates":[{"content":{"parts":[{"text":"The answer is 42."}]}}]}`,
		`data: {"candidates":[{"content":{"parts":[{"text":"","thoughtSignature":"` + testResponsesGeminiThoughtSignature + `"}]},"finishReason":"STOP"}]}`,
		`data: [DONE]`,
	}
	var state any
	var completed ojson.Result
	for _, chunk := range chunks {
		for _, event := range ConvertGeminiResponseToOpenAIResponses(context.Background(), "gemini-3.8-flash-high", nil, nil, []byte(chunk), &state) {
			name, data := parseSSEEvent(t, event)
			if (name == "response.output_item.added" || name == "response.output_item.done") && data.Get("item.type").String() == "reasoning" && len(data.Get("item.summary").Array()) == 0 && data.Get("item.encrypted_content").String() != "" {
				t.Fatalf("late signature emitted an empty reasoning item: %s", data.Raw)
			}
			if name == "response.completed" {
				completed = data.Get("response.output")
			}
		}
	}
	if len(completed.Array()) != 2 || completed.Get("0.summary.0.text").String() != "Let me think." || completed.Get("1.type").String() != "message" {
		t.Fatalf("unexpected completed output: %s", completed.Raw)
	}
	replayed, _ := ConvertOpenAIResponsesRequestToGemini("gemini-3.8-flash-high", []byte(`{"input":`+completed.Raw+`}`), false)
	if ojson.GetBytesNoCopy(replayed, "contents.0.parts.1.thoughtSignature").String() != testResponsesGeminiThoughtSignature {
		t.Fatalf("late text signature was not restored: %s", replayed)
	}
}

func TestGeminiResponsesTextSignatureCacheRejectsInvalidSignature(t *testing.T) {
	if cacheGeminiResponsesTextSignatures("gemini-test", "msg_resp_invalid_0", "answer", []string{"invalid"}) {
		t.Fatal("invalid signature was accepted into replay cache")
	}
	var state any
	line := []byte(`data: {"candidates":[{"content":{"parts":[{"text":"answer"},{"text":"","thoughtSignature":"invalid"}]},"finishReason":"STOP"}],"responseId":"invalid-cache-fallback"}`)
	var completed ojson.Result
	streamEvents := ConvertGeminiResponseToOpenAIResponses(context.Background(), "gemini-test", nil, nil, line, &state)
	streamEvents = append(streamEvents, ConvertGeminiResponseToOpenAIResponses(context.Background(), "gemini-test", nil, nil, []byte("[DONE]"), &state)...)
	for _, chunk := range streamEvents {
		name, data := parseSSEEvent(t, chunk)
		if name == "response.completed" {
			completed = data.Get("response.output")
		}
	}
	if len(completed.Array()) != 2 || completed.Get("1.type").String() != "reasoning" {
		t.Fatalf("uncacheable signature lost its fallback carrier: %s", completed.Raw)
	}
}

func TestGeminiResponsesLateSignatureReplayWithThinkingSuffix(t *testing.T) {
	var state any
	line := []byte(`data: {"candidates":[{"content":{"parts":[{"text":"suffix answer"},{"text":"","thoughtSignature":"` + testResponsesGeminiThoughtSignature + `"}]},"finishReason":"STOP"}],"responseId":"issue-5513-suffix"}`)
	var completed ojson.Result
	streamEvents := ConvertGeminiResponseToOpenAIResponses(context.Background(), "gemini-2.5-pro(8192)", nil, nil, line, &state)
	streamEvents = append(streamEvents, ConvertGeminiResponseToOpenAIResponses(context.Background(), "gemini-2.5-pro(8192)", nil, nil, []byte("[DONE]"), &state)...)
	for _, chunk := range streamEvents {
		name, data := parseSSEEvent(t, chunk)
		if name == "response.completed" {
			completed = data.Get("response.output")
		}
	}
	output, _ := ConvertOpenAIResponsesRequestToGemini("gemini-2.5-pro", []byte(`{"input":`+completed.Raw+`}`), false)
	if ojson.GetBytesNoCopy(output, "contents.0.parts.0.thoughtSignature").String() != testResponsesGeminiThoughtSignature {
		t.Fatalf("executor base model could not restore suffixed response signature: %s", output)
	}
}

func TestGeminiResponsesLateThoughtSignatureDoesNotBindEarlierMessage(t *testing.T) {
	signature2 := differentResponsesGeminiThoughtSignature(t)
	lines := []string{
		`data: {"candidates":[{"content":{"parts":[{"text":"earlier answer"}]}}],"responseId":"issue-5513-thought-boundary"}`,
		`data: {"candidates":[{"content":{"parts":[{"text":"later thought","thought":true,"thoughtSignature":"` + testResponsesGeminiThoughtSignature + `"}]}}]}`,
		`data: {"candidates":[{"content":{"parts":[{"text":"","thoughtSignature":"` + signature2 + `"}]},"finishReason":"STOP"}]}`,
		`data: [DONE]`,
	}
	var state any
	var completed ojson.Result
	for _, line := range lines {
		for _, chunk := range ConvertGeminiResponseToOpenAIResponses(context.Background(), "gemini-3.8-flash-high", nil, nil, []byte(line), &state) {
			name, data := parseSSEEvent(t, chunk)
			if name == "response.completed" {
				completed = data.Get("response.output")
			}
		}
	}
	output, _ := ConvertOpenAIResponsesRequestToGemini("gemini-3.8-flash-high", []byte(`{"input":`+completed.Raw+`}`), false)
	parts := ojson.GetBytesNoCopy(output, "contents.0.parts").Array()
	if len(parts) < 2 || parts[0].Get("thoughtSignature").String() != "" || parts[1].Get("thoughtSignature").String() != testResponsesGeminiThoughtSignature || !strings.Contains(string(output), signature2) {
		t.Fatalf("late thought signature rebound to the earlier message: %s", output)
	}
}

func TestOrdinaryGeminiResponsesKeepsUnsignedCallAndResult(t *testing.T) {
	raw := []byte(`{"input":[{"type":"function_call","call_id":"fixture","name":"lookup","arguments":"{}"},{"type":"function_call_output","call_id":"fixture","output":"fixture-result"}]}`)
	out, err := ConvertOpenAIResponsesRequestToGemini("gemini-3-flash", raw, false)
	if err != nil {
		t.Fatal(err)
	}
	call := ojson.GetBytes(out, "contents.0.parts.0")
	result := ojson.GetBytes(out, "contents.1.parts.0.functionResponse")
	if call.Get("functionCall.name").String() != "lookup" || call.Get("thoughtSignature").Exists() || result.Get("name").String() != "lookup" || !strings.Contains(result.Raw, "fixture-result") {
		t.Fatal("ordinary ordered tool call/result changed")
	}
}
