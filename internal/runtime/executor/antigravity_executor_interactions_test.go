package executor

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	authsdk "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	executorapi "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	translation "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestOrdinaryAntigravityInteractionsTranslatesInputAndCompletion(t *testing.T) {
	const frames = "data: {\"response\":{\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"fixture answer\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"totalTokenCount\":5}}}\n\ndata: [DONE]\n\n"
	var request []byte
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", ordinaryAntigravityRT(func(r *http.Request) (*http.Response, error) {
		var err error
		request, err = io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		if r.URL.Path != antigravityStreamPath || r.URL.Query().Get("alt") != "sse" {
			t.Fatal("Interactions request selected an unexpected upstream path")
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}},
			Body: io.NopCloser(strings.NewReader(frames)), Request: r}, nil
	}))
	auth := &authsdk.Auth{Attributes: map[string]string{"base_url": "https://fixture.invalid"}, Metadata: map[string]any{"project_id": "explicit-project", "access_token": "fixture"}}
	req := executorapi.Request{Model: "fixture-model", Payload: []byte(`{"input":"hi","stream":true}`)}
	opts := executorapi.Options{SourceFormat: translation.FormatInteractions, ResponseFormat: translation.FormatInteractions}
	result, err := NewAntigravityExecutor(&config.Config{}).ExecuteStream(ctx, auth, req, opts)
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	for event := range result.Chunks {
		if event.Err != nil {
			t.Fatal(event.Err)
		}
		out.Write(event.Payload)
	}
	if ojson.GetBytesNoCopy(request, "request.contents.0.parts.0.text").String() != "hi" || ojson.GetBytesNoCopy(request, "request.input").Exists() {
		t.Fatalf("Interactions request did not translate to the standard model format: %s", request)
	}
	if !strings.Contains(out.String(), "fixture answer") || strings.Count(out.String(), `"event_type":"interaction.completed"`) != 1 {
		t.Fatalf("Interactions did not receive one ordinary completed response: %s", out.String())
	}
}
