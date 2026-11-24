package executor

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	authsdk "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	executorapi "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	translation "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestOrdinaryClaudeRefusalRetainsHeaderAndNeverRepeatsRequest(t *testing.T) {
	for _, tc := range []struct {
		name  string
		code  int
		retry string
		body  string
	}{
		{"usage-limit-header", 429, "123", `{"error":{"message":"usage limit reached"}}`},
		{"overload-without-header", 429, "", `{"error":{"message":"model overloaded"}}`},
		{"forbidden", 403, "13", `{"error":{"message":"explicit refusal"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", ordinaryClaudeTestRoundTripper(func(r *http.Request) (*http.Response, error) {
				calls++
				h := make(http.Header)
				if tc.retry != "" {
					h.Set("Retry-After", tc.retry)
				}
				return &http.Response{StatusCode: tc.code, Header: h, Body: io.NopCloser(strings.NewReader(tc.body)), Request: r}, nil
			}))
			auth := &authsdk.Auth{Attributes: map[string]string{"base_url": "https://fixture.invalid", "api_key": "fixture"}}
			req := executorapi.Request{Model: "claude-fixture", Payload: []byte(`{"max_tokens":10,"messages":[{"role":"user","content":"ordinary input"}]}`)}
			_, err := NewClaudeExecutor(&config.Config{}).Execute(ctx, auth, req, executorapi.Options{SourceFormat: translation.FormatClaude})
			var refused statusErr
			if !errors.As(err, &refused) || refused.StatusCode() != tc.code || calls != 1 {
				t.Fatalf("attempts=%d err=%v", calls, err)
			}
			if tc.retry == "" {
				if refused.RetryAfter() != nil {
					t.Fatalf("invented retry time: %v", refused.RetryAfter())
				}
			} else {
				want, _ := time.ParseDuration(tc.retry + "s")
				if refused.RetryAfter() == nil || *refused.RetryAfter() != want {
					t.Fatalf("retry=%v want %v", refused.RetryAfter(), want)
				}
			}
		})
	}
}

func TestOrdinaryClaudeTransportErrorDoesNotExposePrivateRoute(t *testing.T) {
	const private = "https://fixture-user:fixture-pass@private-route.invalid"
	calls := 0
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", ordinaryClaudeTestRoundTripper(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("failure contacting " + private)
	}))
	auth := &authsdk.Auth{Attributes: map[string]string{"base_url": "https://fixture.invalid", "api_key": "fixture"}}
	req := executorapi.Request{Model: "claude-fixture", Payload: []byte(`{"max_tokens":10,"messages":[{"role":"user","content":"ordinary input"}]}`)}
	_, err := NewClaudeExecutor(&config.Config{}).Execute(ctx, auth, req, executorapi.Options{SourceFormat: translation.FormatClaude})
	if err == nil || calls != 1 || strings.Contains(err.Error(), "fixture-pass") || strings.Contains(err.Error(), "private-route.invalid") {
		t.Fatalf("ordinary transport did not return a safe terminal error: attempts=%d", calls)
	}
}
