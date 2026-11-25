package executor

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	authsdk "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	executorapi "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	translation "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestOrdinaryAntigravityStreamFramingCompatibility(t *testing.T) {
	for _, tc := range []struct{ name, frames, want string }{
		{"multiline and non-data", "data: {\"response\":{\"candidates\":[{\"content\":{\"role\":\"model\",\n: keepalive\nevent: message\nid: 42\ndata: \"parts\":[{\"text\":\"multiline\"}]},\"finishReason\":\"STOP\"}]}}\n\n", "multiline"},
		{"malformed event followed by terminal", "data: {\"broken\"\n\ndata: {\"response\":{\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"recovered\"}]},\"finishReason\":\"STOP\"}]}}\n\n", "recovered"},
		{"malformed without terminal", "data: {\"broken\"\n\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var sends int
			ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", ordinaryAntigravityRT(func(r *http.Request) (*http.Response, error) {
				sends++
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(tc.frames)), Request: r}, nil
			}))
			account := &authsdk.Auth{ID: "ordinary-selected", Attributes: map[string]string{"base_url": "https://fixture.invalid"}, Metadata: map[string]any{"access_token": "fixture", "project_id": "fixture"}}
			request := executorapi.Request{Model: "gemini-3.7-flash", Payload: []byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`)}
			result, err := NewAntigravityExecutor(&config.Config{}).ExecuteStream(ctx, account, request, executorapi.Options{SourceFormat: translation.FormatGemini, ResponseFormat: translation.FormatAntigravity})
			if err != nil {
				t.Fatal(err)
			}
			var output strings.Builder
			for chunk := range result.Chunks {
				if chunk.Err != nil {
					err = chunk.Err
				}
				output.Write(chunk.Payload)
			}
			if sends != 1 {
				t.Fatalf("upstream sent %d times", sends)
			}
			if tc.want != "" {
				if err != nil || !strings.Contains(output.String(), tc.want) {
					t.Fatalf("output=%q err=%v", output.String(), err)
				}
			} else {
				var status statusErr
				if !errors.As(err, &status) || status.StatusCode() != http.StatusBadGateway {
					t.Fatalf("unterminated framing returned %v", err)
				}
			}
		})
	}
}
