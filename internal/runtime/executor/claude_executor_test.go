package executor

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"github.com/MoeForge/moego/ojson"
	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	_ "github.com/router-for-me/CLIProxyAPI/v8/internal/translator"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestClaudeOrdinaryBetaAndRetryMigration(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, duplicate := range []bool{false, true} {
			host := "api.anthropic.com"
			if duplicate {
				host = "gateway.invalid"
			}
			calls := 0
			rt := canonicalClaudeTestTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.Host != host {
					t.Fatalf("origin = %s", r.URL.Host)
				}
				if duplicate {
					if r.Header.Get("Authorization") != "Bearer fixture" || r.Header.Get("X-Api-Key") != "" {
						t.Fatal("configured Bearer auth mixed with API key")
					}
				} else if r.Header.Get("X-Api-Key") != "fixture" || r.Header.Get("Authorization") != "" {
					t.Fatal("configured API key mixed with Bearer auth")
				}
				if r.Header.Get("Anthropic-Beta") != "files-api-2025-04-14,context-1m-2025-08-07" {
					t.Fatalf("beta header=%q", r.Header.Get("Anthropic-Beta"))
				}
				body, err := io.ReadAll(r.Body)
				if err != nil || ojson.GetBytesNoCopy(body, "betas").Exists() {
					t.Fatalf("beta leaked to request body: %s, err=%v", body, err)
				}
				header := http.Header{"Retry-After": {"30"}, "Anthropic-Ratelimit-Unified-Reset": {"9999999999"}}
				if duplicate {
					header["Retry-After"] = []string{"30", "14400"}
				}
				return &http.Response{StatusCode: 429, Header: header, Body: io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"rate_limit_error","message":"too many requests"}}`)), Request: r}, nil
			})
			ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", rt)
			a := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": "https://" + host, "api_key": "fixture"}}
			if duplicate {
				delete(a.Attributes, "api_key")
				a.Metadata = map[string]any{"access_token": "fixture"}
			}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, Headers: http.Header{"Anthropic-Beta": {"files-api-2025-04-14,claude-code-fixture"}}}
			req := cliproxyexecutor.Request{Model: "claude-opus-5", Payload: []byte(`{"model":"claude-opus-5","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"betas":["context-1m-2025-08-07","files-api-2025-04-14","bad beta"]}`)}
			e := NewClaudeExecutor(&config.Config{})
			var err error
			if stream {
				_, err = e.ExecuteStream(ctx, a, req, opts)
			} else {
				_, err = e.Execute(ctx, a, req, opts)
			}
			var status statusErr
			if calls != 1 || !errors.As(err, &status) || status.StatusCode() != 429 || !strings.Contains(status.Error(), "too many requests") {
				t.Fatalf("calls=%d err=%v", calls, err)
			}
			if duplicate {
				if status.RetryAfter() != nil {
					t.Fatalf("ambiguous Retry-After accepted: %v", status.RetryAfter())
				}
			} else if status.RetryAfter() == nil || *status.RetryAfter() != 30*time.Second {
				t.Fatalf("Retry-After ignored: %v", status.RetryAfter())
			}
		}
	}
}

func TestClaudeOrdinaryModelAndKimiCountMigration(t *testing.T) {
	for _, name := range []string{"legacy first-party", "supported first-party", "legacy gateway", "kimi count"} {
		t.Run(name, func(t *testing.T) {
			model := "claude-haiku-4-5-20251001"
			base := "https://api.anthropic.com"
			messages := `[{"role":"user","content":"one"},{"role":"system","content":"keep me"},{"role":"user","content":"two"}]`
			if name == "supported first-party" {
				model = "claude-opus-5"
			}
			if name == "legacy gateway" {
				base = "https://gateway.invalid"
			}
			if name == "kimi count" {
				base = "https://gateway.invalid/v1"
				messages = `[{"role":"user","content":"Count me"}]`
			}
			auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "fixture", "base_url": base}}
			body := []byte(`{"model":"` + model + `","max_tokens":16,"system":"instructions","metadata":{"user_id":"keep"},"messages":` + messages + `,"tools":[{"name":"weather","input_schema":{"type":"object"}}]}`)
			req := cliproxyexecutor.Request{Model: model, Payload: body}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude}
			calls := 0
			rt := canonicalClaudeTestTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if name == "legacy first-party" {
					return nil, errors.New("legacy request reached upstream")
				}
				got, err := io.ReadAll(r.Body)
				if err != nil {
					t.Fatal(err)
				}
				reply := `{"type":"message","model":"` + model + `","content":[{"type":"text","text":"ok"}]}`
				if name == "kimi count" {
					if r.URL.Host != "gateway.invalid" || r.URL.Path != "/v1/messages/count_tokens" || ojson.GetBytesNoCopy(got, "tools.0.name").String() != "weather" || ojson.GetBytesNoCopy(got, "metadata.user_id").String() != "keep" || ojson.GetBytesNoCopy(got, "max_tokens").Exists() {
						t.Fatalf("Kimi native count request %s %s", r.URL, got)
					}
					reply = `{"input_tokens":26}`
				} else if ojson.GetBytesNoCopy(got, "messages.1.role").String() != "system" {
					t.Fatalf("system turn lost: %s", got)
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(reply)), Request: r}, nil
			})
			ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", rt)
			var got cliproxyexecutor.Response
			var err error
			if name == "kimi count" {
				got, err = NewKimiExecutor(&config.Config{}).CountTokens(ctx, auth, req, opts)
			} else {
				got, err = NewClaudeExecutor(&config.Config{}).Execute(ctx, auth, req, opts)
			}
			if name == "legacy first-party" {
				var status statusErr
				if calls != 0 || !errors.As(err, &status) || status.StatusCode() != 400 || !status.IsRequestScoped() {
					t.Fatalf("calls=%d err=%v", calls, err)
				}
			} else if calls != 1 || err != nil {
				t.Fatalf("calls=%d err=%v", calls, err)
			} else if name == "kimi count" && ojson.GetBytesNoCopy(got.Payload, "input_tokens").Int() != 26 {
				t.Fatalf("payload=%s", got.Payload)
			}
		})
	}
}

func TestEnforceCacheControlLimit_PreservesKeyOrderWhenModified(t *testing.T) {
	payload := []byte(`{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"u1","cache_control":{"type":"ephemeral"}},{"type":"text","text":"u2","cache_control":{"type":"ephemeral"}}]}],"tools":[{"name":"t1","cache_control":{"type":"ephemeral"}},{"name":"t2","cache_control":{"type":"ephemeral"}}],"system":[{"type":"text","text":"s1","cache_control":{"type":"ephemeral"}}]}`)

	out := enforceCacheControlLimit(payload, 4)

	if got := countCacheControls(out); got != 4 {
		t.Fatalf("cache_control count = %d, want 4", got)
	}
	if ojson.GetBytes(out, "tools.0.cache_control").Exists() {
		t.Fatalf("tools.0.cache_control should be removed first (non-last tool)")
	}

	outStr := string(out)
	idxModel := strings.Index(outStr, `"model"`)
	idxMessages := strings.Index(outStr, `"messages"`)
	idxTools := strings.Index(outStr, `"tools"`)
	idxSystem := strings.Index(outStr, `"system"`)
	if idxModel == -1 || idxMessages == -1 || idxTools == -1 || idxSystem == -1 {
		t.Fatalf("failed to locate top-level keys in output: %s", outStr)
	}
	if !(idxModel < idxMessages && idxMessages < idxTools && idxTools < idxSystem) {
		t.Fatalf("top-level key order changed:\noriginal: %s\ngot:      %s", payload, out)
	}
}

func TestEnforceCacheControlLimit_ReservesThreadMarker(t *testing.T) {
	payload := []byte(`{
		"thread": {"type":"create"},
		"tools": [{"name":"t1","cache_control":{"type":"ephemeral"}}],
		"system": [
			{"type":"text","text":"s1","cache_control":{"type":"ephemeral"}},
			{"type":"text","text":"s2","cache_control":{"type":"ephemeral"}}
		],
		"messages": [{"role":"user","content":[
			{"type":"text","text":"u1","cache_control":{"type":"ephemeral"}},
			{"type":"text","text":"u2","cache_control":{"type":"ephemeral"}}
		]}]
	}`)

	out := enforceCacheControlLimit(payload, 4)

	if got := countCacheControls(out); got != 3 {
		t.Fatalf("cache_control count = %d, want 3 when thread is present", got)
	}
}

func TestEnforceCacheControlLimit_StripsNonLastToolBeforeMessages(t *testing.T) {
	payload := []byte(`{
		"tools": [
			{"name":"t1","cache_control":{"type":"ephemeral"}},
			{"name":"t2","cache_control":{"type":"ephemeral"}}
		],
		"system": [{"type":"text","text":"s1","cache_control":{"type":"ephemeral"}}],
		"messages": [
			{"role":"user","content":[{"type":"text","text":"u1","cache_control":{"type":"ephemeral"}}]},
			{"role":"user","content":[{"type":"text","text":"u2","cache_control":{"type":"ephemeral"}}]}
		]
	}`)

	out := enforceCacheControlLimit(payload, 4)

	if got := countCacheControls(out); got != 4 {
		t.Fatalf("cache_control count = %d, want 4", got)
	}
	if ojson.GetBytes(out, "tools.0.cache_control").Exists() {
		t.Fatalf("tools.0.cache_control should be removed first (non-last tool)")
	}
	if !ojson.GetBytes(out, "tools.1.cache_control").Exists() {
		t.Fatalf("tools.1.cache_control (last tool) should be preserved")
	}
	if !ojson.GetBytes(out, "messages.0.content.0.cache_control").Exists() || !ojson.GetBytes(out, "messages.1.content.0.cache_control").Exists() {
		t.Fatalf("message cache_control blocks should be preserved when non-last tool removal is enough")
	}
}

func TestEnforceCacheControlLimit_ToolOnlyPayloadStillRespectsLimit(t *testing.T) {
	payload := []byte(`{
		"tools": [
			{"name":"t1","cache_control":{"type":"ephemeral"}},
			{"name":"t2","cache_control":{"type":"ephemeral"}},
			{"name":"t3","cache_control":{"type":"ephemeral"}},
			{"name":"t4","cache_control":{"type":"ephemeral"}},
			{"name":"t5","cache_control":{"type":"ephemeral"}}
		]
	}`)

	out := enforceCacheControlLimit(payload, 4)

	if got := countCacheControls(out); got != 4 {
		t.Fatalf("cache_control count = %d, want 4", got)
	}
	if ojson.GetBytes(out, "tools.0.cache_control").Exists() {
		t.Fatalf("tools.0.cache_control should be removed to satisfy max=4")
	}
	if !ojson.GetBytes(out, "tools.4.cache_control").Exists() {
		t.Fatalf("last tool cache_control should be preserved when possible")
	}
}

