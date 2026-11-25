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

// Preserves the production-side rule that a user message can contain both a result and text.
func TestOrdinaryAntigravityMixedUserContentKeepsRoleAndText(t *testing.T) {
	payload := []byte(`{"request":{"contents":[{"role":"user","parts":[{"functionResponse":{"name":"run","response":{"result":"ok"}}},{"text":"user follow-up"}]}]}}`)
	out := ojson.GetBytes(normalizeAntigravityGeminiFunctionResponseRoles(payload), "request.contents.0")
	if out.Get("role").String() != "user" || out.Get("parts.0.functionResponse.name").String() != "run" || out.Get("parts.1.text").String() != "user follow-up" {
		t.Fatalf("mixed user content changed: %s", out.Raw)
	}
}

// Preserves the upstream countTokens assertion using an in-memory HTTP transport.
func TestOrdinaryAntigravityCountTokensStripsStatefulFields(t *testing.T) {
	calls := 0
	rt := ordinaryAntigravityRT(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Path != antigravityCountTokensPath {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		for _, field := range []string{"toolConfig", "labels", "sessionId", "safetySettings"} {
			if ojson.GetBytes(body, "request."+field).Exists() {
				t.Fatalf("countTokens retained %s", field)
			}
		}
		if got := ojson.GetBytes(body, "request.contents.0.parts.0.text").String(); got != "hello" {
			t.Fatalf("message text=%q", got)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"totalTokens":42}`)), Request: r}, nil
	})
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", rt)
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": "https://fixture.invalid"}, Metadata: map[string]any{"access_token": "fixture"}}
	req := cliproxyexecutor.Request{Model: "gemini-3.6-flash", Payload: []byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}],"toolConfig":{"functionCallingConfig":{"mode":"AUTO"}},"labels":{"source":"fixture"},"sessionId":"fixture-session","safetySettings":[{"category":"HARM_CATEGORY_HARASSMENT","threshold":"BLOCK_NONE"}]}`)}
	exec := NewAntigravityExecutor(&config.Config{})
	_, err := exec.CountTokens(ctx, auth, req, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatGemini, ResponseFormat: sdktranslator.FormatGemini})
	if err != nil || calls != 1 {
		t.Fatalf("CountTokens error=%v, calls=%d", err, calls)
	}
}

// Retains the upstream boundary rules for Gemini history and Claude targets.
func TestOrdinaryAntigravityGeminiBoundaryRoles(t *testing.T) {
	const model = `{"role":"model","parts":[{"text":"previous answer"}]}`
	const user = `{"role":"user","parts":[{"text":"question"}]}`
	for _, tc := range []struct {
		name, target, contents, roles string
	}{
		{"assistant first", "gemini-3.6-flash", model + "," + user, "user,model,user"},
		{"assistant last", "gemini-3.6-flash", user + "," + model, "user,model,user"},
		{"claude unchanged", "claude-sonnet", model + "," + user, "model,user"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := ojson.NewDocString(`{"request":{"contents":[` + tc.contents + `]}}`)
			ensureAntigravityGeminiBoundaryUserContentDoc(tc.target, doc)
			var roles []string
			for _, content := range doc.Get("request.contents").Array() {
				roles = append(roles, content.Get("role").String())
			}
			if got := strings.Join(roles, ","); got != tc.roles {
				t.Fatalf("roles=%s want %s", got, tc.roles)
			}
		})
	}
}

func TestOrdinaryAntigravityRoleOrdersMixedParallelResponses(t *testing.T) {
	payload := []byte(`{"request":{"contents":[{"role":"model","parts":[{"functionCall":{"id":"call-1","name":"read","args":{"file":"one"}}},{"functionCall":{"id":"call-2","name":"read","args":{"file":"two"}}}]},{"role":"user","parts":[{"text":"results follow"},{"functionResponse":{"id":"call-2","name":"read","response":{"result":"two"}}},{"text":"continue"},{"functionResponse":{"id":"call-1","name":"read","response":{"result":"one"}}}]}]}}`)
	if errValidate := internalsignature.ValidateGeminiFunctionCallPairing(payload); errValidate == nil {
		t.Fatal("permuted input unexpectedly passed function call pairing validation")
	}
	output := normalizeAntigravityGeminiFunctionResponseRoles(payload)
	if got := ojson.GetBytes(output, "request.contents.1.role").String(); got != "user" {
		t.Fatalf("mixed functionResponse/user content role = %q, want user; output=%s", got, output)
	}
	parts := ojson.GetBytes(output, "request.contents.1.parts").Array()
	if len(parts) != 4 {
		t.Fatalf("mixed response parts = %d, want 4; output=%s", len(parts), output)
	}
	if got := parts[0].Get("functionResponse.id").String(); got != "call-1" {
		t.Fatalf("first functionResponse.id = %q, want call-1; output=%s", got, output)
	}
	if got := parts[1].Get("functionResponse.id").String(); got != "call-2" {
		t.Fatalf("second functionResponse.id = %q, want call-2; output=%s", got, output)
	}
	if got := parts[2].Get("text").String(); got != "results follow" {
		t.Fatalf("first trailing text = %q; output=%s", got, output)
	}
	if got := parts[3].Get("text").String(); got != "continue" {
		t.Fatalf("second trailing text = %q; output=%s", got, output)
	}
	if errValidate := internalsignature.ValidateGeminiFunctionCallPairing(output); errValidate != nil {
		t.Fatalf("normalized mixed parallel responses are invalid: %v; output=%s", errValidate, output)
	}
}

