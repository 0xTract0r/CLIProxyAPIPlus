package openai

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
)

func TestOpenAIOrdinaryCompactCatalog(t *testing.T) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/models?client_version=1.2.3", nil)
	NewOpenAIAPIHandler(&handlers.BaseAPIHandler{}).OpenAIModels(c)
	var body map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK || body["models"] == nil || body["data"] != nil {
		t.Fatalf("unexpected compact catalog response status=%d, body=%s", rec.Code, rec.Body.String())
	}
}

func TestOpenAIOrdinaryResponseObjectToChat(t *testing.T) {
	raw := []byte(`{"id":"fixture","object":"response","status":"completed","model":"fixture-model","output":[{"type":"message","content":[{"type":"output_text","text":"hello"}]}]}`)
	message := convertResponsesObjectToChatCompletion(context.Background(), "fixture-model", nil, nil, raw)
	var body struct {
		Choices []struct {
			Finish  string `json:"finish_reason"`
			Message struct{ Content string }
		}
	}
	if err := json.Unmarshal(message, &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Choices) != 1 || body.Choices[0].Finish != "stop" || body.Choices[0].Message.Content != "hello" {
		t.Fatalf("unexpected conversion: %s", message)
	}
	incomplete := []byte(`{"id":"fixture","object":"response","status":"incomplete","model":"fixture-model","incomplete_details":{"reason":"max_output_tokens"},"output":[]}`)
	payload := convertResponsesObjectToChatCompletion(context.Background(), "fixture-model", nil, nil, incomplete)
	if err := json.Unmarshal(payload, &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Choices) != 1 || body.Choices[0].Finish != "length" {
		t.Fatalf("incomplete response finish: %s", payload)
	}

}

type openAIOrdinaryFlushWriter struct {
	gin.ResponseWriter
	err error
}

func (w openAIOrdinaryFlushWriter) FlushError() error { return w.err }

func TestOpenAIOrdinaryResponsesAsChatStream(t *testing.T) {
	const created = `data: {"type":"response.created","response":{"id":"fixture","status":"in_progress","model":"fixture-model"}}`
	const partial = `data: {"type":"response.output_text.delta","delta":"hello"}`
	const complete = `data: {"type":"response.completed","response":{"id":"fixture","status":"completed","model":"fixture-model"}}`
	const incomplete = `data: {"type":"response.incomplete","response":{"id":"fixture","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"}}}`
	const failed = `data: {"type":"response.failed","response":{"status":"failed","error":{"message":"fixture failure"}}}`
	blocked := errors.New("fixture flush error")
	for _, test := range []struct {
		name      string
		initial   string
		frames    []string
		flushErr  error
		finish    string
		wantError bool
	}{
		{"completed", created, []string{partial, complete, complete}, nil, "stop", false},
		{"initial completed", complete, nil, nil, "stop", false},
		{"named terminal frame", "event: response.completed\n" + `data: {"response":{"status":"completed"}}`, nil, nil, "stop", false},
		{"incomplete", created, []string{incomplete}, nil, "length", false},
		{"missing terminal", created, []string{partial}, nil, "", true},
		{"failed event once", created, []string{partial, failed, complete}, nil, "", true},
		{"done with failure status", created, []string{`data: {"type":"response.done","response":{"status":"failed"}}`}, nil, "", true},
		{"done with incomplete status", created, []string{`data: {"type":"response.done","response":{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"}}}`}, nil, "length", false},
		{"terminal flush failure", complete, nil, blocked, "", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			if test.flushErr != nil {
				c.Writer = openAIOrdinaryFlushWriter{c.Writer, test.flushErr}
			}
			data := make(chan []byte, len(test.frames))
			for _, f := range test.frames {
				data <- []byte(f)
			}
			close(data)
			var once int
			var outcome error
			NewOpenAIAPIHandler(&handlers.BaseAPIHandler{}).forwardResponsesAsChatStream(c, c.Writer, func(err error) { once++; outcome = err }, data, nil, c.Request.Context(), "fixture-model", nil, nil, []byte(test.initial))
			if once != 1 || (outcome != nil) != test.wantError {
				t.Fatalf("cancel count %d, err %v", once, outcome)
			}
			body := rec.Body.String()
			if test.flushErr != nil {
				if !errors.Is(outcome, test.flushErr) {
					t.Fatalf("flush error lost: %v", outcome)
				}
				return
			}
			if test.wantError {
				errorFrames := 0
				for _, frame := range strings.Split(body, "\n\n") {
					var event map[string]json.RawMessage
					if json.Unmarshal([]byte(strings.TrimPrefix(frame, "data: ")), &event) == nil && event["error"] != nil {
						errorFrames++
					}
				}
				if strings.Contains(body, "[DONE]") || errorFrames != 1 {
					t.Fatalf("expected one top-level error and no success marker: %q", body)
				}
				return
			}
			if strings.Count(body, "[DONE]") != 1 || !strings.Contains(body, `"finish_reason":"`+test.finish+`"`) {
				t.Fatalf("invalid success terminal: %q", body)
			}
		})
	}
}