func TestEnsureModelMaxTokens_DefaultsMissingValue(t *testing.T) {
	reg := registry.GetGlobalRegistry()
	clientID := "test-claude-default-max-tokens-client"
	modelID := "test-claude-default-max-tokens-model"
	reg.RegisterClient(clientID, "claude", []*registry.ModelInfo{{
		ID:          modelID,
		Type:        "claude",
		OwnedBy:     "anthropic",
		Object:      "model",
		Created:     time.Now().Unix(),
		UserDefined: true,
	}})
	defer reg.UnregisterClient(clientID)

	input := []byte(`{"model":"test-claude-default-max-tokens-model","messages":[{"role":"user","content":"hi"}]}`)
	out := ensureModelMaxTokens(input, modelID)

	if got := ojson.GetBytes(out, "max_tokens").Int(); got != defaultModelMaxTokens {
		t.Fatalf("max_tokens = %d, want %d", got, defaultModelMaxTokens)
	}
}

func TestEnsureModelMaxTokens_PreservesExplicitValue(t *testing.T) {
	reg := registry.GetGlobalRegistry()
	clientID := "test-claude-preserve-max-tokens-client"
	modelID := "test-claude-preserve-max-tokens-model"
	reg.RegisterClient(clientID, "claude", []*registry.ModelInfo{{
		ID:                  modelID,
		Type:                "claude",
		OwnedBy:             "anthropic",
		Object:              "model",
		Created:             time.Now().Unix(),
		MaxCompletionTokens: 4096,
		UserDefined:         true,
	}})
	defer reg.UnregisterClient(clientID)

	input := []byte(`{"model":"test-claude-preserve-max-tokens-model","max_tokens":2048,"messages":[{"role":"user","content":"hi"}]}`)
	out := ensureModelMaxTokens(input, modelID)

	if got := ojson.GetBytes(out, "max_tokens").Int(); got != 2048 {
		t.Fatalf("max_tokens = %d, want %d", got, 2048)
	}
}

func TestEnsureModelMaxTokens_SkipsUnregisteredModel(t *testing.T) {
	input := []byte(`{"model":"test-claude-unregistered-model","messages":[{"role":"user","content":"hi"}]}`)
	out := ensureModelMaxTokens(input, "test-claude-unregistered-model")

	if ojson.GetBytes(out, "max_tokens").Exists() {
		t.Fatalf("max_tokens should remain unset, got %s", ojson.GetBytes(out, "max_tokens").Raw)
	}
}

func TestEnsureModelMaxTokens_UsesRegisteredMaxCompletionTokens(t *testing.T) {
	reg := registry.GetGlobalRegistry()
	clientID := "test-claude-max-completion-tokens-client"
	modelID := "test-claude-max-completion-tokens-model"
	reg.RegisterClient(clientID, "claude", []*registry.ModelInfo{{
		ID:                  modelID,
		Type:                "claude",
		OwnedBy:             "anthropic",
		Object:              "model",
		Created:             time.Now().Unix(),
		MaxCompletionTokens: 4096,
		UserDefined:         true,
	}})
	defer reg.UnregisterClient(clientID)

	input := []byte(`{"model":"test-claude-max-completion-tokens-model","messages":[{"role":"user","content":"hi"}]}`)
	out := ensureModelMaxTokens(input, modelID)

	if got := ojson.GetBytes(out, "max_tokens").Int(); got != 4096 {
		t.Fatalf("max_tokens = %d, want %d", got, 4096)
	}
}

func TestNormalizeCacheControlTTL_DowngradesLaterOneHourBlocks(t *testing.T) {
	payload := []byte(`{
		"tools": [{"name":"t1","cache_control":{"type":"ephemeral","ttl":"1h"}}],
		"system": [{"type":"text","text":"s1","cache_control":{"type":"ephemeral"}}],
		"messages": [{"role":"user","content":[{"type":"text","text":"u1","cache_control":{"type":"ephemeral","ttl":"1h"}}]}]
	}`)

	out := normalizeCacheControlTTL(payload)

	if got := ojson.GetBytes(out, "tools.0.cache_control.ttl").String(); got != "1h" {
		t.Fatalf("tools.0.cache_control.ttl = %q, want %q", got, "1h")
	}
	if ojson.GetBytes(out, "messages.0.content.0.cache_control.ttl").Exists() {
		t.Fatalf("messages.0.content.0.cache_control.ttl should be removed after a default-5m block")
	}
}

func TestNormalizeCacheControlTTL_PreservesKeyOrderWhenModified(t *testing.T) {
	payload := []byte(`{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"u1","cache_control":{"type":"ephemeral","ttl":"1h"}}]}],"tools":[{"name":"t1","cache_control":{"type":"ephemeral"}}],"system":[{"type":"text","text":"s1","cache_control":{"type":"ephemeral"}}]}`)

	out := normalizeCacheControlTTL(payload)

	if ojson.GetBytes(out, "messages.0.content.0.cache_control.ttl").Exists() {
		t.Fatalf("messages.0.content.0.cache_control.ttl should be removed after a default-5m block")
	}

	outStr := string(out)
	idxModel := strings.Index(outStr, `"model"`)
	idxMessages := strings.Index(outStr, `"messages"`)
	idxTools := strings.Index(outStr, `"tools"`)
	idxSystem := strings.Index(outStr, `"system"`)
	if idxModel == -1 || idxMessages == -1 || idxTools == -1 || idxSystem == -1 {
		t.Fatalf("failed to locate top-level keys in output: %s", outStr)
	}
	if !(idxModel < idxMessages && idxMessages < idxTools && idxTools < idxSystem) {
		t.Fatalf("top-level key order changed:\noriginal: %s\ngot:      %s", payload, out)
	}
}

func TestNormalizeCacheControlTTL_PreservesOriginalBytesWhenNoChange(t *testing.T) {
	// Payload where no TTL normalization is needed (all blocks use 1h with no
	// preceding 5m block). The text intentionally contains HTML chars (<, >, &)
	// that json.Marshal would escape to \u003c etc., altering byte identity.
	payload := []byte(`{"tools":[{"name":"t1","cache_control":{"type":"ephemeral","ttl":"1h"}}],"system":[{"type":"text","text":"<system-reminder>foo & bar</system-reminder>","cache_control":{"type":"ephemeral","ttl":"1h"}}],"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]}`)

	out := normalizeCacheControlTTL(payload)

	if !bytes.Equal(out, payload) {
		t.Fatalf("normalizeCacheControlTTL altered bytes when no change was needed.\noriginal: %s\ngot:      %s", payload, out)
	}
}

func assertStatusErr(t *testing.T, err error, want int) {
	t.Helper()

	status, ok := err.(interface{ StatusCode() int })
	if !ok {
		t.Fatalf("error %T does not expose StatusCode", err)
	}
	if got := status.StatusCode(); got != want {
		t.Fatalf("StatusCode() = %d, want %d", got, want)
	}
}

