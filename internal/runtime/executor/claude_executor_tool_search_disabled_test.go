package executor

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	responsesclaude "github.com/router-for-me/CLIProxyAPI/v8/internal/translator/claude/openai/responses"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func toolSearchReloadContext(t *testing.T, flipTo bool) (context.Context, *[]byte) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	ginCtx, _ := gin.CreateTestContext(nil)
	ginCtx.Request = httptest_NewRequest()
	ginCtx.Request.Header = make(http.Header)
	var sent []byte
	transport := ordinaryClaudeTestRoundTripper(func(req *http.Request) (*http.Response, error) {
		body, errRead := io.ReadAll(req.Body)
		if errRead != nil {
			t.Fatal(errRead)
		}
		sent = body
		responsesclaude.SetClientToolSearchEnabled(flipTo)
		contentType := "application/json"
		response := `{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[{"type":"tool_use","id":"toolu_search","name":"tool_search","input":{"query":"calendar"}}],"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}}`
		if ojson.GetBytes(body, "stream").Bool() {
			contentType = "text/event-stream"
			response = strings.Join([]string{
				`event: message_start` + "\n" + `data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[],"stop_reason":null,"usage":{"input_tokens":1,"output_tokens":0}}}`,
				`event: content_block_start` + "\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_search","name":"tool_search","input":{}}}`,
				`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"query\":\"calendar\"}"}}`,
				`event: content_block_stop` + "\n" + `data: {"type":"content_block_stop","index":0}`,
				`event: message_delta` + "\n" + `data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":1}}`,
				`event: message_stop` + "\n" + `data: {"type":"message_stop"}`,
			}, "\n\n") + "\n\n"
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{contentType}}, Body: io.NopCloser(strings.NewReader(response)), Request: req}, nil
	})
	ctx := context.WithValue(context.Background(), "gin", ginCtx)
	return context.WithValue(ctx, "cliproxy.roundtripper", http.RoundTripper(transport)), &sent
}

// A config reload while a request is upstream must not change how that
// request's response is translated: the value seen when it started holds.
func TestClaudeExecutor_ToolSearchSwitchIsPinnedPerRequest(t *testing.T) {
	payload := []byte(nativeToolSearchResponsesPayload)
	auth := nativeToolSearchAuth(nil)
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, OriginalRequest: payload}
	for _, tc := range []struct {
		name      string
		start     bool
		wantItem  string
		wantTools bool
	}{
		{name: "on then reloaded off", start: true, wantItem: "tool_search_call", wantTools: true},
		{name: "off then reloaded on", start: false, wantItem: "function_call", wantTools: false},
	} {
		t.Run(tc.name+" execute", func(t *testing.T) {
			enableClaudeToolSearchForTest(t, tc.start)
			ctx, sent := toolSearchReloadContext(t, !tc.start)
			resp, errExecute := NewClaudeExecutor(&config.Config{}).Execute(ctx, auth, cliproxyexecutor.Request{Model: "claude-sonnet-4-5", Payload: payload}, opts)
			if errExecute != nil {
				t.Fatalf("Execute: %v", errExecute)
			}
			if got := ojson.GetBytes(*sent, `tools.#(name=="tool_search")`).Exists(); got != tc.wantTools {
				t.Fatalf("upstream tool_search declared = %v, want %v: %s", got, tc.wantTools, *sent)
			}
			if got := ojson.GetBytes(resp.Payload, "output.0.type").String(); got != tc.wantItem {
				t.Fatalf("output item = %s, want %s: %s", got, tc.wantItem, resp.Payload)
			}
		})
		t.Run(tc.name+" stream", func(t *testing.T) {
			enableClaudeToolSearchForTest(t, tc.start)
			ctx, sent := toolSearchReloadContext(t, !tc.start)
			result, errStream := NewClaudeExecutor(&config.Config{}).ExecuteStream(ctx, auth, cliproxyexecutor.Request{Model: "claude-sonnet-4-5", Payload: payload}, opts)
			if errStream != nil {
				t.Fatalf("ExecuteStream: %v", errStream)
			}
			var items []string
			for chunk := range result.Chunks {
				if chunk.Err != nil {
					t.Fatalf("stream chunk: %v", chunk.Err)
				}
				for _, line := range strings.Split(string(chunk.Payload), "\n") {
					data := ojson.Parse(strings.TrimPrefix(line, "data: "))
					if data.Get("type").String() == "response.output_item.done" {
						items = append(items, data.Get("item.type").String())
					}
				}
			}
			if got := ojson.GetBytes(*sent, `tools.#(name=="tool_search")`).Exists(); got != tc.wantTools {
				t.Fatalf("upstream tool_search declared = %v, want %v: %s", got, tc.wantTools, *sent)
			}
			if len(items) != 1 || items[0] != tc.wantItem {
				t.Fatalf("stream output items = %v, want [%s]", items, tc.wantItem)
			}
		})
	}
}

// A reload between the fallback decision and translation must not change the
// translation: it follows the value pinned when the fallback was decided.
func TestClaudeExecutor_ToolSearchFallbackPinsTranslation(t *testing.T) {
	hosted := `{"type":"tool_search","name":"HostedSearch"}`
	payload := []byte(strings.Replace(nativeToolSearchResponsesPayload, `],"stream":false}`, `,`+hosted+`],"stream":false}`, 1))
	official := nativeToolSearchAuth(nil)
	gateway := nativeToolSearchAuth(map[string]string{"base_url": nativeToolSearchGateway})
	for _, tc := range []struct {
		name          string
		start         bool
		auth          *cliproxyauth.Auth
		wantToolNames map[string]bool
	}{
		// Native: the client search tool stays and hosted search is dropped.
		{name: "on official then off", start: true, auth: official, wantToolNames: map[string]bool{"tool_search": true, "HostedSearch": false}},
		// Fallback strips client search; the pinned translation still drops hosted search.
		{name: "on gateway then off", start: true, auth: gateway, wantToolNames: map[string]bool{"tool_search": false, "HostedSearch": false}},
		// Dev: no client search tool and the named hosted declaration passes through.
		{name: "off official then on", start: false, auth: official, wantToolNames: map[string]bool{"tool_search": false, "HostedSearch": true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			enableClaudeToolSearchForTest(t, tc.start)
			ex := NewClaudeExecutor(&config.Config{})
			req := cliproxyexecutor.Request{Model: "claude-sonnet-4-5", Payload: payload}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, OriginalRequest: payload}
			ctx, req, _ := ex.applyNativeToolSearchFallback(context.Background(), tc.auth, sdktranslator.FormatOpenAIResponse, "claude-sonnet-4-5", req, opts)
			responsesclaude.SetClientToolSearchEnabled(!tc.start)
			body, errTranslate := helps.TranslateRequestReturningError(ctx, nil, &config.Config{}, sdktranslator.FormatOpenAIResponse, sdktranslator.FormatClaude, "claude-sonnet-4-5", req.Payload, false, false)
			if errTranslate != nil {
				t.Fatalf("translate: %v", errTranslate)
			}
			for name, want := range tc.wantToolNames {
				if got := ojson.GetBytes(body, `tools.#(name=="`+name+`")`).Exists(); got != want {
					t.Errorf("tool %s declared = %v, want %v: %s", name, got, want, ojson.GetBytes(body, "tools").Raw)
				}
			}
		})
	}
}
