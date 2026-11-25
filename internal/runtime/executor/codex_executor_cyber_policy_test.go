package executor

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

type codexPolicyFixtureTransport func(*http.Request) (*http.Response, error)

func (f codexPolicyFixtureTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestOrdinaryCodexPolicyHandling(t *testing.T) {
	refusal := "data: {\"type\":\"error\",\"status\":403,\"error\":{\"code\":\"cyber_policy\",\"message\":\"fixture refusal\"}}\n\n"
	duplicate := "data: {\"type\":\"response.failed\",\"status\":403,\"response\":{\"error\":{\"code\":\"cyber_policy\",\"message\":\"fixture refusal\"}}}\n\n"
	completed := "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_fixture\",\"status\":\"completed\",\"output\":[]}}\n\n"
	for _, tc := range []struct {
		name        string
		body        string
		wantRefusal bool
	}{
		{"single_terminal", refusal, true},
		{"duplicate_terminal", refusal + duplicate, true},
		{"normal_completion", completed, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", codexPolicyFixtureTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(tc.body)), Request: r}, nil
			}))
			executor := NewCodexExecutor(&config.Config{})
			auth := &coreauth.Auth{ID: "fixture", Provider: "codex", Attributes: map[string]string{"base_url": "https://upstream.invalid", "api_key": "fixture"}}
			result, err := executor.ExecuteStream(ctx, auth, coreexecutor.Request{Model: "gpt-5", Payload: []byte(`{"model":"gpt-5","input":"fixture"}`)}, coreexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse})
			if err != nil {
				t.Fatal(err)
			}
			terminal, success := 0, 0
			for chunk := range result.Chunks {
				if chunk.Err != nil {
					var coded interface{ StatusCode() int }
					if !errors.As(chunk.Err, &coded) || coded.StatusCode() != http.StatusForbidden || !strings.Contains(chunk.Err.Error(), "cyber_policy") {
						t.Fatalf("unexpected terminal error: %v", chunk.Err)
					}
					terminal++
				}
				if strings.Contains(string(chunk.Payload), "response.completed") {
					success++
				}
			}
			if calls != 1 || (tc.wantRefusal && (terminal != 1 || success != 0)) || (!tc.wantRefusal && (terminal != 0 || success != 1)) {
				t.Fatalf("calls=%d terminal=%d success=%d", calls, terminal, success)
			}
		})
	}
}