func executeClaudeContextManagementRequest(t *testing.T, cfg *config.Config, payload []byte, stream bool) []byte {
	t.Helper()

	var upstreamBody []byte
	transport := canonicalClaudeTestTransport(func(req *http.Request) (*http.Response, error) {
		var errRead error
		upstreamBody, errRead = io.ReadAll(req.Body)
		if errRead != nil {
			t.Fatal(errRead)
		}
		contentType := "application/json"
		responseBody := `{"id":"msg_test","type":"message","role":"assistant","model":"claude-opus-5","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
		if stream {
			contentType = "text/event-stream"
			responseBody = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_test\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-opus-5\",\"content\":[],\"stop_reason\":null,\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{contentType}},
			Body:       io.NopCloser(strings.NewReader(responseBody)),
			Request:    req,
		}, nil
	})
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", http.RoundTripper(transport))
	executor := NewClaudeExecutor(cfg)
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "key-payload-rule"}}
	request := cliproxyexecutor.Request{Model: "claude-opus-5", Payload: payload}
	options := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude}

	if stream {
		result, errStream := executor.ExecuteStream(ctx, auth, request, options)
		if errStream != nil {
			t.Fatalf("ExecuteStream() error = %v", errStream)
		}
		for chunk := range result.Chunks {
			if chunk.Err != nil {
				t.Fatalf("stream chunk error = %v", chunk.Err)
			}
		}
		return upstreamBody
	}
	if _, errExecute := executor.Execute(ctx, auth, request, options); errExecute != nil {
		t.Fatalf("Execute() error = %v", errExecute)
	}
	return upstreamBody
}

func TestClaudeExecutor_CountTokensCountsLocallyWithoutUpstreamRequest(t *testing.T) {
	payload := []byte(`{
		"system":"client system instructions",
		"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]
	}`)
	const expectedCount int64 = 7

	testCases := []struct {
		name   string
		apiKey string
	}{
		{name: "custom API key", apiKey: "key-123"},
		{name: "custom OAuth", apiKey: "sk-ant-oat-custom"},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", canonicalClaudeTestTransport(func(r *http.Request) (*http.Response, error) {
				t.Errorf("unexpected upstream call: %s", r.URL)
				return nil, errors.New("unexpected upstream call")
			}))

			executor := NewClaudeExecutor(&config.Config{})
			auth := &cliproxyauth.Auth{Attributes: map[string]string{
				"api_key":  testCase.apiKey,
				"base_url": "https://local-estimation.invalid",
			}}
			resp, errCount := executor.CountTokens(ctx, auth, cliproxyexecutor.Request{
				Model:   "claude-sonnet-4-5",
				Payload: payload,
			}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")})
			if errCount != nil {
				t.Fatalf("CountTokens() error = %v", errCount)
			}
			if got := ojson.GetBytes(resp.Payload, "input_tokens").Int(); got != expectedCount {
				t.Fatalf("input_tokens = %d, want %d; payload = %s", got, expectedCount, resp.Payload)
			}
		})
	}

	executor := NewClaudeExecutor(&config.Config{})
	resp, err := executor.CountTokens(context.Background(), nil, cliproxyexecutor.Request{
		Model:   "claude-sonnet-4-5",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FormatClaude,
		ResponseFormat: sdktranslator.FormatGemini,
	})
	if err != nil {
		t.Fatalf("CountTokens() Gemini response error = %v", err)
	}
	if got := ojson.GetBytes(resp.Payload, "totalTokens").Int(); got != expectedCount {
		t.Fatalf("Gemini totalTokens = %d, want %d; payload = %s", got, expectedCount, resp.Payload)
	}
	if got := ojson.GetBytes(resp.Payload, "promptTokensDetails.0.tokenCount").Int(); got != expectedCount {
		t.Fatalf("Gemini prompt token detail = %d, want %d; payload = %s", got, expectedCount, resp.Payload)
	}
}

func TestClaudeExecutor_CountTokensRejectsInvalidRequests(t *testing.T) {
	testCases := []struct {
		name    string
		payload string
	}{
		{name: "invalid JSON", payload: `not-json`},
		{name: "non-object", payload: `[]`},
		{name: "missing messages", payload: `{}`},
		{name: "empty messages", payload: `{"messages":[]}`},
		{name: "non-array messages", payload: `{"messages":"invalid"}`},
		{name: "invalid role", payload: `{"messages":[{"role":"system","content":"hello"}]}`},
		{name: "invalid content", payload: `{"messages":[{"role":"user","content":42}]}`},
		{name: "non-object content block", payload: `{"messages":[{"role":"user","content":[42]}]}`},
		{name: "untyped content block", payload: `{"messages":[{"role":"user","content":[{"text":"hello"}]}]}`},
	}

	executor := NewClaudeExecutor(&config.Config{})
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := executor.CountTokens(context.Background(), nil, cliproxyexecutor.Request{
				Model:   "claude-sonnet-4-5",
				Payload: []byte(testCase.payload),
			}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude})
			assertStatusErr(t, err, http.StatusBadRequest)
			requestErr, ok := err.(cliproxyexecutor.RequestScopedError)
			if !ok || !requestErr.IsRequestScoped() {
				t.Fatalf("error %T is not request-scoped", err)
			}
		})
	}
}

func TestDecodeResponseBodyStackedRepeatedHeaders(t *testing.T) {
	payload := []byte("stacked Claude response")
	var gzipOutput bytes.Buffer
	gzipWriter := gzip.NewWriter(&gzipOutput)
	if _, errWrite := gzipWriter.Write(payload); errWrite != nil {
		t.Fatal(errWrite)
	}
	if errClose := gzipWriter.Close(); errClose != nil {
		t.Fatal(errClose)
	}
	var brotliOutput bytes.Buffer
	brotliWriter := brotli.NewWriter(&brotliOutput)
	if _, errWrite := brotliWriter.Write(gzipOutput.Bytes()); errWrite != nil {
		t.Fatal(errWrite)
	}
	if errClose := brotliWriter.Close(); errClose != nil {
		t.Fatal(errClose)
	}

	header := make(http.Header)
	header.Add("Content-Encoding", "gzip")
	header.Add("Content-Encoding", "br")
	decoded, errDecode := decodeResponseBody(io.NopCloser(bytes.NewReader(brotliOutput.Bytes())), claudeResponseContentEncoding(header))
	if errDecode != nil {
		t.Fatal(errDecode)
	}
	defer decoded.Close()
	got, errRead := io.ReadAll(decoded)
	if errRead != nil {
		t.Fatal(errRead)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("decoded body = %q, want %q", got, payload)
	}
}

func TestDecodeResponseBody_MagicByteGzipNoHeader(t *testing.T) {
	const plaintext = "data: {\"type\":\"message_stop\"}\n"

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	_, _ = gz.Write([]byte(plaintext))
	_ = gz.Close()

	rc := io.NopCloser(&buf)
	decoded, err := decodeResponseBody(rc, "")
	if err != nil {
		t.Fatalf("decodeResponseBody error: %v", err)
	}
	defer decoded.Close()

	got, err := io.ReadAll(decoded)
	if err != nil {
		t.Fatalf("ReadAll error: %v", err)
	}
	if string(got) != plaintext {
		t.Errorf("decoded = %q, want %q", got, plaintext)
	}
}

func TestDecodeResponseBody_MagicByteZstdNoHeader(t *testing.T) {
	const plaintext = "data: {\"type\":\"message_stop\"}\n"

	var buf bytes.Buffer
	enc, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatalf("zstd.NewWriter: %v", err)
	}
	_, _ = enc.Write([]byte(plaintext))
	_ = enc.Close()

	rc := io.NopCloser(&buf)
	decoded, err := decodeResponseBody(rc, "")
	if err != nil {
		t.Fatalf("decodeResponseBody error: %v", err)
	}
	defer decoded.Close()

	got, err := io.ReadAll(decoded)
	if err != nil {
		t.Fatalf("ReadAll error: %v", err)
	}
	if string(got) != plaintext {
		t.Errorf("decoded = %q, want %q", got, plaintext)
	}
}

func TestDecodeResponseBody_PlainTextNoHeader(t *testing.T) {
	const plaintext = "data: {\"type\":\"message_stop\"}\n"
	rc := io.NopCloser(strings.NewReader(plaintext))
	decoded, err := decodeResponseBody(rc, "")
	if err != nil {
		t.Fatalf("decodeResponseBody error: %v", err)
	}
	defer decoded.Close()

	got, err := io.ReadAll(decoded)
	if err != nil {
		t.Fatalf("ReadAll error: %v", err)
	}
	if string(got) != plaintext {
		t.Errorf("decoded = %q, want %q", got, plaintext)
	}
}

type limitsOrdinaryRT func(*http.Request) (*http.Response, error)

func (f limitsOrdinaryRT) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestClaudeExecutor_ChatCompletionsDefaultMaxTokensAndPayloadDefaultRule(t *testing.T) {
	var seenBody []byte
	transport := limitsOrdinaryRT(func(req *http.Request) (*http.Response, error) {
		seenBody, _ = io.ReadAll(req.Body)
		contentType := "application/json"
		payload := `{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5-5","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":1}}`
		if req.Header.Get("Accept") == "text/event-stream" {
			contentType = "text/event-stream"
			payload = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-opus-5-5\",\"content\":[],\"usage\":{\"input_tokens\":10,\"output_tokens\":1}}}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(payload)), Request: req}, nil
	})
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", transport)

	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"api_key":  "sk-ant-api-test",
			"base_url": "https://claude-fixture.invalid",
		},
	}

	t.Run("omitted max_tokens uses registry 128k limit", func(t *testing.T) {
		executor := NewClaudeExecutor(&config.Config{})
		payload := []byte(`{"model":"claude-opus-5-5","reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`)
		_, errExec := executor.Execute(ctx, auth, cliproxyexecutor.Request{
			Model:   "claude-opus-5-5",
			Payload: payload,
		}, cliproxyexecutor.Options{
			SourceFormat:    sdktranslator.FormatOpenAI,
			ResponseFormat:  sdktranslator.FormatOpenAI,
			OriginalRequest: payload,
		})
		if errExec != nil {
			t.Fatalf("Execute() error = %v", errExec)
		}
		if got := ojson.GetBytes(seenBody, "max_tokens").Int(); got != 128000 {
			t.Fatalf("Execute max_tokens = %d, want 128000; body=%s", got, seenBody)
		}
	})

	t.Run("payload.default applies when chat completions omits max_tokens and preserves explicit client limit", func(t *testing.T) {
		executor := NewClaudeExecutor(&config.Config{
			Payload: config.PayloadConfig{
				Default: []config.PayloadRule{{
					Models: []config.PayloadModelRule{{Name: "claude-opus-5-5", Protocol: "claude"}},
					Params: map[string]any{"max_tokens": 16384},
				}},
			},
		})

		omittedPayload := []byte(`{"model":"claude-opus-5-5","messages":[{"role":"user","content":"hi"}]}`)
		_, errExec := executor.Execute(ctx, auth, cliproxyexecutor.Request{
			Model:   "claude-opus-5-5",
			Payload: omittedPayload,
		}, cliproxyexecutor.Options{
			SourceFormat:    sdktranslator.FormatOpenAI,
			ResponseFormat:  sdktranslator.FormatOpenAI,
			OriginalRequest: omittedPayload,
		})
		if errExec != nil {
			t.Fatalf("Execute() omitted error = %v", errExec)
		}
		if got := ojson.GetBytes(seenBody, "max_tokens").Int(); got != 16384 {
			t.Fatalf("Execute omitted max_tokens = %d, want payload.default 16384; body=%s", got, seenBody)
		}

		streamResult, errStream := executor.ExecuteStream(ctx, auth, cliproxyexecutor.Request{
			Model:   "claude-opus-5-5",
			Payload: omittedPayload,
		}, cliproxyexecutor.Options{
			SourceFormat:    sdktranslator.FormatOpenAI,
			ResponseFormat:  sdktranslator.FormatOpenAI,
			OriginalRequest: omittedPayload,
			Stream:          true,
		})
		if errStream != nil {
			t.Fatalf("ExecuteStream() omitted error = %v", errStream)
		}
		for chunk := range streamResult.Chunks {
			if chunk.Err != nil {
				t.Fatalf("ExecuteStream() chunk error = %v", chunk.Err)
			}
		}
		if got := ojson.GetBytes(seenBody, "max_tokens").Int(); got != 16384 {
			t.Fatalf("ExecuteStream omitted max_tokens = %d, want payload.default 16384; body=%s", got, seenBody)
		}

		protocolCases := []struct {
			name      string
			format    sdktranslator.Format
			payload   string
			wantLimit int64
		}{
			{
				name:      "chat completions explicit max_completion_tokens preserved",
				format:    sdktranslator.FormatOpenAI,
				payload:   `{"model":"claude-opus-5-5","max_completion_tokens":4096,"messages":[{"role":"user","content":"hi"}]}`,
				wantLimit: 4096,
			},
			{
				name:      "responses omitted max_output_tokens uses payload.default",
				format:    sdktranslator.FormatOpenAIResponse,
				payload:   `{"model":"claude-opus-5-5","input":"hi"}`,
				wantLimit: 16384,
			},
			{
				name:      "responses explicit max_output_tokens preserved",
				format:    sdktranslator.FormatOpenAIResponse,
				payload:   `{"model":"claude-opus-5-5","max_output_tokens":4096,"input":"hi"}`,
				wantLimit: 4096,
			},
			{
				name:      "gemini omitted maxOutputTokens uses payload.default",
				format:    sdktranslator.FormatGemini,
				payload:   `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`,
				wantLimit: 16384,
			},
			{
				name:      "gemini explicit maxOutputTokens preserved",
				format:    sdktranslator.FormatGemini,
				payload:   `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"maxOutputTokens":4096}}`,
				wantLimit: 4096,
			},
		}

		for _, pc := range protocolCases {
			raw := []byte(pc.payload)
			_, errCase := executor.Execute(ctx, auth, cliproxyexecutor.Request{
				Model:   "claude-opus-5-5",
				Payload: raw,
			}, cliproxyexecutor.Options{
				SourceFormat:    pc.format,
				ResponseFormat:  pc.format,
				OriginalRequest: raw,
			})
			if errCase != nil {
				t.Fatalf("%s: Execute() error = %v", pc.name, errCase)
			}
			if got := ojson.GetBytes(seenBody, "max_tokens").Int(); got != pc.wantLimit {
				t.Fatalf("%s: max_tokens = %d, want %d; body=%s", pc.name, got, pc.wantLimit, seenBody)
			}
		}
	})
}

