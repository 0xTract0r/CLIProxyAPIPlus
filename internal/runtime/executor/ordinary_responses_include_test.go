package executor

import (
	"context"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	_ "github.com/router-for-me/CLIProxyAPI/v8/internal/translator"
	codexresponses "github.com/router-for-me/CLIProxyAPI/v8/internal/translator/codex/openai/responses"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

const ordinaryIncludeFixturePayload = `{"model":"gpt-5","input":"synthetic","include":["unknown.value","output.encrypted_content","file_search_call.results","web_search_call.results","web_search_call.action.sources","web_search_call.results",17,"message.input_image.image_url","computer_call_output.output.image_url","code_interpreter_call.outputs","message.output_text.logprobs"]}`

func ordinaryIncludeFixtureFields(raw []byte) []string {
	var fields []string
	for _, item := range ojson.GetBytesNoCopy(raw, "include").Array() {
		fields = append(fields, item.String())
	}
	return fields
}

func TestOrdinaryResponsesIncludeFakeHTTP(t *testing.T) {
	legacy := []string{"reasoning.encrypted_content", "web_search_call.action.sources"}
	hosted := append(slices.Clone(legacy), "web_search_call.results")
	platform := append(slices.Clone(hosted), "file_search_call.results", "message.input_image.image_url", "computer_call_output.output.image_url", "code_interpreter_call.outputs", "message.output_text.logprobs")
	for _, c := range []struct {
		name, base, key, alt string
		stream               bool
		want                 []string
	}{
		{"platform nonstream", "https://api.fixture.invalid/v1", "fixture", "", false, platform},
		{"platform stream", "https://api.fixture.invalid/v1", "fixture", "", true, platform},
		{"hosted stream", "", "", "", true, hosted},
		{"hosted with key", "https://chatgpt.com/backend-api/codex", "fixture", "", true, hosted},
		{"custom access token", "https://api.fixture.invalid/v1", "", "", true, hosted},
		{"compact", "https://api.fixture.invalid/v1", "fixture", "responses/compact", false, legacy},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := NewCodexExecutor(&config.Config{})
			cred := &cliproxyauth.Auth{ID: "include-fixture", Attributes: map[string]string{"base_url": c.base, "api_key": c.key}, Metadata: map[string]any{"access_token": "fixture"}}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, Alt: c.alt}
			req := cliproxyexecutor.Request{Model: "gpt-5", Payload: []byte(ordinaryIncludeFixturePayload)}
			var got []string
			calls := 0
			rt := ordinaryCodexTestRoundTripper(func(r *http.Request) (*http.Response, error) {
				calls++
				raw, err := io.ReadAll(r.Body)
				if err != nil {
					return nil, err
				}
				got = ordinaryIncludeFixtureFields(raw)
				body := "data: " + `{"type":"response.completed","response":{"id":"fixture","object":"response","status":"completed","output":[]}}` + "\n\n"
				if c.alt != "" {
					body = `{"id":"fixture","object":"response.compaction","output":[]}`
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
			})
			ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", rt)
			if c.stream {
				out, err := e.ExecuteStream(ctx, cred, req, opts)
				if err != nil {
					t.Fatal(err)
				}
				for chunk := range out.Chunks {
					if chunk.Err != nil {
						t.Fatal(chunk.Err)
					}
				}
			} else if _, err := e.Execute(ctx, cred, req, opts); err != nil {
				t.Fatal(err)
			}
			if calls != 1 || !slices.Equal(got, c.want) {
				t.Fatalf("attempts=%d include=%v, want %v", calls, got, c.want)
			}
		})
	}
}

