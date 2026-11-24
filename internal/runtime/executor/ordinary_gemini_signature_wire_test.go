package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/signature"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

type ordinaryGeminiSignatureTransport func(*http.Request) (*http.Response, error)

func (f ordinaryGeminiSignatureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestOrdinaryGeminiExecutorSignatureWire(t *testing.T) {
	const opaque = "issued-native-test-value=="
	cfg := &config.Config{}
	fixtures := []struct {
		name    string
		execute func(context.Context, *cliproxyauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error)
	}{
		{name: "Gemini", execute: NewGeminiExecutor(cfg).Execute},
		{name: "Vertex", execute: NewGeminiVertexExecutor(cfg).Execute},
	}
	for _, f := range fixtures {
		t.Run(f.name, func(t *testing.T) {
			calls := 0
			ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", ordinaryGeminiSignatureTransport(func(req *http.Request) (*http.Response, error) {
				calls++
				data, err := io.ReadAll(req.Body)
				if err != nil {
					return nil, err
				}
				got := ojson.GetBytes(data, "contents.1.parts.0.thoughtSignature").String()
				if got != opaque || ojson.GetBytes(data, "contents.1.parts.1.thoughtSignature").Exists() {
					return nil, fmt.Errorf("wire did not preserve issued signature and unsigned sibling")
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]}}]}`)), Request: req}, nil
			}))
			auth := &cliproxyauth.Auth{ID: "synthetic", Attributes: map[string]string{"api_key": "fake-api-key", "base_url": "https://provider.example.invalid"}}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatGemini}
			fixture := func(value string) cliproxyexecutor.Request {
				return cliproxyexecutor.Request{Model: "gemini-2.5-flash", Payload: []byte(fmt.Sprintf(`{"contents":[{"role":"user","parts":[{"text":"What's the weather?"}]},{"role":"model","parts":[{"functionCall":{"name":"weather","args":{}},"thoughtSignature":%q},{"functionCall":{"name":"forecast","args":{}}}]}]}`, value))}
			}
			if _, err := f.execute(ctx, auth, fixture(signature.GeminiSkipThoughtSignatureValidator), opts); err == nil || !strings.Contains(err.Error(), "invalid Gemini thought signature") || calls != 0 {
				t.Fatalf("rejected signature reached transport or missed local error, calls=%d err=%v", calls, err)
			}
			if _, err := f.execute(ctx, auth, fixture(opaque), opts); err != nil || calls != 1 {
				t.Fatalf("issued signature did not reach the fake HTTP transport exactly once, calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestOrdinaryAIStudioSignatureTranslatedRequest(t *testing.T) {
	e := NewAIStudioExecutor(&config.Config{}, "aistudio", nil)
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatGemini}
	const opaque = "issued-native-test-value=="
	fixture := func(value string) cliproxyexecutor.Request {
		return cliproxyexecutor.Request{Model: "gemini-2.5-flash", Payload: []byte(fmt.Sprintf(`{"contents":[{"role":"user","parts":[{"text":"What's the weather?"}]},{"role":"model","parts":[{"functionCall":{"name":"weather","args":{}},"thoughtSignature":%q}]}]}`, value))}
	}
	if _, _, err := e.translateRequest(context.Background(), fixture(signature.GeminiSkipThoughtSignatureValidator), opts, false); err == nil || !strings.Contains(err.Error(), "invalid Gemini thought signature") {
		t.Fatalf("expected a local rejection, got %v", err)
	}
	payload, _, err := e.translateRequest(context.Background(), fixture(opaque), opts, false)
	if err != nil || ojson.GetBytes(payload, "contents.1.parts.0.thoughtSignature").String() != opaque {
		t.Fatalf("issued signature did not survive ordinary translation: %v", err)
	}
}

func TestOrdinaryGeminiRejectsForeignSignatureBeforeTransport(t *testing.T) {
	if signature.DetectSignatureProvider(testClaudeCAISSample) != signature.SignatureProviderClaude {
		t.Fatal("existing foreign-format fixture did not classify as Claude")
	}
	e := NewGeminiExecutor(&config.Config{})
	for _, route := range []string{"execute", "stream", "count"} {
		t.Run(route, func(t *testing.T) {
			calls := 0
			ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", ordinaryGeminiSignatureTransport(func(*http.Request) (*http.Response, error) {
				calls++
				return nil, fmt.Errorf("invalid request must be rejected before transport")
			}))
			auth := &cliproxyauth.Auth{ID: "synthetic", Attributes: map[string]string{"api_key": "fake-api-key", "base_url": "https://provider.example.invalid"}}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatGemini}
			req := cliproxyexecutor.Request{Model: "gemini-2.5-flash", Payload: []byte(fmt.Sprintf(`{"contents":[{"role":"user","parts":[{"text":"hello"}]},{"role":"model","parts":[{"functionCall":{"name":"weather","args":{}},"thoughtSignature":%q}]}]}`, testClaudeCAISSample))}
			var err error
			switch route {
			case "execute":
				_, err = e.Execute(ctx, auth, req, opts)
			case "stream":
				_, err = e.ExecuteStream(ctx, auth, req, opts)
			case "count":
				_, err = e.CountTokens(ctx, auth, req, opts)
			}
			status, ok := err.(interface{ StatusCode() int })
			if !ok || status.StatusCode() != http.StatusBadRequest || calls != 0 {
				t.Fatalf("expected local 400 with zero transport calls, got %v, calls=%d", err, calls)
			}
		})
	}
}

func TestOrdinaryGeminiInteractionsRejectsForeignAndKeepsOpaque(t *testing.T) {
	for _, tc := range []struct {
		name   string
		wire   string
		reject bool
	}{
		{"foreign", fmt.Sprintf("%q", testClaudeCAISSample), true},
		{"wrong type", "7", true},
		{"opaque", `"issued-native-test-value=="`, false},
		{"unrecognized prefixed", `"claude#opaque-not-a-verified-envelope"`, false},
		{"ordinary UUID", `"eb432ffe-3494-454c-93b8-69bfeeac7d49"`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"input":[{"type":"thought","signature":` + tc.wire + `}]}`
			doc := ojson.NewDocString(body)
			err := signature.ValidateOutboundGeminiInteractionsSignatures(doc)
			if (err != nil) != tc.reject {
				t.Fatalf("unexpected outbound validation result: %v", err)
			}
			if doc.String() != body {
				t.Fatal("outbound validation changed input bytes")
			}
		})
	}
}
