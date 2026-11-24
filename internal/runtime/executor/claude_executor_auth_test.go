package executor

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestClaudeExecutorDuplicateMetadataIsRequestScoped(t *testing.T) {
	testCases := []struct {
		name string
		run  func(context.Context, *ClaudeExecutor, *cliproxyauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) error
	}{
		{
			name: "execute",
			run: func(ctx context.Context, executor *ClaudeExecutor, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) error {
				_, errExecute := executor.Execute(ctx, auth, req, opts)
				return errExecute
			},
		},
		{
			name: "stream",
			run: func(ctx context.Context, executor *ClaudeExecutor, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) error {
				_, errStream := executor.ExecuteStream(ctx, auth, req, opts)
				return errStream
			},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			upstreamCalled := false
			transport := roundTripperFunc(func(*http.Request) (*http.Response, error) {
				upstreamCalled = true
				return nil, errors.New("unexpected upstream request")
			})
			ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(transport))

			auth := &cliproxyauth.Auth{
				Provider:   "claude",
				Attributes: map[string]string{"api_key": "ordinary-fixture"},
			}
			req := cliproxyexecutor.Request{
				Model: "claude-opus-5",
				Payload: []byte(`{"model":"claude-opus-5","messages":[{"role":"user","content":"hello"}],` +
					`"metadata":{"user_id":"{}"},"metadata":{"user_id":"{}"}}`),
			}
			errRun := testCase.run(ctx, NewClaudeExecutor(&config.Config{}), auth, req, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude})
			if errRun == nil {
				t.Fatal("duplicate metadata error = nil")
			}
			if upstreamCalled {
				t.Fatal("duplicate metadata reached upstream")
			}
			var requestErr cliproxyexecutor.RequestScopedError
			if !errors.As(errRun, &requestErr) || requestErr == nil || !requestErr.IsRequestScoped() {
				t.Fatalf("duplicate metadata error = %T %v, want request-scoped", errRun, errRun)
			}
			var statusErr interface{ StatusCode() int }
			if !errors.As(errRun, &statusErr) || statusErr.StatusCode() != http.StatusBadRequest {
				t.Fatalf("duplicate metadata error = %T %v, want HTTP 400", errRun, errRun)
			}
		})
	}
}