func TestClaudeExecutor_ExecuteStreamOpenAIResponseTranslatesCacheAndTrailingUsageChunk(t *testing.T) {
	upstreamStream := "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_123","type":"message","role":"assistant","content":[],"model":"claude-opus-5","stop_reason":null,"usage":{"input_tokens":2095,"cache_creation_input_tokens":7185,"cache_read_input_tokens":355598,"output_tokens":1}}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":15}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"

	transport := limitsOrdinaryRT(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(upstreamStream)), Request: req}, nil
	})
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", transport)

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":  "key-123",
		"base_url": "https://claude-fixture.invalid",
	}}
	payload := []byte(`{"model":"claude-opus-5","messages":[{"role":"user","content":"hello"}],"stream":true}`)

	result, err := executor.ExecuteStream(ctx, auth, cliproxyexecutor.Request{
		Model:   "claude-opus-5",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FormatOpenAI,
		ResponseFormat: sdktranslator.FormatOpenAI,
	})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}

	var allChunks [][]byte
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("unexpected chunk error: %v", chunk.Err)
		}
		allChunks = append(allChunks, chunk.Payload)
	}

	var trailingUsageChunk []byte
	for _, chunk := range allChunks {
		choices := ojson.GetBytes(chunk, "choices")
		if choices.Exists() && len(choices.Array()) == 0 && ojson.GetBytes(chunk, "usage").Exists() {
			trailingUsageChunk = chunk
			break
		}
	}

	if trailingUsageChunk == nil {
		t.Fatalf("expected trailing usage chunk with choices: [], got: %s", string(bytes.Join(allChunks, []byte("\n"))))
	}

	if gotCached := ojson.GetBytes(trailingUsageChunk, "usage.prompt_tokens_details.cached_tokens").Int(); gotCached != 355598 {
		t.Errorf("cached_tokens = %d, want 355598", gotCached)
	}
	if gotWrite := ojson.GetBytes(trailingUsageChunk, "usage.prompt_tokens_details.cache_write_tokens").Int(); gotWrite != 7185 {
		t.Errorf("cache_write_tokens = %d, want 7185", gotWrite)
	}
	if gotCreation := ojson.GetBytes(trailingUsageChunk, "usage.prompt_tokens_details.cached_creation_tokens").Int(); gotCreation != 7185 {
		t.Errorf("cached_creation_tokens = %d, want 7185", gotCreation)
	}
	if gotOutput := ojson.GetBytes(trailingUsageChunk, "usage.completion_tokens").Int(); gotOutput != 15 {
		t.Errorf("completion_tokens = %d, want 15", gotOutput)
	}
}

func TestShouldUseClaudeUpstreamTokenCount(t *testing.T) {
	tests := []struct {
		name    string
		apiKey  string
		baseURL string
		want    bool
	}{
		{name: "official OAuth", apiKey: "sk-ant-oat-official", baseURL: "https://api.anthropic.com", want: true},
		{name: "official API key", apiKey: "key-official", baseURL: "https://api.anthropic.com:443", want: true},
		{name: "custom OAuth", apiKey: "sk-ant-oat-custom", baseURL: "https://gateway.example"},
		{name: "custom API key", apiKey: "key-custom", baseURL: "https://gateway.example"},
		{name: "lookalike host", apiKey: "sk-ant-oat-lookalike", baseURL: "https://api.anthropic.com.example"},
		{name: "insecure official host", apiKey: "sk-ant-oat-http", baseURL: "http://api.anthropic.com"},
		{name: "missing credential", baseURL: "https://api.anthropic.com"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := shouldUseClaudeUpstreamTokenCount(test.apiKey, test.baseURL); got != test.want {
				t.Fatalf("shouldUseClaudeUpstreamTokenCount() = %v, want %v", got, test.want)
			}
		})
	}
}

type canonicalClaudeTestTransport func(*http.Request) (*http.Response, error)

func (f canonicalClaudeTestTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func sanitizeClaudeWebSearchDomains(body []byte) []byte {
	tools := ojson.GetBytesNoCopy(body, "tools")
	if !tools.Exists() || !tools.IsArray() {
		return body
	}
	doc := ojson.NewDoc(body)
	if !sanitizeClaudeWebSearchDomainsDoc(doc) {
		return body
	}
	return doc.Bytes()
}

func TestSanitizeClaudeWebSearchDomains(t *testing.T) {
	// Mirrors the litellm payload from issue #2681: a non-empty allowed_domains
	// alongside an empty blocked_domains, which Anthropic rejects as ambiguous.
	input := []byte(`{"tools":[{"type":"web_search_20250305","name":"web_search","allowed_domains":["anthropic.com"],"blocked_domains":[],"max_uses":8}]}`)
	out := sanitizeClaudeWebSearchDomains(input)

	if ojson.GetBytes(out, "tools.0.blocked_domains").Exists() {
		t.Fatalf("empty blocked_domains should be removed: %s", string(out))
	}
	if got := ojson.GetBytes(out, "tools.0.allowed_domains").Array(); len(got) != 1 || got[0].String() != "anthropic.com" {
		t.Fatalf("non-empty allowed_domains should be preserved: %s", string(out))
	}
	if got := ojson.GetBytes(out, "tools.0.max_uses").Int(); got != 8 {
		t.Fatalf("max_uses should be preserved: got %d", got)
	}
}

func TestSanitizeClaudeWebSearchDomains_LeavesNonBuiltinAndNonEmpty(t *testing.T) {
	// Empty arrays on non-web_search tools must be left untouched.
	input := []byte(`{"tools":[{"type":"custom","name":"x","blocked_domains":[]},{"type":"web_search_20250305","name":"web_search","blocked_domains":["evil.com"]}]}`)
	out := sanitizeClaudeWebSearchDomains(input)

	if !ojson.GetBytes(out, "tools.0.blocked_domains").Exists() {
		t.Fatalf("non-web_search tool fields should be untouched: %s", string(out))
	}
	if got := ojson.GetBytes(out, "tools.1.blocked_domains").Array(); len(got) != 1 || got[0].String() != "evil.com" {
		t.Fatalf("non-empty blocked_domains should be preserved: %s", string(out))
	}
}

func TestOrdinaryClaudeBuiltinSearchDomainsOnWire(t *testing.T) {
	calls := 0
	transport := canonicalClaudeTestTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		if ojson.GetBytes(body, "tools.0.blocked_domains").Exists() ||
			ojson.GetBytes(body, "tools.0.allowed_domains.0").String() != "example.test" ||
			!ojson.GetBytes(body, "tools.1.blocked_domains").Exists() {
			t.Fatalf("unexpected tool domains %s", body)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("{}")), Request: r}, nil
	})
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", transport)
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": "https://fixture.invalid", "api_key": "fixture"}}
	body := []byte(`{"tools":[{"type":"web_search_20250305","name":"web_search","allowed_domains":["example.test"],"blocked_domains":[]},{"name":"custom","blocked_domains":[]}]}`)
	resp, err := NewClaudeExecutor(&config.Config{}).sendOrdinaryClaudeRequest(ctx, auth, "/v1/messages", body, false, nil, nil)
	if err != nil || calls != 1 {
		t.Fatalf("send err=%v calls=%d", err, calls)
	}
	resp.Body.Close()
}

func TestClaudeExecutor_ExecuteOpenAINonStreamRejectsEmptyClaudeStream(t *testing.T) {
	_, err := executeOpenAIChatCompletionThroughClaude(t, "")
	if err == nil {
		t.Fatal("Execute error = nil, want empty stream error")
	}
	assertOrdinaryClaudeSSEStatus(t, err, http.StatusBadGateway)
	if !strings.Contains(err.Error(), "empty stream response") {
		t.Fatalf("Execute error = %q, want empty stream response", err.Error())
	}
}

func TestClaudeExecutor_ExecuteOpenAINonStreamRejectsClaudeErrorEvent(t *testing.T) {
	body := `data: {"type":"error","error":{"type":"overloaded_error","message":"upstream overloaded"}}` + "\n"
	_, err := executeOpenAIChatCompletionThroughClaude(t, body)
	if err == nil {
		t.Fatal("Execute error = nil, want upstream error event")
	}
	assertOrdinaryClaudeSSEStatus(t, err, http.StatusBadGateway)
	if !strings.Contains(err.Error(), "upstream overloaded") {
		t.Fatalf("Execute error = %q, want upstream overloaded", err.Error())
	}
}

func TestClaudeExecutor_ExecuteOpenAINonStreamRejectsIncompleteClaudeStream(t *testing.T) {
	body := strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"msg_123","model":"claude-3-5-sonnet-20241022"}}`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	_, err := executeOpenAIChatCompletionThroughClaude(t, body)
	if err == nil {
		t.Fatal("Execute error = nil, want incomplete stream error")
	}
	assertOrdinaryClaudeSSEStatus(t, err, http.StatusBadGateway)
	if !strings.Contains(err.Error(), "ended before message completion") {
		t.Fatalf("Execute error = %q, want incomplete stream error", err.Error())
	}
}

func TestClaudeExecutor_ExecuteOpenAINonStreamConvertsValidClaudeStream(t *testing.T) {
	body := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_123","model":"claude-3-5-sonnet-20241022"}}`,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":2,"output_tokens":1}}`,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	resp, err := executeOpenAIChatCompletionThroughClaude(t, body)
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if got := ojson.GetBytes(resp.Payload, "id").String(); got != "msg_123" {
		t.Fatalf("response id = %q, want msg_123; payload=%s", got, string(resp.Payload))
	}
	if got := ojson.GetBytes(resp.Payload, "model").String(); got != "claude-3-5-sonnet-20241022" {
		t.Fatalf("response model = %q, want claude-3-5-sonnet-20241022", got)
	}
	if got := ojson.GetBytes(resp.Payload, "choices.0.message.content").String(); got != "ok" {
		t.Fatalf("response content = %q, want ok", got)
	}
	if got := ojson.GetBytes(resp.Payload, "usage.total_tokens").Int(); got != 3 {
		t.Fatalf("usage.total_tokens = %d, want 3", got)
	}
}

