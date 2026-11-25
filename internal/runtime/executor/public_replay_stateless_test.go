package executor

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestOrdinaryPublicGeminiNeverReplays(t *testing.T) {
	cfg := &config.Config{}
	type execute func(context.Context, *cliproxyauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error)
	for _, provider := range []struct {
		name string
		run  execute
	}{
		{"aistudio", nil},
		{"gemini", NewGeminiExecutor(cfg).Execute},
		{"vertex", NewGeminiVertexExecutor(cfg).Execute},
	} {
		t.Run(provider.name, func(t *testing.T) {
			for i, caller := range []string{"scope-a", "scope-b", ""} {
				payload := `{"contents":[{"role":"user","parts":[{"text":"question"}]},{"role":"model","parts":[{"text":"previous answer"}]},{"role":"user","parts":[{"text":"follow up"}]}]}`
				req := cliproxyexecutor.Request{Model: "gemini-3.7-flash", Payload: []byte(payload)}
				opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatGemini, Metadata: map[string]any{cliproxyexecutor.CallerScopeMetadataKey: caller}}
				if i < 2 {
					opts.Headers = http.Header{"X-Session-Id": {"shared-explicit-session"}}
				}
				assertBody := func(body []byte) {
					t.Helper()
					parts := ojson.GetBytes(body, "contents.1.parts").Array()
					if len(parts) != 1 || parts[0].Get("text").String() != "previous answer" || parts[0].Get("thoughtSignature").Exists() || strings.Contains(string(body), "provider-reply-signature") {
						t.Fatalf("ordinary input was replayed or modified: %s", body)
					}
				}
				if provider.run == nil {
					exec := NewAIStudioExecutor(cfg, "aistudio", nil)
					body, _, err := exec.translateRequest(t.Context(), req, opts, false)
					if err != nil {
						t.Fatal(err)
					}
					assertBody(body)
					continue
				}
				calls := 0
				rt := publicReplayFixtureRT(func(r *http.Request) (*http.Response, error) {
					calls++
					if r.URL.Host != "fixture.invalid" {
						t.Fatalf("unexpected host %s", r.URL.Host)
					}
					body, err := io.ReadAll(r.Body)
					if err != nil {
						t.Fatal(err)
					}
					assertBody(body)
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"candidates":[{"content":{"role":"model","parts":[{"text":"done","thoughtSignature":"provider-reply-signature"}]},"finishReason":"STOP"}]}`)), Request: r}, nil
				})
				ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", rt)
				auth := &cliproxyauth.Auth{ID: caller + provider.name, Attributes: map[string]string{"api_key": "fixture", "base_url": "https://fixture.invalid"}}
				if _, err := provider.run(ctx, auth, req, opts); err != nil {
					t.Fatal(err)
				}
				if calls != 1 {
					t.Fatalf("upstream calls = %d", calls)
				}
			}
		})
	}
}

type publicReplayFixtureRT func(*http.Request) (*http.Response, error)

func (f publicReplayFixtureRT) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestOrdinaryPublicGeminiSharedReplayNeverOpensScope(t *testing.T) {
	const body = `{"sessionId":"fixture-session","contents":[{"role":"user","parts":[{"text":"hello","thoughtSignature":"opaque-fixture"}]}]}`
	for _, source := range []sdktranslator.Format{sdktranslator.FormatGemini, sdktranslator.FormatOpenAI} {
		opts := cliproxyexecutor.Options{SourceFormat: source, Headers: http.Header{"X-Session-Id": {"explicit-fixture"}}, Metadata: map[string]any{cliproxyexecutor.CallerScopeMetadataKey: "fixture-scope"}}
		req := cliproxyexecutor.Request{Model: "gemini-3.7-flash", Payload: []byte(body)}
		doc := ojson.NewDoc(req.Payload)
		scope, saved, changed, err := prepareGeminiTopLevelReasoningReplayPayloadDoc(t.Context(), req.Model, req, opts, doc)
		if err != nil || scope.valid() || saved != nil || changed || string(doc.Bytes()) != body {
			t.Fatalf("public replay opened a scope or changed the request for %v", source)
		}
		if changed, err := prepareGeminiTopLevelCountTokensReasoningReplayPayloadDoc(t.Context(), req.Model, req, opts, doc); err != nil || changed || string(doc.Bytes()) != body {
			t.Fatalf("countTokens replay changed the request for %v", source)
		}
	}
}
