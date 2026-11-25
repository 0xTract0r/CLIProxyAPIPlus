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
	executorapi "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	translation "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestOrdinaryClaudeExplicitFastRequestPreservesTerminalFailures(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		code, want               int
		contentEncoding, payload string
	}{
		{"provider refusal", 429, 429, "", `{"error":{"message":"fixture rejected"}}`},
		{"invalid encoding", 200, 502, "gzip", "not compressed"},
		{"synthetic route failure", 0, 502, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", ordinaryClaudeTestRoundTripper(func(req *http.Request) (*http.Response, error) {
				calls++
				if tc.code == 0 {
					return nil, io.ErrUnexpectedEOF
				}
				h := http.Header{"Content-Type": {"application/json"}}
				if tc.contentEncoding != "" {
					h.Set("Content-Encoding", tc.contentEncoding)
				}
				return &http.Response{Request: req, StatusCode: tc.code, Header: h, Body: io.NopCloser(strings.NewReader(tc.payload))}, nil
			}))
			a := &coreauth.Auth{Attributes: map[string]string{"api_key": "fixture", "base_url": "https://fixture.invalid"}}
			req := executorapi.Request{Model: "claude-opus-5", Payload: []byte(`{"max_tokens":24,"speed":"fast","messages":[{"role":"user","content":"hello"}]}`)}
			opts := executorapi.Options{SourceFormat: translation.FormatClaude, Headers: http.Header{"Anthropic-Beta": {"fast-mode-2026-02-01"}}}
			_, err := NewClaudeExecutor(&config.Config{}).Execute(ctx, a, req, opts)
			var status interface{ StatusCode() int }
			if !errors.As(err, &status) || status.StatusCode() != tc.want || calls != 1 {
				t.Fatalf("status=%v attempts=%d", err, calls)
			}
		})
	}
}
