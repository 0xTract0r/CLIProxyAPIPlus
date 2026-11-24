package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

type ampResponseNameExecutor struct {
	lastModel     string
	reportedModel string
}

func (*ampResponseNameExecutor) Identifier() string { return "amp-response-names-fixture" }

func (*ampResponseNameExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, fmt.Errorf("fixture does not implement count-tokens")
}

func (*ampResponseNameExecutor) Refresh(context.Context, *coreauth.Auth) (*coreauth.Auth, error) {
	return nil, fmt.Errorf("fixture has no upstream credentials")
}

func (*ampResponseNameExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("fixture does not permit network")
}

func (f *ampResponseNameExecutor) Execute(_ context.Context, _ *coreauth.Auth, req coreexecutor.Request, _ coreexecutor.Options) (coreexecutor.Response, error) {
	f.lastModel = gjson.GetBytes(req.Payload, "model").String()
	model := f.reportedModel
	if model == "" {
		model = f.lastModel
	}
	return coreexecutor.Response{Payload: []byte(fmt.Sprintf(`{"id":"completion-fixture","object":"chat.completion","model":%q,"choices":[],"seen_model":%q}`, model, f.lastModel))}, nil
}

func (f *ampResponseNameExecutor) ExecuteStream(_ context.Context, _ *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	f.lastModel = gjson.GetBytes(req.Payload, "model").String()
	chunks := []string{
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"type\":\"message\",\"id\":\"message-fixture\",\"content\":[]}}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_fixture\",\"name\":\"gl",
		"ob\",\"input\":{}}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"needle\\\":\\\"glob\\\"}\"}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":1}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	}
	if string(opts.SourceFormat) == "openai" {
		chunks = []string{
			fmt.Sprintf(`{"id":"chat-stream-fixture","object":"chat.completion.chunk","model":%q,"choices":[{"index":0,"delta":{"content":"first"}}]}`, f.lastModel),
			fmt.Sprintf(`{"id":"chat-stream-fixture","object":"chat.completion.chunk","model":%q,"choices":[{"index":0,"finish_reason":"stop","delta":{}}]}`, f.lastModel),
		}
	}
	data := make(chan coreexecutor.StreamChunk, len(chunks))
	for _, chunk := range chunks {
		data <- coreexecutor.StreamChunk{Payload: []byte(chunk)}
	}
	close(data)
	return &coreexecutor.StreamResult{Chunks: data}, nil
}

func TestOrdinaryAmpClientResponseNames(t *testing.T) {
	const routed = "amp-local/response-fixture"
	const alias = "amp-response-alias"
	const suffix = "(xhigh)"
	f := &ampResponseNameExecutor{}
	s := newAmpForwardingFixture(t, config.AmpCode{
		ModelMappings: []config.AmpModelMapping{{From: alias, To: routed}},
	})
	manager := s.handlers.AuthManager
	manager.RegisterExecutor(f)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID: f.Identifier(), Provider: f.Identifier(), Status: coreauth.StatusActive,
	}); err != nil {
		t.Fatal(err)
	}
	models := registry.GetGlobalRegistry()
	models.RegisterClient(f.Identifier(), f.Identifier(), []*registry.ModelInfo{{ID: routed, Type: "openai", Object: "model", OwnedBy: "fixture"}})
	t.Cleanup(func() { models.UnregisterClient(f.Identifier()) })

	call := func(path, body string) string {
		t.Helper()
		req := ampFixtureRequest(http.MethodPost, path, body, "127.0.0.1:5000", ampInboundFixture)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected normal response: HTTP %d %s", w.Code, w.Body.String())
		}
		return w.Body.String()
	}
	t.Run("client model", func(t *testing.T) {
		out := call("/api/provider/openai/v1/chat/completions", `{"model":"`+alias+suffix+`","messages":[]}`)
		if got := gjson.Get(out, "seen_model").String(); got != routed+suffix || f.lastModel != routed+suffix {
			t.Fatalf("execution model changed: payload=%q evidence=%q", f.lastModel, got)
		}
		if got := gjson.Get(out, "model").String(); got != alias+suffix {
			t.Fatalf("client model = %q, want explicit alias %q", got, alias+suffix)
		}
	})
	t.Run("client model in both stream chunks", func(t *testing.T) {
		out := call("/api/provider/openai/v1/chat/completions", `{"model":"`+alias+suffix+`","stream":true,"messages":[]}`)
		if strings.Count(out, `"model":"`+alias+suffix+`"`) != 2 || !strings.Contains(out, "data: [DONE]") ||
			!strings.Contains(out, `"finish_reason":"stop"`) || f.lastModel != routed+suffix {
			t.Fatalf("expected the declared alias in both ordinary stream chunks: %s", out)
		}
	})
	t.Run("different provider-reported model", func(t *testing.T) {
		f.reportedModel = "fixture-provider-model-revision"
		t.Cleanup(func() { f.reportedModel = "" })
		out := call("/api/provider/openai/v1/chat/completions", `{"model":"`+alias+suffix+`","messages":[]}`)
		if got := gjson.Get(out, "model").String(); got != f.reportedModel {
			t.Fatalf("unrelated reported model was rewritten: %q", got)
		}
	})
	t.Run("declared tool", func(t *testing.T) {
		out := call("/api/provider/anthropic/v1/messages", `{"model":"`+routed+`","stream":true,"messages":[],"tools":[{"name":"Glob","input_schema":{"type":"object"}}]}`)
		if !strings.Contains(out, `"name":"Glob"`) || !strings.Contains(out, `"id":"toolu_fixture"`) ||
			!strings.Contains(out, `"index":1`) || !strings.Contains(out, `{\"needle\":\"glob\"}`) || !strings.Contains(out, `event: message_stop`) {
			t.Fatalf("declared tool name, tool pairing or event data changed: %s", out)
		}
	})
	t.Run("ambiguous spelling", func(t *testing.T) {
		out := call("/api/provider/anthropic/v1/messages", `{"model":"`+routed+`","stream":true,"messages":[],"tools":[{"name":"Glob","input_schema":{"type":"object"}},{"name":"glob","input_schema":{"type":"object"}}]}`)
		if !strings.Contains(out, `"name":"glob"`) || strings.Contains(out, `"name":"Glob"`) {
			t.Fatal("a case collision changed the provider's tool spelling")
		}
	})
	t.Run("unknown returned nickname", func(t *testing.T) {
		out := call("/api/provider/anthropic/v1/messages", `{"model":"`+routed+`","stream":true,"messages":[],"tools":[{"name":"Find","input_schema":{"type":"object"}}]}`)
		if !strings.Contains(out, `"name":"glob"`) || strings.Contains(out, `"name":"Find"`) {
			t.Fatal("an unrelated tool was renamed")
		}
	})
	t.Run("normal non-Amp route", func(t *testing.T) {
		out := call("/v1/messages", `{"model":"`+routed+`","stream":true,"messages":[],"tools":[{"name":"Glob","input_schema":{"type":"object"}}]}`)
		if !strings.Contains(out, `"name":"glob"`) || strings.Contains(out, `"name":"Glob"`) {
			t.Fatal("normal non-Amp response changed")
		}
	})

}
