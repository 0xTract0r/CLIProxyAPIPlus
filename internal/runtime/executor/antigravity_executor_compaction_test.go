package executor

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	executorapi "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	translation "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestOrdinaryAntigravityCompactionWithoutDocumentedProviderProtocolIsRejected(t *testing.T) {
	const input = `{"model":"gemini-3.7-flash","input":[{"type":"message","role":"user","content":"synthetic context"},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"synthetic previous answer"}]}]}`
	const streaming = `{"model":"gemini-3.7-flash","stream":true,"input":[{"type":"message","role":"user","content":"synthetic context"},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"synthetic previous answer"}]},{"type":"compaction_trigger"}]}`
	for _, tc := range []struct {
		name, model, payload, alt string
		stream                    bool
	}{
		{"explicit Responses compact", "gemini-3.7-flash", input, "responses/compact", false},
		{"Gemini stream compact trigger", "gemini-3.7-flash", streaming, "", true},
		{"Claude stream compact trigger", "claude-sonnet-4-6", streaming, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sends := 0
			ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", ordinaryAntigravityRT(func(*http.Request) (*http.Response, error) {
				sends++
				return nil, errors.New("unsupported compaction reached ordinary generation")
			}))
			exec := NewAntigravityExecutor(&config.Config{})
			request := executorapi.Request{Model: tc.model, Payload: []byte(tc.payload)}
			opts := executorapi.Options{SourceFormat: translation.FormatOpenAIResponse, ResponseFormat: translation.FormatOpenAIResponse, Alt: tc.alt}
			var err error
			if tc.stream {
				_, err = exec.ExecuteStream(ctx, ordinaryPortAuth(), request, opts)
			} else {
				_, err = exec.Execute(ctx, ordinaryPortAuth(), request, opts)
			}
			var status statusErr
			if sends != 0 || !errors.As(err, &status) || status.StatusCode() != http.StatusBadRequest || !strings.Contains(status.Error(), "compaction") {
				t.Fatalf("unsupported compaction did not fail closed: sends=%d err=%v", sends, err)
			}
		})
	}
}
