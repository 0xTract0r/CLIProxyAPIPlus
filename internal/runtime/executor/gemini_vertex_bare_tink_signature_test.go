package executor

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	internalsignature "github.com/router-for-me/CLIProxyAPI/v8/internal/signature"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestGeminiVertexSanitizePreservesOpaqueServiceSignature(t *testing.T) {
	const input = `{"contents":[{"role":"user","parts":[{"text":"input"}]},{"role":"model","parts":[{"functionCall":{"name":"fixture","args":{}},"thoughtSignature":"fixture-opaque-from-service"}]}]}`
	out := internalsignature.SanitizeGeminiRequestThoughtSignatures([]byte(input), "contents")
	path := "contents.1.parts.0.thoughtSignature"
	if before, after := ojson.GetBytesNoCopy([]byte(input), path), ojson.GetBytesNoCopy(out, path); after.Raw != before.Raw {
		t.Fatal("sanitization modified a provider-supplied opaque field")
	}

	t.Run("final_outbound", func(t *testing.T) {
		// Reuse the existing fixture value without changing or printing its bytes.
		fixture := ojson.GetBytesNoCopy([]byte(input), path)
		if fixture.Type != ojson.String || fixture.String() == "" {
			t.Fatal("expected existing opaque fixture value")
		}
		assertOrdinaryGeminiFinalOutboundOpaque(t, fixture.Raw)
	})
}

func assertOrdinaryGeminiFinalOutboundOpaque(t *testing.T, existing string) {
	t.Helper()
	longValue := `"` + strings.Repeat("ordinary opaque fixture data | ", 384) + `"`
	unknown := `"ordinary unknown format: \u2603 \/ text"`
	for _, protocol := range []struct {
		name     string
		format   sdktranslator.Format
		provider string
		endpoint string
		response string
		paths    []string
		request  func(first string) string
	}{
		{
			name: "generate_content", format: sdktranslator.FormatGemini,
			provider: "gemini", endpoint: ":generateContent",
			response: `{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]}}]}`,
			paths:    []string{"contents.1.parts.0.thoughtSignature", "contents.1.parts.1.thoughtSignature", "contents.1.parts.2.thoughtSignature"},
			request: func(first string) string {
				return `{"contents":[{"role":"user","parts":[{"text":"fixture request"}]},{"role":"model","parts":[` +
					`{"text":"first","thoughtSignature":` + first + `},` +
					`{"text":"second","thoughtSignature":` + longValue + `},` +
					`{"text":"third","thoughtSignature":` + unknown + `}]}]}`
			},
		},
		{
			name: "interactions", format: sdktranslator.FormatInteractions,
			provider: "gemini-interactions", endpoint: "/interactions",
			response: `{"outputs":[{"type":"text","text":"ok"}]}`,
			paths:    []string{"input.1.signature", "input.2.signature", "input.3.signature"},
			request: func(first string) string {
				return `{"model":"gemini-2.5-flash","input":[{"type":"text","text":"fixture request"},` +
					`{"type":"thought","signature":` + first + `},` +
					`{"type":"thought","signature":` + longValue + `},` +
					`{"type":"thought","signature":` + unknown + `}]}`
			},
		},
	} {
		t.Run(protocol.name, func(t *testing.T) {
			executor := NewGeminiExecutor(&config.Config{})
			auth := &cliproxyauth.Auth{ID: "ordinary-fixture", Provider: protocol.provider,
				Attributes: map[string]string{"base_url": "https://provider.example.invalid", "api_key": "ordinary-test-fixture"}}
			opts := cliproxyexecutor.Options{SourceFormat: protocol.format}
			t.Run("opaque_bytes", func(t *testing.T) {
				body := []byte(protocol.request(existing))
				calls := 0
				ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", ordinaryGeminiSignatureTransport(func(req *http.Request) (*http.Response, error) {
					calls++
					if req.Method != http.MethodPost || !strings.HasSuffix(req.URL.Path, protocol.endpoint) {
						return nil, errors.New("unexpected executor route")
					}
					wire, err := io.ReadAll(req.Body)
					if err != nil {
						return nil, err
					}
					for _, path := range protocol.paths {
						before, after := ojson.GetBytesNoCopy(body, path), ojson.GetBytesNoCopy(wire, path)
						if before.Type != ojson.String || after.Type != ojson.String || before.Raw != after.Raw {
							return nil, errors.New("outbound opaque field bytes changed")
						}
					}
					return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header),
						Body: io.NopCloser(strings.NewReader(protocol.response)), Request: req}, nil
				}))
				_, err := executor.Execute(ctx, auth, cliproxyexecutor.Request{Model: "gemini-2.5-flash", Payload: body}, opts)
				if err != nil || calls != 1 {
					t.Fatalf("expected unchanged opaque fields and one fake transport call, got %d: %v", calls, err)
				}
			})
			t.Run("wrong_type", func(t *testing.T) {
				calls := 0
				ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", ordinaryGeminiSignatureTransport(func(*http.Request) (*http.Response, error) {
					calls++
					return nil, errors.New("wrong field type reached transport")
				}))
				body := []byte(protocol.request("7"))
				_, err := executor.Execute(ctx, auth, cliproxyexecutor.Request{Model: "gemini-2.5-flash", Payload: body}, opts)
				var status interface{ StatusCode() int }
				if !errors.As(err, &status) || status.StatusCode() != http.StatusBadRequest || calls != 0 {
					t.Fatalf("expected local 400 before transport, got %v and %d calls", err, calls)
				}
			})
		})
	}
}
