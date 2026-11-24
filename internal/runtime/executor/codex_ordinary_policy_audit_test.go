package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestOrdinaryCodexRefusalReachesServingManagerOnce(t *testing.T) {
	for _, name := range []string{"http", "sync-sse", "stream", "buffered-stream", "unrelated"} {
		t.Run(name, func(t *testing.T) {
			m := coreauth.NewManager(nil, nil, nil)
			a := &coreauth.Auth{ID: "local-audit-fixture", Provider: "codex", Attributes: map[string]string{"base_url": "https://upstream.invalid", "api_key": "fixture"}}
			if _, err := m.Register(context.Background(), a); err != nil {
				t.Fatal(err)
			}
			cfg := &config.Config{}
			cfg.Codex.StreamBootstrapBuffering = name == "buffered-stream"
			e := NewCodexAutoExecutorWithManager(cfg, m)
			calls := 0
			ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", ordinaryCodexTestRoundTripper(func(r *http.Request) (*http.Response, error) {
				calls++
				code := "cyber_policy"
				if name == "unrelated" {
					code = "fixture_refusal"
				}
				event := fmt.Sprintf(`{"type":"response.failed","status":403,"response":{"error":{"code":%q,"message":"fixture refusal"}}}`, code)
				body := "data: " + event + "\n\n" + "data: " + event + "\n\n"
				status := 200
				if name == "http" || name == "unrelated" {
					status = 403
					body = fmt.Sprintf(`{"error":{"code":%q,"message":"fixture refusal"}}`, code)
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Retry-After": []string{"17"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
			}))
			req := coreexecutor.Request{Model: "gpt-5", Payload: []byte(`{"model":"gpt-5","input":"fixture"}`)}
			opts := coreexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse}
			var err error
			if name == "stream" || name == "buffered-stream" {
				var result *coreexecutor.StreamResult
				result, err = e.ExecuteStream(ctx, a, req, opts)
				if err == nil {
					for ch := range result.Chunks {
						if ch.Err != nil {
							err = ch.Err
						}
					}
				}
			} else {
				_, err = e.Execute(ctx, a, req, opts)
			}
			got, ok := m.GetByID(a.ID)
			if !ok {
				t.Fatal("credential missing")
			}
			expected := 1
			if name == "unrelated" {
				expected = 0
			}
			if err == nil || calls != 1 || got.CyberPolicyFlagCount != expected {
				t.Fatalf("expected one refusal and %d audit records; got error %v, calls %d, count %d", expected, err, calls, got.CyberPolicyFlagCount)
			}
		})
	}
}