func executeOpenAIChatCompletionThroughClaude(t *testing.T, upstreamBody string) (cliproxyexecutor.Response, error) {
	t.Helper()
	hits := 0
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", canonicalClaudeTestTransport(func(r *http.Request) (*http.Response, error) {
		hits++
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		if !ojson.GetBytes(body, "stream").Bool() || r.Header.Get("Accept") != "text/event-stream" {
			t.Fatalf("wrong response negotiation: %s %s", r.Header.Get("Accept"), body)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream; charset=utf-8"}}, Body: io.NopCloser(strings.NewReader(upstreamBody)), Request: r}, nil
	}))
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": "https://fixture.invalid", "api_key": "fixture"}}
	req := cliproxyexecutor.Request{Model: "claude-3-5-sonnet-20241022", Payload: []byte(`{"model":"claude-3-5-sonnet-20241022","messages":[{"role":"user","content":"hi"}]}`)}
	out, err := NewClaudeExecutor(&config.Config{}).Execute(ctx, auth, req, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI})
	if hits != 1 {
		t.Fatalf("upstream requests=%d", hits)
	}
	return out, err
}

func assertOrdinaryClaudeSSEStatus(t *testing.T, err error, want int) {
	t.Helper()
	var status interface{ StatusCode() int }
	if !errors.As(err, &status) || status.StatusCode() != want {
		t.Fatalf("error=%v want status %d", err, want)
	}
}

func TestOrdinaryClaudeSamplingAtExecute(t *testing.T) {
	for _, tc := range []struct{ thinking, forced bool }{{}, {thinking: true}, {thinking: true, forced: true}} {
		active := tc.thinking && !tc.forced
		ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", canonicalClaudeTestTransport(func(r *http.Request) (*http.Response, error) {
			data, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatal(err)
			}
			if ojson.GetBytes(data, "temperature").Exists() || ojson.GetBytes(data, "top_p").Exists() || ojson.GetBytes(data, "top_k").Exists() == active || (tc.forced && ojson.GetBytes(data, "thinking").Exists()) {
				t.Fatalf("wrong active=%t sampling shape %s", active, data)
			}
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"msg_ordinary","model":"claude-3-5-sonnet-20241022","content":[{"type":"text","text":"ok"}]}`)), Request: r}, nil
		}))
		auth := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": "https://fixture.invalid", "api_key": "fixture"}}
		extra := ""
		if tc.thinking {
			extra = `,"thinking":{"type":"enabled","budget_tokens":1024}`
		}
		if tc.forced {
			extra += `,"tools":[{"name":"read","input_schema":{"type":"object"}}],"tool_choice":{"type":"any"}`
		}
		req := cliproxyexecutor.Request{Model: "claude-3-5-sonnet-20241022", Payload: []byte(`{"model":"claude-3-5-sonnet-20241022","max_tokens":2048,"temperature":0.3,"top_p":0.7,"top_k":32,"messages":[{"role":"user","content":"hi"}]` + extra + "}")}
		if _, err := NewClaudeExecutor(&config.Config{}).Execute(ctx, auth, req, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestClaudeExecutor_ExecuteOpenAINonStreamRequiresMessageStop(t *testing.T) {
	body := "data: {\"type\":\"message_start\",\"message\":{\"id\":\"fixture\",\"model\":\"claude-3-5-sonnet-20241022\"}}\n\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n"
	_, err := executeOpenAIChatCompletionThroughClaude(t, body)
	assertOrdinaryClaudeSSEStatus(t, err, http.StatusBadGateway)
}

func TestClaudeExecutor_ExecuteOpenAINonStreamRetainsPolicyRefusal(t *testing.T) {
	body := "data: {\"type\":\"message_start\",\"message\":{\"id\":\"fixture\",\"model\":\"claude-3-5-sonnet-20241022\"}}\n\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"refusal\",\"stop_details\":{\"type\":\"refusal\",\"explanation\":\"fixture refusal\"}}}\n\n" +
		"data: {\"type\":\"message_stop\"}\n\n"
	_, err := executeOpenAIChatCompletionThroughClaude(t, body)
	assertOrdinaryClaudeSSEStatus(t, err, http.StatusBadRequest)
	if !strings.Contains(err.Error(), "fixture refusal") {
		t.Fatal(err)
	}
}

func TestOrdinaryClaudeResponseFormatDeterminesWireMode(t *testing.T) {
	for _, from := range []sdktranslator.Format{sdktranslator.FormatClaude, sdktranslator.FormatOpenAI} {
		for _, to := range []sdktranslator.Format{sdktranslator.FormatClaude, sdktranslator.FormatOpenAI} {
			t.Run(from.String()+"_to_"+to.String(), func(t *testing.T) {
				const model = "claude-3-5-sonnet-20241022"
				stream := to == sdktranslator.FormatOpenAI
				calls := 0
				ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", canonicalClaudeTestTransport(func(r *http.Request) (*http.Response, error) {
					calls++
					data, err := io.ReadAll(r.Body)
					if err != nil {
						t.Fatal(err)
					}
					if ojson.GetBytes(data, "stream").Bool() != stream {
						t.Fatalf("wire mode for %v: %s", to, data)
					}
					contentType := "application/json"
					body := `{"id":"fixture","model":"claude-3-5-sonnet-20241022","content":[{"type":"text","text":"reply"}],"stop_reason":"end_turn"}`
					if stream {
						contentType = "text/event-stream"
						body = "data: {\"type\":\"message_start\",\"message\":{\"id\":\"fixture\",\"model\":\"claude-3-5-sonnet-20241022\"}}\n\n" +
							"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"reply\"}}\n\n" +
							"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n" +
							"data: {\"type\":\"message_stop\"}\n\n"
					}
					if r.Header.Get("Accept") != contentType {
						t.Fatalf("Accept=%s want %s", r.Header.Get("Accept"), contentType)
					}
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
				}))
				auth := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": "https://fixture.invalid", "api_key": "fixture"}}
				cfg := &config.Config{Payload: config.PayloadConfig{Override: []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: model, Protocol: "claude"}}, Params: map[string]any{"stream": !stream}}}}}
				req := cliproxyexecutor.Request{Model: model, Payload: []byte(`{"model":"claude-3-5-sonnet-20241022","messages":[{"role":"user","content":"hi"}]}`)}
				out, err := NewClaudeExecutor(cfg).Execute(ctx, auth, req, cliproxyexecutor.Options{SourceFormat: from, ResponseFormat: to})
				if err != nil || calls != 1 {
					t.Fatalf("Execute calls=%d error=%v", calls, err)
				}
				path := "content.0.text"
				if stream {
					path = "choices.0.message.content"
				}
				if got := ojson.GetBytes(out.Payload, path).String(); got != "reply" || out.Headers.Get("Content-Type") != "application/json" {
					t.Fatalf("converted content=%q, Content-Type=%s", got, out.Headers.Get("Content-Type"))
				}
			})
		}
	}
}

func TestClaudeOrdinaryRemoteCountProtocol(t *testing.T) {
	for _, tc := range []struct {
		name, body, encoding string
		status, want         int
		detail               string
	}{
		{"valid", `{"input_tokens":42}`, "", 200, 0, ""},
		{"invalid gzip error", "fixture broken gzip", "gzip", 400, 400, "failed to decode error response body"},
		{"negative", `{"input_tokens":-1}`, "", 200, 502, "invalid Claude token count response"},
		{"fractional", `{"input_tokens":2.5}`, "", 200, 502, "invalid Claude token count response"},
		{"wrong type", `{"input_tokens":"42"}`, "", 200, 502, "invalid Claude token count response"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			rt := canonicalClaudeTestTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				payload, err := io.ReadAll(r.Body)
				if err != nil {
					t.Fatal(err)
				}
				if r.URL.Host != "api.anthropic.com" || r.URL.Path != "/v1/messages/count_tokens" {
					t.Fatalf("unexpected count endpoint %s", r.URL)
				}
				if countCacheControls(payload) > 4 || ojson.GetBytes(payload, "stream").Exists() || ojson.GetBytes(payload, "max_tokens").Exists() || canonicalClaudeTTLOrderingViolation(payload) {
					t.Fatalf("count guard not applied: %s", payload)
				}
				h := http.Header{"Content-Type": {"application/json"}}
				if tc.encoding != "" {
					h.Set("Content-Encoding", tc.encoding)
				}
				return &http.Response{StatusCode: tc.status, Header: h, Body: io.NopCloser(strings.NewReader(tc.body)), Request: r}, nil
			})
			auth := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": "https://api.anthropic.com", "api_key": "fixture"}}
			ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", rt)
			req := cliproxyexecutor.Request{Model: "claude-3-5-haiku-20241022", Payload: []byte(`{"max_tokens":16,"tools":[{"name":"t","input_schema":{"type":"object"},"cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":[{"type":"text","text":"1","cache_control":{"type":"ephemeral"}},{"type":"text","text":"2","cache_control":{"type":"ephemeral","ttl":"1h"}},{"type":"text","text":"3","cache_control":{"type":"ephemeral","ttl":"1h"}},{"type":"text","text":"4","cache_control":{"type":"ephemeral","ttl":"1h"}},{"type":"text","text":"5","cache_control":{"type":"ephemeral","ttl":"1h"}}]}]}`)}
			out, err := NewClaudeExecutor(&config.Config{}).CountTokens(ctx, auth, req, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude})
			if calls != 1 {
				t.Fatalf("count calls=%d", calls)
			}
			if tc.want == 0 {
				if err != nil || ojson.GetBytes(out.Payload, "input_tokens").Int() != 42 {
					t.Fatalf("count got %s, err=%v", out.Payload, err)
				}
				return
			}
			assertOrdinaryClaudeSSEStatus(t, err, tc.want)
			if !strings.Contains(err.Error(), tc.detail) {
				t.Fatal(err)
			}
		})
	}
}

func canonicalClaudeTTLOrderingViolation(payload []byte) bool {
	seen5m := false
	violates := false

	checkCC := func(cc ojson.Result) {
		if !cc.Exists() || violates {
			return
		}
		ttl := cc.Get("ttl").String()
		if ttl != "1h" {
			seen5m = true
			return
		}
		if seen5m {
			violates = true
		}
	}

	tools := ojson.GetBytes(payload, "tools")
	if tools.IsArray() {
		tools.ForEach(func(_, tool ojson.Result) bool {
			checkCC(tool.Get("cache_control"))
			return !violates
		})
	}

	system := ojson.GetBytes(payload, "system")
	if system.IsArray() {
		system.ForEach(func(_, item ojson.Result) bool {
			checkCC(item.Get("cache_control"))
			return !violates
		})
	}

	messages := ojson.GetBytes(payload, "messages")
	if messages.IsArray() {
		messages.ForEach(func(_, msg ojson.Result) bool {
			content := msg.Get("content")
			if content.IsArray() {
				content.ForEach(func(_, item ojson.Result) bool {
					checkCC(item.Get("cache_control"))
					return !violates
				})
			}
			return !violates
		})
	}

	return violates
}

func TestOrdinaryClaudeToolSystemWireOrder(t *testing.T) {
	const body = `{"model":"claude-sonnet-5","system":[{"type":"text","text":"first","cache_control":{"type":"ephemeral"}},{"type":"text","text":"second"}],"tools":[{"name":"Lookup_2","input_schema":{"type":"object","properties":{}}}],"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"toolu_safe","name":"Lookup_2","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_safe","content":"fixture"},{"type":"text","text":"next"}]}]}`
	e := NewClaudeExecutor(&config.Config{})
	for _, stream := range []bool{false, true} {
		out, err := e.prepareOrdinaryClaudeMessages(context.Background(), nil, cliproxyexecutor.Request{Model: "claude-sonnet-5", Payload: []byte(body)}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude}, stream, false, "claude-sonnet-5", "claude-sonnet-5")
		if err != nil {
			t.Fatal(err)
		}
		for path, want := range map[string]string{"system.0.text": "first", "system.1.text": "second", "system.0.cache_control.type": "ephemeral", "tools.0.name": "Lookup_2", "messages.0.content.0.id": "toolu_safe", "messages.0.content.0.name": "Lookup_2", "messages.1.content.0.tool_use_id": "toolu_safe", "messages.1.content.1.text": "next"} {
			if got := ojson.GetBytesNoCopy(out, path).String(); got != want {
				t.Errorf("stream=%t %s = %q, want %q", stream, path, got, want)
			}
		}
	}
}