func TestOrdinaryAntigravityRoleOrdersParallelResponses(t *testing.T) {
	payload := []byte(`{"request":{"contents":[{"role":"model","parts":[{"functionCall":{"id":"call-1","name":"read","args":{"file":"one"}}},{"functionCall":{"id":"call-2","name":"read","args":{"file":"two"}}}]},{"role":" Model ","parts":[{"functionResponse":{"id":"call-2","name":"read","response":{"result":"two"}}},{"functionResponse":{"id":"call-1","name":"read","response":{"result":"one"}}}]}]}}`)
	output := normalizeAntigravityGeminiFunctionResponseRoles(payload)
	if got := ojson.GetBytes(output, "request.contents.1.role").String(); got != "model" {
		t.Fatalf("functionResponse role = %q, want model; output=%s", got, output)
	}
	if got := ojson.GetBytes(output, "request.contents.1.parts.0.functionResponse.id").String(); got != "call-1" {
		t.Fatalf("first functionResponse.id = %q, want call-1; output=%s", got, output)
	}
	if got := ojson.GetBytes(output, "request.contents.1.parts.1.functionResponse.id").String(); got != "call-2" {
		t.Fatalf("second functionResponse.id = %q, want call-2; output=%s", got, output)
	}
	if errValidate := internalsignature.ValidateGeminiFunctionCallPairing(output); errValidate != nil {
		t.Fatalf("normalized parallel responses are invalid: %v; output=%s", errValidate, output)
	}
}

func TestOrdinaryAntigravityRoleDoesNotCrossEmptyContentBoundary(t *testing.T) {
	for _, boundary := range []string{
		`{"role":"user","parts":[]}`,
		`{"role":"user"}`,
		`{"role":"user","parts":null}`,
	} {
		payload := []byte(`{"request":{"contents":[{"role":"model","parts":[{"functionCall":{"id":"call-1","name":"read","args":{}}},{"functionCall":{"id":"call-2","name":"read","args":{}}}]},` + boundary + `,{"role":"user","parts":[{"functionResponse":{"id":"call-2","name":"read","response":{"result":"two"}}},{"functionResponse":{"id":"call-1","name":"read","response":{"result":"one"}}}]}]}}`)
		output := normalizeAntigravityGeminiFunctionResponseRoles(payload)
		if got := ojson.GetBytes(output, "request.contents.2.role").String(); got != "model" {
			t.Fatalf("pure functionResponse role = %q, want model; output=%s", got, output)
		}
		if got := ojson.GetBytes(output, "request.contents.2.parts.0.functionResponse.id").String(); got != "call-2" {
			t.Fatalf("response crossed content boundary %s and was reordered: first id=%q; output=%s", boundary, got, output)
		}
		if errValidate := internalsignature.ValidateGeminiFunctionCallPairing(output); errValidate == nil {
			t.Fatalf("responses crossing content boundary %s were accepted: %s", boundary, output)
		}
	}
}

func TestOrdinaryAntigravityCanonicalPathsNeverRetry(t *testing.T) {
	type call func(*AntigravityExecutor, context.Context, *cliproxyauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) error
	for _, route := range []struct {
		name, path string
		run        call
	}{
		{"generate", antigravityGeneratePath, func(e *AntigravityExecutor, c context.Context, a *cliproxyauth.Auth, r cliproxyexecutor.Request, o cliproxyexecutor.Options) error {
			_, err := e.Execute(c, a, r, o)
			return err
		}},
		{"stream", antigravityStreamPath, func(e *AntigravityExecutor, c context.Context, a *cliproxyauth.Auth, r cliproxyexecutor.Request, o cliproxyexecutor.Options) error {
			_, err := e.ExecuteStream(c, a, r, o)
			return err
		}},
		{"count", antigravityCountTokensPath, func(e *AntigravityExecutor, c context.Context, a *cliproxyauth.Auth, r cliproxyexecutor.Request, o cliproxyexecutor.Options) error {
			_, err := e.CountTokens(c, a, r, o)
			return err
		}},
	} {
		t.Run(route.name, func(t *testing.T) {
			for _, unavailable := range []bool{false, true} {
				calls := 0
				rt := ordinaryAntigravityRT(func(r *http.Request) (*http.Response, error) {
					calls++
					if r.URL.Host != "fixture.invalid" || r.URL.Path != route.path {
						t.Fatalf("route changed: %s", r.URL)
					}
					if unavailable {
						return nil, io.ErrUnexpectedEOF
					}
					return &http.Response{StatusCode: http.StatusTooManyRequests, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"fixture"}}`)), Request: r}, nil
				})
				ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", rt)
				auth := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": "https://fixture.invalid"}, Metadata: map[string]any{"access_token": "fixture", "project_id": "fixture"}}
				req := cliproxyexecutor.Request{Model: "gemini-3.6-flash", Payload: []byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`)}
				err := route.run(NewAntigravityExecutor(&config.Config{}), ctx, auth, req, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatGemini})
				if calls != 1 || err == nil {
					t.Fatalf("request count=%d error=%v", calls, err)
				}
				if unavailable && !errors.Is(err, io.ErrUnexpectedEOF) {
					t.Fatalf("expected original transport error: %v", err)
				}
				if !unavailable {
					var status interface{ StatusCode() int }
					if !errors.As(err, &status) || status.StatusCode() != 429 {
						t.Fatalf("expected original 429: %v", err)
					}
				}
			}
		})
	}
}