func TestOrdinaryResponsesIncludeFakeWebSocket(t *testing.T) {
	captured := make(chan []string, 1)
	server := newCodexPipeServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture" {
			return
		}
		upgrader := websocket.Upgrader{}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_, b, err := conn.ReadMessage()
		if err != nil {
			return
		}
		captured <- ordinaryIncludeFixtureFields(b)
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"fixture","object":"response","status":"completed","output":[]}}`))
	}))
	e := NewCodexWebsocketsExecutor(&config.Config{})
	e.directDialContext = server.dial
	cred := &cliproxyauth.Auth{ID: "include-fixture", ProxyURL: "direct", Attributes: map[string]string{"base_url": server.URL, "api_key": "fixture"}}
	ctx, cancel := context.WithTimeout(cliproxyexecutor.WithDownstreamWebsocket(t.Context()), 3*time.Second)
	defer cancel()
	out, err := e.ExecuteStream(ctx, cred, cliproxyexecutor.Request{Model: "gpt-5", Payload: []byte(ordinaryIncludeFixturePayload)}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse})
	if err != nil {
		t.Fatal(err)
	}
	for chunk := range out.Chunks {
		if chunk.Err != nil {
			t.Fatal(chunk.Err)
		}
	}
	select {
	case got := <-captured:
		want := []string{"reasoning.encrypted_content", "web_search_call.action.sources", "web_search_call.results", "file_search_call.results", "message.input_image.image_url", "computer_call_output.output.image_url", "code_interpreter_call.outputs", "message.output_text.logprobs"}
		if !slices.Equal(got, want) {
			t.Fatalf("include=%v want=%v", got, want)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func TestOrdinaryResponsesIncludeDoesNotBroadenConverterOrXAI(t *testing.T) {
	raw := []byte(ordinaryIncludeFixturePayload)
	want := []string{"reasoning.encrypted_content", "web_search_call.action.sources"}
	translated, err := codexresponses.ConvertOpenAIResponsesRequestToCodex("gpt-5", raw, true)
	if err != nil || !slices.Equal(ordinaryIncludeFixtureFields(translated), want) {
		t.Fatalf("shared converter include=%v err=%v", ordinaryIncludeFixtureFields(translated), err)
	}
	e := NewXAIExecutor(&config.Config{})
	for _, stream := range []bool{false, true} {
		prepared, err := e.prepareResponsesRequestDoc(t.Context(), cliproxyexecutor.Request{Model: "grok-4", Payload: raw}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse}, stream)
		if err != nil {
			t.Fatal(err)
		}
		if got := ordinaryIncludeFixtureFields(prepared.ensureDoc().Bytes()); !slices.Equal(got, want) {
			t.Errorf("xAI stream=%v include=%v want=%v", stream, got, want)
		}
	}
}

func TestOrdinaryResponsesIncludeXAIStandardHTTPWire(t *testing.T) {
	raw := []byte(ordinaryIncludeFixturePayload)
	want := []string{"reasoning.encrypted_content", "web_search_call.action.sources"}
	cred := &cliproxyauth.Auth{Provider: "xai", ID: "fixture", Attributes: map[string]string{
		"api_key": "fixture", "base_url": "https://xai.fixture.invalid/v1",
	}}
	calls := 0
	rt := ordinaryCodexTestRoundTripper(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Host != "xai.fixture.invalid" || r.URL.Path != "/v1/responses" ||
			r.Header.Get("Authorization") != "Bearer fixture" {
			t.Fatalf("xAI selected endpoint or credential changed")
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		if got := ordinaryIncludeFixtureFields(body); !slices.Equal(got, want) {
			t.Errorf("xAI include=%v want=%v", got, want)
		}
		return &http.Response{StatusCode: 400, Header: http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{"error":{"message":"test rejection"}}`)), Request: r}, nil
	})
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", rt)
	_, err := NewXAIExecutor(&config.Config{}).ExecuteStream(
		ctx, cred, cliproxyexecutor.Request{Model: "grok-4", Payload: raw},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse})
	if err == nil || calls != 1 {
		t.Fatalf("standard xAI fixture should get one terminal response, requests=%d err=%v", calls, err)
	}
}

func TestOrdinaryResponsesIncludeNeedsExplicitInboundRequest(t *testing.T) {
	req := cliproxyexecutor.Request{Model: "gpt-5", Payload: []byte(ordinaryIncludeFixturePayload)}
	cred := &cliproxyauth.Auth{ID: "include-fixture", Attributes: map[string]string{"base_url": "https://api.fixture.invalid/v1", "api_key": "fixture"}}
	e := NewCodexExecutor(&config.Config{})
	for _, source := range []string{`{"input":"fixture"}`, `{"input":"fixture","include":"web_search_call.results"}`, `{"input":"fixture","include":["unknown.value","output.encrypted_content"]}`} {
		body, err := e.prepareOrdinaryCodexBody(t.Context(), cred, req, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, OriginalRequest: []byte(source)}, true, false)
		if err != nil {
			t.Fatal(err)
		}
		if got := ordinaryIncludeFixtureFields(body); slices.Contains(got, "web_search_call.results") || slices.Contains(got, "file_search_call.results") || slices.Contains(got, "output.encrypted_content") {
			t.Fatalf("unrequested include was promoted: %v", got)
		}
	}
}