func TestOrdinaryClaudeSystemTextBoundary(t *testing.T) {
	e := NewClaudeExecutor(&config.Config{})
	for _, tc := range []struct {
		system  string
		allowed bool
	}{
		{`"literal p<&>rompt"`, true}, {`""`, true},
		{`[{"type":"text","text":"fixture"}]`, true},
		{`[{"type":"image","source":{}}]`, false},
		{`[{"type":"text","text":4}]`, false},
	} {
		req := cliproxyexecutor.Request{Model: "claude-sonnet-5", Payload: []byte(`{"model":"claude-sonnet-5","messages":[],"system":` + tc.system + `}`)}
		out, err := e.prepareOrdinaryClaudeMessages(t.Context(), nil, req, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude}, false, false, "claude-sonnet-5", "claude-sonnet-5")
		if tc.allowed && (err != nil || ojson.GetBytesNoCopy(out, "system").Raw != tc.system) || !tc.allowed && err == nil {
			t.Errorf("system %s: error=%v output=%s", tc.system, err, out)
		}
	}
}

func TestOrdinaryClaudePayloadRulesPreserveCallerOwnership(t *testing.T) {
	for _, mode := range []string{"disabled", "enabled"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", mode, stream), func(t *testing.T) {
				const model = "claude-opus-5"
				ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", canonicalClaudeTestTransport(func(r *http.Request) (*http.Response, error) {
					b, err := io.ReadAll(r.Body)
					if err != nil {
						t.Fatal(err)
					}
					if ojson.GetBytes(b, "thinking.type").String() != mode || ojson.GetBytes(b, "context_management.edits.0.type").String() != "caller_owned" {
						t.Fatalf("ordinary coefficients lost: %s", b)
					}
					body := `{"id":"fixture","model":"claude-opus-5","content":[{"type":"text","text":"ok"}]}`
					header := http.Header{"Content-Type": {"application/json"}}
					if stream {
						body = "data: {\"type\":\"message_stop\"}\n\n"
						header.Set("Content-Type", "text/event-stream")
					}
					return &http.Response{StatusCode: 200, Header: header, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
				}))
				auth := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": "https://fixture.invalid", "api_key": "fixture"}}
				cfg := &config.Config{Payload: config.PayloadConfig{Override: []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: model, Protocol: "claude"}}, Params: map[string]any{"thinking.type": mode}}}}}
				req := cliproxyexecutor.Request{Model: model, Payload: []byte(`{"max_tokens":16,"messages":[{"role":"user","content":"hi"}],"context_management":{"edits":[{"type":"caller_owned"}]}}`)}
				opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude}
				exec := NewClaudeExecutor(cfg)
				if !stream {
					if _, err := exec.Execute(ctx, auth, req, opts); err != nil {
						t.Fatal(err)
					}
					return
				}
				result, err := exec.ExecuteStream(ctx, auth, req, opts)
				if err != nil {
					t.Fatal(err)
				}
				for chunk := range result.Chunks {
					if chunk.Err != nil {
						t.Fatal(chunk.Err)
					}
				}
			})
		}
	}
}

func TestOrdinaryClaudeSystemAndToolsAcrossMessagesAndCount(t *testing.T) {
	const body = `{"model":"claude-sonnet-5","max_tokens":100,"system":[{"type":"text","text":"top one"},{"type":"text","text":"top two"}],"tools":[{"name":"advisor","input_schema":{"type":"object","properties":{}}}],"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"toolu_safe","name":"advisor","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_safe","content":"first result"},{"type":"text","text":"advisor is ordinary text"}]}]}`
	e := NewClaudeExecutor(&config.Config{})
	a := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "synthetic", "base_url": "https://api.anthropic.com"}}
	seen := map[string]int{}
	tr := canonicalClaudeTestTransport(func(r *http.Request) (*http.Response, error) {
		seen[r.URL.Path]++
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		for path, want := range map[string]string{"system.0.text": "top one", "system.1.text": "top two", "tools.0.name": "advisor", "messages.0.content.0.name": "advisor", "messages.1.content.0.tool_use_id": "toolu_safe", "messages.1.content.1.text": "advisor is ordinary text"} {
			if got := ojson.GetBytesNoCopy(raw, path).String(); got != want {
				t.Errorf("%s %s = %q, want %q", r.URL.Path, path, got, want)
			}
		}
		response := `{"input_tokens":9}`
		if r.URL.Path == "/v1/messages" {
			response = `{"id":"msg_fixture","type":"message","role":"assistant","content":[{"type":"tool_use","id":"toolu_return","name":"advisor","input":{}}],"stop_reason":"tool_use","usage":{"input_tokens":9,"output_tokens":1}}`
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(response))}, nil
	})
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", tr)
	req := cliproxyexecutor.Request{Model: "claude-sonnet-5", Payload: []byte(body)}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude}
	out, err := e.Execute(ctx, a, req, opts)
	if err != nil || ojson.GetBytesNoCopy(out.Payload, "content.0.name").String() != "advisor" || ojson.GetBytesNoCopy(out.Payload, "content.0.id").String() != "toolu_return" {
		t.Fatalf("ordinary response tool failed: %s %v", out.Payload, err)
	}
	if _, err = e.CountTokens(ctx, a, req, opts); err != nil {
		t.Fatal(err)
	}
	if seen["/v1/messages"] != 1 || seen["/v1/messages/count_tokens"] != 1 {
		t.Fatalf("missing entry point: %v", seen)
	}
}