func TestOrdinaryAntigravityStreamBoundariesAndReadFailure(t *testing.T) {
	for _, tc := range []struct {
		name, payload, wantLast string
		num                     int
	}{
		{"trailing model", `{"contents":[{"role":"user","parts":[{"text":"question"}]},{"role":"model","parts":[{"text":"answer"}]}]}`, "user", 3},
		{"leading model", `{"contents":[{"role":"model","parts":[{"text":"answer"}]},{"role":"user","parts":[{"text":"followup"}]}]}`, "user", 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt := ordinaryAntigravityRT(func(r *http.Request) (*http.Response, error) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Fatal(err)
				}
				turns := ojson.GetBytes(body, "request.contents").Array()
				if len(turns) != tc.num || turns[0].Get("role").String() != "user" || turns[len(turns)-1].Get("role").String() != tc.wantLast {
					t.Fatalf("boundary turns=%v", turns)
				}
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: {\"response\":{\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"partial\"}]}}]}}\n\n")), Request: r}, nil
			})
			ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", rt)
			auth := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": "https://fixture.invalid"}, Metadata: map[string]any{"access_token": "fixture", "project_id": "fixture"}}
			req := cliproxyexecutor.Request{Model: "gemini-3.6-flash", Payload: []byte(tc.payload)}
			stream, err := NewAntigravityExecutor(&config.Config{}).ExecuteStream(ctx, auth, req, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatGemini, ResponseFormat: sdktranslator.FromString("antigravity")})
			if err != nil {
				t.Fatal(err)
			}
			var complete strings.Builder
			var terminalErr error
			for chunk := range stream.Chunks {
				complete.Write(chunk.Payload)
				if chunk.Err != nil {
					terminalErr = chunk.Err
				}
			}
			output := complete.String()
			if terminalErr == nil || !strings.Contains(output, "partial") || strings.Contains(output, "STOP") || strings.Contains(output, "[DONE]") {
				t.Fatalf("unexpected partial stream outcome error=%v, body=%s", terminalErr, output)
			}
		})
	}
}

func TestOrdinaryAntigravityTrailingToolResultBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, result string
		appendUser   bool
	}{
		{"image", `"parts":[{"inlineData":{"mimeType":"image/png","data":"AA=="}}]`, true},
		{"text", `"response":{"result":"ok"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := ojson.NewDocString(`{"request":{"contents":[{"role":"user","parts":[{"text":"show"}]},{"role":"model","parts":[{"functionResponse":{"name":"show",` + tc.result + `}}]}]}}`)
			ensureAntigravityGeminiBoundaryUserContentDoc("gemini-3.6-flash", doc)
			turns := doc.Get("request.contents").Array()
			want := 2
			if tc.appendUser {
				want = 3
			}
			if len(turns) != want || !turns[1].Get("parts.0.functionResponse").Exists() {
				t.Fatalf("unexpected turns %v", turns)
			}
			if tc.appendUser && turns[2].Get("role").String() != "user" {
				t.Fatalf("media turn must be followed by user: %v", turns)
			}
		})
	}
}

func TestOrdinaryAntigravityResponsesModelFirstGetsLeadingUser(t *testing.T) {
	rt := ordinaryAntigravityRT(func(r *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		first := ojson.GetBytes(body, "request.contents.0")
		second := ojson.GetBytes(body, "request.contents.1")
		if first.Get("role").String() != "user" || !first.Get("parts.0.text").Exists() || first.Get("parts.0.text").String() != "" || second.Get("role").String() != "model" || second.Get("parts.0.text").String() != "prior answer" {
			t.Fatalf("unexpected forward history: %s", body)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: {\"response\":{\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"fixture reply\"}]},\"finishReason\":\"STOP\"}]}}\n\n")), Request: r}, nil
	})
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", rt)
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": "https://fixture.invalid"}, Metadata: map[string]any{"access_token": "fixture", "project_id": "fixture"}}
	req := cliproxyexecutor.Request{Model: "gemini-3.7-flash", Payload: []byte(`{"input":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"prior answer"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"follow up"}]}]}`)}
	stream, err := NewAntigravityExecutor(&config.Config{}).ExecuteStream(ctx, auth, req, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, ResponseFormat: sdktranslator.FormatOpenAIResponse})
	if err != nil {
		t.Fatal(err)
	}
	for chunk := range stream.Chunks {
		if chunk.Err != nil {
			t.Fatal(chunk.Err)
		}
	}
}
