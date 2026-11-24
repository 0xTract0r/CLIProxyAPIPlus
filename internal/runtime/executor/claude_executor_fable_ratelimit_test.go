package executor

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestOrdinaryClaudeModelRateLimitReturnsOriginalRefusal(t *testing.T) {
	const model = "claude-fable-5"
	for _, stream := range []bool{false, true} {
		name := "sync"
		if stream {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			calls := 0
			ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", ordinaryClaudeTestRoundTripper(func(r *http.Request) (*http.Response, error) {
				calls++
				b, err := io.ReadAll(r.Body)
				if err != nil || ojson.GetBytesNoCopy(b, "model").String() != model {
					t.Fatal("request model changed")
				}
				return &http.Response{Request: r, StatusCode: 429, Header: http.Header{"Retry-After": {"12"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"Fable usage limit reached"}}`))}, nil
			}))
			a := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": "https://fixture.invalid", "api_key": "fixture"}}
			req := cliproxyexecutor.Request{Model: model, Payload: []byte(`{"max_tokens":20,"messages":[{"role":"user","content":"hi"}]}`)}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude}
			e := NewClaudeExecutor(&config.Config{})
			var err error
			if stream {
				_, err = e.ExecuteStream(ctx, a, req, opts)
			} else {
				_, err = e.Execute(ctx, a, req, opts)
			}
			var status statusErr
			if !errors.As(err, &status) || status.StatusCode() != 429 || !strings.Contains(status.Error(), "Fable usage limit reached") ||
				status.RetryAfter() == nil || *status.RetryAfter() != 12*time.Second || calls != 1 {
				t.Fatalf("provider refusal lost: err=%v attempts=%d", err, calls)
			}
		})
	}
}