func TestOrdinaryClaudePreservesExplicitThinkingDisplay(t *testing.T) {
	for _, display := range []string{"", "summarized", "omitted"} {
		t.Run("display="+display, func(t *testing.T) {
			ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", canonicalClaudeTestTransport(func(r *http.Request) (*http.Response, error) {
				data, err := io.ReadAll(r.Body)
				if err != nil {
					t.Fatal(err)
				}
				got := ojson.GetBytes(data, "thinking.display")
				if got.Exists() != (display != "") || got.String() != display {
					t.Fatalf("wire display=%q want %q: %s", got.String(), display, data)
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"fixture","model":"claude-opus-5","content":[{"type":"text","text":"ok"}]}`)), Request: r}, nil
			}))
			auth := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": "https://fixture.invalid", "api_key": "fixture"}}
			field := ""
			if display != "" {
				field = `,"display":"` + display + `"`
			}
			req := cliproxyexecutor.Request{Model: "claude-opus-5", Payload: []byte(`{"max_tokens":2048,"thinking":{"type":"adaptive"` + field + `},"messages":[{"role":"user","content":"hi"}]}`)}
			if _, err := NewClaudeExecutor(&config.Config{}).Execute(ctx, auth, req, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestOrdinaryClaudeUnknownInvokeStaysSSEText(t *testing.T) {
	const literal = "<invoke name='unregistered_fixture'>sample</invoke>"
	reply := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_fixture\",\"model\":\"claude-sonnet-5\"}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"" + literal + "\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	calls := 0
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", canonicalClaudeTestTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(reply)), Request: r}, nil
	}))
	a := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "synthetic", "base_url": "https://fixture.invalid"}}
	req := cliproxyexecutor.Request{Model: "claude-sonnet-5", Payload: []byte(`{"model":"claude-sonnet-5","max_tokens":32,"tools":[{"name":"declared_fixture","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"hello"}]}`)}
	out, err := NewClaudeExecutor(&config.Config{}).ExecuteStream(ctx, a, req, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, Stream: true})
	if err != nil {
		t.Fatal(err)
	}
	var wire strings.Builder
	for chunk := range out.Chunks {
		if chunk.Err != nil {
			t.Fatal(chunk.Err)
		}
		wire.Write(chunk.Payload)
	}
	var literalDeltas, terminals int
	for _, line := range strings.Split(wire.String(), "\n") {
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		ev := ojson.Parse(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		if ev.Get("delta.type").String() == "text_delta" && ev.Get("delta.text").String() == literal {
			literalDeltas++
		}
		if ev.Get("type").String() == "message_stop" {
			terminals++
		}
		if ev.Get("content_block.type").String() == "tool_use" || ev.Get("delta.type").String() == "input_json_delta" {
			t.Fatal("literal text became a structured tool event")
		}
	}
	if calls != 1 || literalDeltas != 1 || terminals != 1 {
		t.Fatalf("calls=%d deltas=%d terminal=%d wire=%s", calls, literalDeltas, terminals, wire.String())
	}
}

func TestOrdinaryClaudeBetweenToolsDoesNotAddBlockBinding(t *testing.T) {
	e := NewClaudeExecutor(&config.Config{})
	req := cliproxyexecutor.Request{Model: "claude-sonnet-5-5", Payload: []byte(`{"model":"claude-sonnet-5-5","max_tokens":2048,"thinking":{"type":"between_tools"},"messages":[{"role":"user","content":"hi"}]}`)}
	out, err := e.prepareOrdinaryClaudeMessages(t.Context(), nil, req, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude}, false, false, req.Model, req.Model)
	if err != nil {
		t.Fatal(err)
	}
	if ojson.GetBytesNoCopy(out, "thinking.block_binding").Exists() {
		t.Fatal("ordinary preparation synthesized block_binding")
	}
}

func TestOrdinaryClaudeLeavesToolResultRepresentationsIntact(t *testing.T) {
	for _, tc := range []struct{ name, raw, field, want string }{
		{"string", `"plain string result"`, "", "plain string result"},
		{"nested reference", `[{"type":"tool_reference","tool_name":"mcp__fixture__read"}]`, ".0.tool_name", "mcp__fixture__read"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", canonicalClaudeTestTransport(func(r *http.Request) (*http.Response, error) {
				payload, err := io.ReadAll(r.Body)
				if err != nil {
					t.Fatal(err)
				}
				if got := ojson.GetBytes(payload, "messages.1.content.0.content"+tc.field).String(); got != tc.want {
					t.Fatalf("tool_result field = %q want %q", got, tc.want)
				}
				if got := ojson.GetBytes(payload, "messages.1.content.0.tool_use_id").String(); got != "call-fixture" {
					t.Fatalf("tool_result id=%q", got)
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"fixture","model":"claude-opus-5","content":[{"type":"text","text":"ok"}]}`)), Request: r}, nil
			}))
			auth := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": "https://fixture.invalid", "api_key": "fixture"}}
			req := cliproxyexecutor.Request{Model: "claude-opus-5", Payload: []byte(`{"max_tokens":32,"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"call-fixture","name":"read","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-fixture","content":` + tc.raw + `}]}]}`)}
			if _, err := NewClaudeExecutor(&config.Config{}).Execute(ctx, auth, req, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestOrdinaryClaudeStreamCancellationBeforeHeaders(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	calls := 0
	rt := canonicalClaudeTestTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		cancel()
		return nil, r.Context().Err()
	})
	ctx = context.WithValue(ctx, "cliproxy.roundtripper", rt)
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": "https://fixture.invalid", "api_key": "fixture"}}
	req := cliproxyexecutor.Request{Model: "claude-opus-5", Payload: []byte(`{"max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`)}
	_, err := NewClaudeExecutor(&config.Config{}).ExecuteStream(ctx, auth, req, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude})
	if calls != 1 || !ordinaryClaudeTestIsCancellation(err) {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}

func ordinaryClaudeTestIsCancellation(err error) bool {
	var scoped cliproxyexecutor.RequestScopedError
	var httpStatus interface{ StatusCode() int }
	return errors.Is(err, context.Canceled) && errors.As(err, &scoped) && scoped.IsRequestScoped() && !errors.As(err, &httpStatus)
}

type claudeCancellationTestBody struct {
	io.ReadCloser
	closed chan struct{}
}

func (b claudeCancellationTestBody) Close() error {
	close(b.closed)
	return b.ReadCloser.Close()
}

func TestOrdinaryClaudeStreamCancellationDuringBodyTerminatesWithoutReader(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	reader, writer := io.Pipe()
	defer writer.Close()
	closed := make(chan struct{})
	calls := 0
	rt := canonicalClaudeTestTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		go func() {
			defer writer.CloseWithError(r.Context().Err())
			select {
			case <-r.Context().Done():
				_ = writer.CloseWithError(r.Context().Err())
			case <-closed:
			}
		}()
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: claudeCancellationTestBody{ReadCloser: reader, closed: closed}, Request: r}, nil
	})
	ctx = context.WithValue(ctx, "cliproxy.roundtripper", rt)
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": "https://fixture.invalid", "api_key": "fixture"}}
	req := cliproxyexecutor.Request{Model: "claude-opus-5", Payload: []byte(`{"max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`)}
	result, err := NewClaudeExecutor(&config.Config{}).ExecuteStream(ctx, auth, req, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = io.WriteString(writer, "event: message_start\ndata: {\"type\":\"message_start\"}\n\n"); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("unread stream blocked transport cleanup")
	}
	n := 0
	for chunk := range result.Chunks {
		if chunk.Err == nil {
			continue
		}
		if !ordinaryClaudeTestIsCancellation(chunk.Err) {
			t.Fatalf("unexpected terminal %v", chunk.Err)
		}
		n++
	}
	if calls != 1 || n != 1 {
		t.Fatalf("requests=%d terminals=%d", calls, n)
	}
}

func TestOrdinaryClaudeMissingExplicitAPIKeyNeverSends(t *testing.T) {
	calls := 0
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", canonicalClaudeTestTransport(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("unexpected request")
	}))
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": "https://fixture.invalid", "auth_kind": "apikey"}, Metadata: map[string]any{"access_token": "stale-fixture"}}
	req := cliproxyexecutor.Request{Model: "claude-opus-5", Payload: []byte(`{"max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`)}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, Headers: http.Header{"Authorization": {"Bearer caller-fixture"}, "X-Api-Key": {"caller-fixture"}}}
	_, err := NewClaudeExecutor(&config.Config{}).Execute(ctx, auth, req, opts)
	assertOrdinaryClaudeSSEStatus(t, err, http.StatusUnauthorized)
	if calls != 0 {
		t.Fatalf("upstream calls=%d", calls)
	}
}

func TestOrdinaryClaudePreservesExplicitCacheTTLAndBeta(t *testing.T) {
	const beta = "extended-cache-ttl-2025-04-11"
	for _, explicit := range []bool{false, true} {
		ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", canonicalClaudeTestTransport(func(r *http.Request) (*http.Response, error) {
			raw, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatal(err)
			}
			got := ojson.GetBytes(raw, "messages.0.content.0.cache_control.ttl").String()
			if explicit && (got != "1h" || r.Header.Get("Anthropic-Beta") != beta) {
				t.Fatalf("explicit TTL=%q beta=%q", got, r.Header.Get("Anthropic-Beta"))
			}
			if !explicit && (strings.Contains(string(raw), `"ttl":"1h"`) || r.Header.Get("Anthropic-Beta") != "") {
				t.Fatalf("unexpected automatic TTL or beta")
			}
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"fixture","model":"claude-opus-5","content":[{"type":"text","text":"ok"}]}`)), Request: r}, nil
		}))
		auth := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": "https://fixture.invalid"}, Metadata: map[string]any{"access_token": "fixture"}}
		payload := `{"max_tokens":32,"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`
		opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude}
		if explicit {
			payload = `{"max_tokens":32,"prompt_cache_options":{"mode":"explicit"},"messages":[{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral","ttl":"1h"}}]}]}`
			opts.Headers = http.Header{"Anthropic-Beta": {beta}}
		}
		if _, err := NewClaudeExecutor(&config.Config{}).Execute(ctx, auth, cliproxyexecutor.Request{Model: "claude-opus-5", Payload: []byte(payload)}, opts); err != nil {
			t.Fatal(err)
		}
	}
}

func TestOrdinaryClaudeCallerFieldsOnMessagesWire(t *testing.T) {
	const model = "claude-opus-5"
	for _, tc := range []struct {
		name, extra string
		config      config.PayloadConfig
		fields      map[string]string
		absent      []string
	}{
		{"plain user turn", "", config.PayloadConfig{}, map[string]string{"messages.0.content.0.text": "title is ordinary text"}, []string{"metadata.user_id", "fallbacks", "diagnostics"}},
		{"caller fields", `,"diagnostics":{"caller_custom_key":"caller"},"fallbacks":[{"model":"fixture-fallback"}],"metadata":{"user_id":"caller-id"}`, config.PayloadConfig{}, map[string]string{"diagnostics.caller_custom_key": "caller", "fallbacks.0.model": "fixture-fallback", "metadata.user_id": "caller-id"}, nil},
		{"explicit cache off", `,"prompt_cache_options":{"mode":"explicit"}`, config.PayloadConfig{}, map[string]string{"messages.0.content.0.text": "title is ordinary text"}, []string{"messages.0.content.0.cache_control"}},
		{"raw rule", "", config.PayloadConfig{OverrideRaw: []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: model, Protocol: "claude"}}, Params: map[string]any{"fallbacks": `[{"model":"explicit-fixture"}]`}}}}, map[string]string{"fallbacks.0.model": "explicit-fixture"}, nil},
		{"operator filter", `,"fallbacks":[{"model":"fixture-fallback"}],"thinking":{"type":"adaptive","display":"omitted"}`, config.PayloadConfig{Filter: []config.PayloadFilterRule{{Models: []config.PayloadModelRule{{Name: model, Protocol: "claude"}}, Params: []string{"fallbacks", "thinking.display"}}}}, map[string]string{"thinking.type": "adaptive"}, []string{"fallbacks", "thinking.display"}},
		{"plain system", `,"system":"plain user instructions"`, config.PayloadConfig{}, map[string]string{"system.0.text": "plain user instructions", "messages.0.content.0.text": "title is ordinary text"}, []string{"fallbacks", "diagnostics"}},
		{"payload override", "", config.PayloadConfig{Override: []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: model, Protocol: "claude"}}, Params: map[string]any{"max_tokens": 64, "diagnostics.operator_custom": "rule"}}}}, map[string]string{"max_tokens": "64", "diagnostics.operator_custom": "rule"}, []string{"fallbacks"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", canonicalClaudeTestTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				raw, err := io.ReadAll(r.Body)
				if err != nil {
					t.Fatal(err)
				}
				if r.URL.Path != "/v1/messages" || r.URL.Host != "fixture.invalid" {
					t.Fatalf("unexpected egress %s", r.URL)
				}
				for field, want := range tc.fields {
					if got := ojson.GetBytes(raw, field).String(); got != want {
						t.Errorf("%s=%q want %q", field, got, want)
					}
				}
				for _, field := range tc.absent {
					if ojson.GetBytes(raw, field).Exists() {
						t.Errorf("unexpected synthesized field %s", field)
					}
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"fixture","model":"claude-opus-5","content":[{"type":"text","text":"verbatim ordinary reply"}]}`)), Request: r}, nil
			}))
			auth := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": "https://fixture.invalid", "api_key": "fixture"}}
			req := cliproxyexecutor.Request{Model: model, Payload: []byte(`{"max_tokens":32,"messages":[{"role":"user","content":[{"type":"text","text":"title is ordinary text"}]}]` + tc.extra + "}")}
			out, err := NewClaudeExecutor(&config.Config{Payload: tc.config}).Execute(ctx, auth, req, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude})
			if err != nil || calls != 1 || ojson.GetBytes(out.Payload, "content.0.text").String() != "verbatim ordinary reply" {
				t.Fatalf("requests=%d output=%s error=%v", calls, out.Payload, err)
			}
		})
	}
}

func TestOrdinaryClaudePreservesPublicToolDefinitionsAndChoice(t *testing.T) {
	for _, tool := range []struct{ name, definition string }{
		{"fixture_custom", `{"name":"fixture_custom","input_schema":{"type":"object","properties":{"path":{"type":"string"}}}}`},
		{"bash", `{"type":"bash_20250124","name":"bash"}`},
		{"mcp__fixture__read", `{"name":"mcp__fixture__read","input_schema":{"type":"object"}}`},
	} {
		t.Run(tool.name, func(t *testing.T) {
			calls := 0
			ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", canonicalClaudeTestTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				b, err := io.ReadAll(r.Body)
				if err != nil {
					t.Fatal(err)
				}
				if got := ojson.GetBytes(b, "tools.0").Raw; got != tool.definition {
					t.Fatalf("tool definition = %s want %s", got, tool.definition)
				}
				if got := ojson.GetBytes(b, "tool_choice.name").String(); got != tool.name {
					t.Fatalf("tool choice=%q", got)
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"fixture","model":"claude-opus-5","content":[{"type":"text","text":"ok"}]}`)), Request: r}, nil
			}))
			auth := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": "https://fixture.invalid", "api_key": "fixture"}}
			r := cliproxyexecutor.Request{Model: "claude-opus-5", Payload: []byte(`{"max_tokens":32,"prompt_cache_options":{"mode":"explicit"},"tools":[` + tool.definition + `],"tool_choice":{"type":"tool","name":"` + tool.name + `"},"messages":[{"role":"user","content":"hi"}]}`)}
			if _, err := NewClaudeExecutor(&config.Config{}).Execute(ctx, auth, r, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude}); err != nil || calls != 1 {
				t.Fatalf("calls=%d error=%v", calls, err)
			}
		})
	}
}

