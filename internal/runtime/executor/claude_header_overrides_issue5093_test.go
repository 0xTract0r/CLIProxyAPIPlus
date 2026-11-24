package executor

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	executorapi "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	translation "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

type ordinaryClaudeHeaderFixture func(*http.Request) (*http.Response, error)

func (f ordinaryClaudeHeaderFixture) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestOrdinaryClaude5093HeaderPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name, base, credentialBeta, want string
		strip, append                    []string
	}{
		{"provider applies operator policy last", "https://api.anthropic.com", "credential-beta", "body-beta,caller-beta,operator-beta",
			[]string{" caller-beta ", "strip-beta"}, []string{"caller-beta", "operator-beta", "body-beta", "claude-code-untrusted"}},
		{"gateway credential beta takes precedence", "https://fixture.invalid", "credential-beta,credential-beta,claude-code-untrusted", "credential-beta",
			[]string{"credential-beta"}, []string{"operator-beta"}},
		{"gateway without override uses operator policy", "https://fixture.invalid", "", "caller-beta,body-beta,operator-beta",
			[]string{"strip-beta"}, []string{"operator-beta"}},
		{"remove empty beta header", "https://api.anthropic.com", "", "",
			[]string{"caller-beta", "strip-beta", "body-beta"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &coreauth.Auth{Attributes: map[string]string{
				"base_url": tc.base, "api_key": "fixture-key",
				"header:X-Client-Request-Id": "fixture-request-id", "header:X-Custom-Test": "fixture-custom",
				"header:Authorization": "forbidden", "header:X-Api-Key": "forbidden",
				"header:Host": "forbidden.invalid", "header:X-App": "forbidden", "header:X-Foo-Api-Key": "forbidden",
				"header:X-Claude-Code-Session-Id": "forbidden", "header:X-Forwarded-Host": "forbidden.invalid",
				"header:X-Custom-Bad": "forbidden\r\nX-Another: value",
			}}
			if tc.credentialBeta != "" {
				a.Attributes["header:Anthropic-Beta"] = tc.credentialBeta
			}
			cfg := &config.Config{}
			cfg.ClaudeHeaderDefaults.AnthropicBetaStrip = tc.strip
			cfg.ClaudeHeaderDefaults.AnthropicBetaAppend = tc.append
			calls := 0
			ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", ordinaryClaudeHeaderFixture(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.String() != tc.base+"/v1/messages" || r.Host != r.URL.Host || r.Header.Get("X-Api-Key") != "fixture-key" || r.Header.Get("Authorization") != "" {
					t.Fatal("standard origin or authentication changed")
				}
				if got := r.Header.Get("Anthropic-Beta"); got != tc.want {
					t.Errorf("Beta=%q want %q", got, tc.want)
				}
				if tc.want == "" && len(r.Header.Values("Anthropic-Beta")) != 0 {
					t.Error("empty Beta header was sent")
				}
				if r.Header.Get("X-Client-Request-Id") != "fixture-request-id" || r.Header.Get("X-Custom-Test") != "fixture-custom" {
					t.Error("ordinary custom headers were not preserved")
				}
				for _, name := range []string{"X-App", "X-Foo-Api-Key", "X-Claude-Code-Session-Id", "X-Forwarded-Host", "X-Custom-Bad"} {
					if r.Header.Get(name) != "" {
						t.Errorf("restricted header %s was sent", name)
					}
				}
				content, err := io.ReadAll(r.Body)
				if err != nil || ojson.GetBytesNoCopy(content, "betas").Exists() {
					t.Fatal("local beta control field leaked into payload")
				}
				return &http.Response{Request: r, StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"fixture","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}]}`))}, nil
			}))
			req := executorapi.Request{Model: "claude-test", Payload: []byte(`{"max_tokens":8,"messages":[{"role":"user","content":"fixture"}],"betas":["body-beta","caller-beta"]}`)}
			opts := executorapi.Options{SourceFormat: translation.FormatClaude, Headers: http.Header{"Anthropic-Beta": {"caller-beta,strip-beta"}}}
			if _, err := NewClaudeExecutor(cfg).Execute(ctx, a, req, opts); err != nil || calls != 1 {
				t.Fatalf("attempts=%d err=%v", calls, err)
			}
		})
	}
}