func TestOrdinaryClaudeThinkingRespectsProtocolBoundary(t *testing.T) {
	for _, tc := range []struct {
		name    string
		source  sdktranslator.Format
		payload string
		native  bool
	}{
		{"native opaque", sdktranslator.FormatClaude, `{"max_tokens":64,"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"caller reasoning","signature":"c3ludGhldGljLW9wYXF1ZS1maXh0dXJl"},{"type":"text","text":"previous answer"}]},{"role":"user","content":"follow up"}]}`, true},
		{"translated opaque", sdktranslator.FormatOpenAIResponse, `{"max_output_tokens":64,"input":[{"type":"reasoning","encrypted_content":"opaque-from-other-protocol","summary":[{"type":"summary_text","text":"unavailable reasoning"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"previous answer"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"follow up"}]}]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", canonicalClaudeTestTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				b, err := io.ReadAll(r.Body)
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(b), "previous answer") || !strings.Contains(string(b), "follow up") {
					t.Fatalf("lost ordinary content: %s", b)
				}
				if tc.native && !strings.Contains(string(b), `"signature":"c3ludGhldGljLW9wYXF1ZS1maXh0dXJl"`) {
					t.Fatalf("native opaque value changed: %s", b)
				}
				if !tc.native && (strings.Contains(string(b), "opaque-from-other-protocol") || strings.Contains(string(b), "unavailable reasoning")) {
					t.Fatalf("cross-protocol state forwarded: %s", b)
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"fixture","model":"claude-opus-5","content":[{"type":"text","text":"ok"}]}`)), Request: r}, nil
			}))
			auth := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": "https://fixture.invalid", "api_key": "fixture"}}
			req := cliproxyexecutor.Request{Model: "claude-opus-5", Payload: []byte(tc.payload)}
			_, err := NewClaudeExecutor(&config.Config{}).Execute(ctx, auth, req, cliproxyexecutor.Options{SourceFormat: tc.source, ResponseFormat: sdktranslator.FormatClaude})
			if err != nil || calls != 1 {
				t.Fatalf("calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestOrdinaryClaudeRejectsInvalidThinkingMetadataLocally(t *testing.T) {
	for _, sig := range []string{`123`, `""`, `"E!!!invalid!!!"`, `"Ev/+/Q=="`, `"gAAAAAAAAAAACQoLDA0ODxAREhMUFRYXGBkaGxwdHh8gISIjJCUmJygpKissLS4vMDEyMzQ1Njc4OTo7PD0-P0BBQkNERUZHSA"`} {
		calls := 0
		ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", canonicalClaudeTestTransport(func(*http.Request) (*http.Response, error) { calls++; return nil, errors.New("unexpected request") }))
		auth := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": "https://fixture.invalid", "api_key": "fixture"}}
		req := cliproxyexecutor.Request{Model: "claude-opus-5", Payload: []byte(`{"max_tokens":32,"messages":[{"role":"user","content":"first"},{"role":"assistant","content":[{"type":"thinking","thinking":"caller text","signature":` + sig + `},{"type":"text","text":"answer"}]},{"role":"user","content":"follow up"}]}`)}
		exec := NewClaudeExecutor(&config.Config{})
		opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude}
		_, err := exec.Execute(ctx, auth, req, opts)
		assertOrdinaryClaudeSSEStatus(t, err, http.StatusBadRequest)
		_, err = exec.ExecuteStream(ctx, auth, req, opts)
		assertOrdinaryClaudeSSEStatus(t, err, http.StatusBadRequest)
		_, err = exec.CountTokens(ctx, auth, req, opts)
		assertOrdinaryClaudeSSEStatus(t, err, http.StatusBadRequest)
		if calls != 0 {
			t.Fatalf("calls=%d", calls)
		}
	}
}

func TestOrdinaryClaudeTextWithInvokeAndTrailingContentStaysText(t *testing.T) {
	const markup = "<invoke name='declared_fixture'>sample</invoke>"
	for _, deltas := range [][]string{{markup + " trailing"}, {markup, " trailing"}} {
		var wire strings.Builder
		wire.WriteString("data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
		for _, s := range deltas {
			wire.WriteString(fmt.Sprintf("data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":%q}}\n\n", s))
		}
		wire.WriteString("data: {\"type\":\"message_stop\"}\n\n")
		ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", canonicalClaudeTestTransport(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(wire.String())), Request: r}, nil
		}))
		auth := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": "https://fixture.invalid", "api_key": "fixture"}}
		req := cliproxyexecutor.Request{Model: "claude-opus-5", Payload: []byte(`{"max_tokens":32,"tools":[{"name":"declared_fixture","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"hello"}]}`)}
		out, err := NewClaudeExecutor(&config.Config{}).ExecuteStream(ctx, auth, req, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude})
		if err != nil {
			t.Fatal(err)
		}
		var text strings.Builder
		for event := range out.Chunks {
			if event.Err != nil {
				t.Fatal(event.Err)
			}
			for _, line := range strings.Split(string(event.Payload), "\n") {
				data, ok := strings.CutPrefix(line, "data:")
				if !ok {
					continue
				}
				item := ojson.Parse(strings.TrimSpace(data))
				if item.Get("content_block.type").String() == "tool_use" || item.Get("delta.type").String() == "input_json_delta" {
					t.Fatal("literal text became a tool event")
				}
				text.WriteString(item.Get("delta.text").String())
			}
		}
		if text.String() != markup+" trailing" {
			t.Fatalf("text=%q", text.String())
		}
	}
}

func TestClaudeExecutor_Explicit1MAliasUsesOfficialModelWithoutLegacyContextBeta(t *testing.T) {
	const model = "claude-sonnet-4-6"
	for _, requested := range []string{model, "sonnet[1m]"} {
		t.Run(requested, func(t *testing.T) {
			calls := 0
			ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", canonicalClaudeTestTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				b, err := io.ReadAll(r.Body)
				if err != nil {
					t.Fatal(err)
				}
				if modelWire := ojson.GetBytes(b, "model").String(); modelWire != model {
					t.Fatalf("wire model=%q", modelWire)
				}
				if r.Header.Get("Anthropic-Beta") != "" {
					t.Fatalf("unexpected beta associated with model metadata")
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"fixture","model":"claude-sonnet-4-6","content":[{"type":"text","text":"ok"}]}`)), Request: r}, nil
			}))
			auth := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": "https://fixture.invalid", "api_key": "fixture"}}
			req := cliproxyexecutor.Request{Model: model, Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, Metadata: map[string]any{cliproxyexecutor.RequestedModelMetadataKey: requested}}
			if _, err := NewClaudeExecutor(&config.Config{}).Execute(ctx, auth, req, opts); err != nil || calls != 1 {
				t.Fatalf("calls=%d error=%v", calls, err)
			}
		})
	}
}

func TestOrdinaryClaudeRejectsForeignToolStateWithoutSending(t *testing.T) {
	calls := 0
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", canonicalClaudeTestTransport(func(*http.Request) (*http.Response, error) { calls++; return nil, errors.New("unexpected request") }))
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": "https://fixture.invalid", "api_key": "fixture"}}
	req := cliproxyexecutor.Request{Model: "claude-opus-5", Payload: []byte(`{"max_tokens":32,"messages":[{"role":"user","content":"first"},{"role":"assistant","content":[{"type":"tool_use","id":"fixture-id","name":"fixture_tool","input":{},"extra_content":{"google":{"thought_signature":"skip_thought_signature_validator"}}},{"type":"text","text":"answer"}]},{"role":"user","content":"follow up"}]}`)}
	_, err := NewClaudeExecutor(&config.Config{}).Execute(ctx, auth, req, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude})
	assertOrdinaryClaudeSSEStatus(t, err, http.StatusBadRequest)
	if calls != 0 {
		t.Fatalf("calls=%d", calls)
	}
}

func TestOrdinaryClaudeDoesNotInjectThinkingDisplay(t *testing.T) {
	for _, extra := range []string{"", `,"thinking":{"type":"disabled"}`} {
		ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", canonicalClaudeTestTransport(func(r *http.Request) (*http.Response, error) {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatal(err)
			}
			if ojson.GetBytes(body, "thinking.display").Exists() {
				t.Fatal("display injected")
			}
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"fixture","model":"claude-opus-5","content":[{"type":"text","text":"ok"}]}`)), Request: r}, nil
		}))
		auth := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": "https://fixture.invalid", "api_key": "fixture"}}
		req := cliproxyexecutor.Request{Model: "claude-opus-5", Payload: []byte(`{"max_tokens":32,"messages":[{"role":"user","content":"hello"}]` + extra + "}")}
		if _, err := NewClaudeExecutor(&config.Config{}).Execute(ctx, auth, req, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude}); err != nil {
			t.Fatal(err)
		}
	}
}
