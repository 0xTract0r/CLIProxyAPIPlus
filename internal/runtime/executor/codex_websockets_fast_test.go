package executor

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// TestCodexFastEnabled covers the per-account & per-model fast gate: fast is opt-in,
// off by default, matches case-insensitively, supports a "*" wildcard, and reads from
// the Metadata fallback when the attribute is absent.
func TestCodexFastEnabled(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		auth  *cliproxyauth.Auth
		model string
		want  bool
	}{
		{"nil auth", nil, "gpt-5.6", false},
		{"no attribute", &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "sk"}}, "gpt-5.6", false},
		{"empty attribute", &cliproxyauth.Auth{Attributes: map[string]string{"fast_models": ""}}, "gpt-5.6", false},
		{"exact match", &cliproxyauth.Auth{Attributes: map[string]string{"fast_models": "gpt-5.6"}}, "gpt-5.6", true},
		{"case insensitive", &cliproxyauth.Auth{Attributes: map[string]string{"fast_models": "GPT-5.6"}}, "gpt-5.6", true},
		{"model not listed", &cliproxyauth.Auth{Attributes: map[string]string{"fast_models": "gpt-5.6"}}, "gpt-5.4", false},
		{"list contains model", &cliproxyauth.Auth{Attributes: map[string]string{"fast_models": "gpt-5.4,gpt-5.6"}}, "gpt-5.6", true},
		{"wildcard enables all", &cliproxyauth.Auth{Attributes: map[string]string{"fast_models": "*"}}, "anything", true},
		{"empty model never matches", &cliproxyauth.Auth{Attributes: map[string]string{"fast_models": "*"}}, "", false},
		{"metadata string fallback", &cliproxyauth.Auth{Metadata: map[string]any{"fast_models": "gpt-5.6"}}, "gpt-5.6", true},
		{"metadata slice fallback", &cliproxyauth.Auth{Metadata: map[string]any{"fast_models": []string{"gpt-5.6"}}}, "gpt-5.6", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := codexFastEnabled(tc.auth, tc.model); got != tc.want {
				t.Fatalf("codexFastEnabled(%v, %q) = %v, want %v", tc.auth, tc.model, got, tc.want)
			}
		})
	}
}

// TestCodexBaseModelNameStripsSuffix confirms the fast gate compares against the base
// model name, not the thinking-suffixed alias.
func TestCodexBaseModelNameStripsSuffix(t *testing.T) {
	t.Parallel()
	if got := codexBaseModelName("gpt-5.6(high)"); got != "gpt-5.6" {
		t.Fatalf("codexBaseModelName(gpt-5.6(high)) = %q, want gpt-5.6", got)
	}
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"fast_models": "gpt-5.6"}}
	if !codexFastEnabled(auth, codexBaseModelName("gpt-5.6(high)")) {
		t.Fatal("fast should be enabled for base model gpt-5.6 when request model is gpt-5.6(high)")
	}
}

// TestApplyCodexServiceTierPriority is a small unit guard on the priority injection.
func TestApplyCodexServiceTierPriority(t *testing.T) {
	t.Parallel()
	out := applyCodexServiceTierPriority([]byte(`{"model":"gpt-5.6"}`))
	if got := gjson.GetBytes(out, "service_tier").String(); got != "priority" {
		t.Fatalf("service_tier = %q, want priority", got)
	}
	if applyCodexServiceTierPriority(nil) != nil {
		t.Fatal("nil body should stay nil")
	}
}

// TestCodexFastSessionFallbackIDPrefersPromptCacheKey verifies the HTTP fast fallback
// derives a stable, namespaced session id (here from prompt_cache_key, since an
// explicit prompt_cache_key suppresses the derived-session identity).
func TestCodexFastSessionFallbackIDPrefersPromptCacheKey(t *testing.T) {
	t.Parallel()
	req := cliproxyexecutor.Request{Payload: []byte(`{"model":"gpt-5.6","prompt_cache_key":"conv-123"}`)}
	got := codexFastSessionFallbackID(cliproxyexecutor.Options{}, req)
	if got != "codex-fast:pck:conv-123" {
		t.Fatalf("fallback id = %q, want codex-fast:pck:conv-123", got)
	}
	if empty := codexFastSessionFallbackID(cliproxyexecutor.Options{}, cliproxyexecutor.Request{Payload: []byte(`{}`)}); empty != "" {
		t.Fatalf("fallback id for empty request = %q, want empty", empty)
	}
}

// fastPrewarmUpstream is a mock codex responses websocket upstream that drives the
// full fast flow: it reads the prewarm frame, answers response.created +
// response.completed with prewarmID, reads the main frame, and answers
// response.completed with mainID. Captured frames are delivered on the channels.
func fastPrewarmUpstream(t *testing.T, prewarmID, mainID string, capturePrewarm, captureMain chan []byte) *httptest.Server {
	t.Helper()
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") == "" {
			t.Errorf("fast request must upgrade to websocket, got plain HTTP %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade websocket: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()

		_, prewarm, errPrewarm := conn.ReadMessage()
		if errPrewarm != nil {
			t.Errorf("read prewarm frame: %v", errPrewarm)
			return
		}
		if capturePrewarm != nil {
			capturePrewarm <- bytes.Clone(prewarm)
		}
		created := []byte(`{"type":"response.created","response":{"id":"","status":"in_progress","output":[]}}`)
		created, _ = sjson.SetBytes(created, "response.id", prewarmID)
		if errWrite := conn.WriteMessage(websocket.TextMessage, created); errWrite != nil {
			t.Errorf("write prewarm created: %v", errWrite)
			return
		}
		completedPrewarm := []byte(`{"type":"response.completed","response":{"id":"","status":"completed","output":[],"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}}`)
		completedPrewarm, _ = sjson.SetBytes(completedPrewarm, "response.id", prewarmID)
		if errWrite := conn.WriteMessage(websocket.TextMessage, completedPrewarm); errWrite != nil {
			t.Errorf("write prewarm completed: %v", errWrite)
			return
		}

		_, main, errMain := conn.ReadMessage()
		if errMain != nil {
			t.Errorf("read main frame: %v", errMain)
			return
		}
		if captureMain != nil {
			captureMain <- bytes.Clone(main)
		}
		completedMain := []byte(`{"type":"response.completed","response":{"id":"","output":[],"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}}`)
		completedMain, _ = sjson.SetBytes(completedMain, "response.id", mainID)
		if errWrite := conn.WriteMessage(websocket.TextMessage, completedMain); errWrite != nil {
			t.Errorf("write main completed: %v", errWrite)
			return
		}
		for {
			if _, _, errDrain := conn.ReadMessage(); errDrain != nil {
				return
			}
		}
	}))
}

// TestCodexAutoExecutorFastRoutesHTTPDownstreamAndRunsPrewarm is the end-to-end fast
// guard for the non-streaming Execute path: a fast-enabled credential must route a
// PLAIN HTTP downstream request (no WithDownstreamWebsocket) to the websocket upstream,
// run the prewarm (generate:false) then the main turn linked via previous_response_id,
// and carry service_tier=priority on both frames. If routing fell back to the HTTP
// executor the upstream would receive a POST rather than an upgrade, which the mock
// rejects.
func TestCodexAutoExecutorFastRoutesHTTPDownstreamAndRunsPrewarm(t *testing.T) {
	capturePrewarm := make(chan []byte, 1)
	captureMain := make(chan []byte, 1)
	server := fastPrewarmUpstream(t, "resp_prewarm_1", "resp_main_1", capturePrewarm, captureMain)
	defer server.Close()

	exec := NewCodexAutoExecutor(&config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}})
	auth := &cliproxyauth.Auth{
		ID:       "codex-fast-route",
		Provider: "codex",
		Attributes: map[string]string{
			"api_key":     "sk-test",
			"base_url":    server.URL,
			"fast_models": "gpt-5-codex",
		},
	}
	req := cliproxyexecutor.Request{
		Model:   "gpt-5-codex",
		Payload: []byte(`{"model":"gpt-5-codex","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"prompt_cache_key":"fast-route-conv"}`),
	}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("codex")}
	t.Cleanup(func() { exec.CloseExecutionSession(codexFastSessionFallbackID(opts, req)) })

	// NOTE: no WithDownstreamWebsocket -> plain HTTP downstream. Fast must still route to ws.
	if _, err := exec.Execute(context.Background(), auth, req, opts); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	prewarm := receiveFrame(t, capturePrewarm, "prewarm")
	if got := gjson.GetBytes(prewarm, "type").String(); got != "response.create" {
		t.Fatalf("prewarm type = %s, want response.create; payload=%s", got, prewarm)
	}
	if generate := gjson.GetBytes(prewarm, "generate"); !generate.Exists() || generate.Bool() {
		t.Fatalf("prewarm generate = %s, want false; payload=%s", generate.Raw, prewarm)
	}
	if got := gjson.GetBytes(prewarm, "service_tier").String(); got != "priority" {
		t.Fatalf("prewarm service_tier = %s, want priority; payload=%s", got, prewarm)
	}

	main := receiveFrame(t, captureMain, "main")
	if got := gjson.GetBytes(main, "type").String(); got != "response.create" {
		t.Fatalf("main type = %s, want response.create; payload=%s", got, main)
	}
	if got := gjson.GetBytes(main, "previous_response_id").String(); got != "resp_prewarm_1" {
		t.Fatalf("main previous_response_id = %s, want resp_prewarm_1; payload=%s", got, main)
	}
	if gjson.GetBytes(main, "generate").Exists() {
		t.Fatalf("main frame must not carry generate; payload=%s", main)
	}
	if got := gjson.GetBytes(main, "service_tier").String(); got != "priority" {
		t.Fatalf("main service_tier = %s, want priority; payload=%s", got, main)
	}
}

func receiveFrame(t *testing.T, ch chan []byte, label string) []byte {
	t.Helper()
	select {
	case payload := <-ch:
		return payload
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s frame", label)
		return nil
	}
}

// TestCodexAutoExecutorFastStreamRunsPrewarmThenMain covers the streaming path: a fast
// HTTP/SSE downstream request must route to ws, run prewarm -> main linked by
// previous_response_id, and complete without hanging.
func TestCodexAutoExecutorFastStreamRunsPrewarmThenMain(t *testing.T) {
	capturePrewarm := make(chan []byte, 1)
	captureMain := make(chan []byte, 1)
	server := fastPrewarmUpstream(t, "resp_prewarm_s", "resp_main_s", capturePrewarm, captureMain)
	defer server.Close()

	exec := NewCodexAutoExecutor(&config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}})
	auth := &cliproxyauth.Auth{
		ID:       "codex-fast-stream",
		Provider: "codex",
		Attributes: map[string]string{
			"api_key":     "sk-test",
			"base_url":    server.URL,
			"fast_models": "*",
		},
	}
	req := cliproxyexecutor.Request{
		Model:   "gpt-5-codex",
		Payload: []byte(`{"model":"gpt-5-codex","stream":true,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"prompt_cache_key":"fast-stream-conv"}`),
	}
	opts := cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FromString("codex"),
		ResponseFormat: sdktranslator.FromString("codex"),
	}
	t.Cleanup(func() { exec.CloseExecutionSession(codexFastSessionFallbackID(opts, req)) })

	// No WithDownstreamWebsocket -> plain HTTP/SSE downstream.
	result, err := exec.ExecuteStream(context.Background(), auth, req, opts)
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	drained := make(chan struct{})
	go func() {
		for range result.Chunks {
		}
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out draining fast stream chunks")
	}

	prewarm := receiveFrame(t, capturePrewarm, "prewarm")
	if generate := gjson.GetBytes(prewarm, "generate"); !generate.Exists() || generate.Bool() {
		t.Fatalf("stream prewarm generate = %s, want false; payload=%s", generate.Raw, prewarm)
	}
	main := receiveFrame(t, captureMain, "main")
	if got := gjson.GetBytes(main, "previous_response_id").String(); got != "resp_prewarm_s" {
		t.Fatalf("stream main previous_response_id = %s, want resp_prewarm_s; payload=%s", got, main)
	}
}

// TestCodexWebsocketsFastPrewarmFailsClosed verifies fail-closed behavior: when the
// prewarm turn returns an upstream error, Execute aborts with an error and never sends
// the main turn (no silent downgrade / mischarge).
func TestCodexWebsocketsFastPrewarmFailsClosed(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	mainReceived := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade websocket: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		// Read prewarm, answer with an upstream error frame.
		if _, _, errRead := conn.ReadMessage(); errRead != nil {
			return
		}
		errFrame := []byte(`{"type":"error","status":429,"error":{"message":"rate limited","type":"rate_limit_error"}}`)
		if errWrite := conn.WriteMessage(websocket.TextMessage, errFrame); errWrite != nil {
			return
		}
		// If a second frame ever arrives, the executor wrongly sent the main turn.
		if _, _, errRead := conn.ReadMessage(); errRead == nil {
			select {
			case mainReceived <- struct{}{}:
			default:
			}
		}
	}))
	defer server.Close()

	exec := NewCodexWebsocketsExecutor(&config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}})
	auth := &cliproxyauth.Auth{
		ID:       "codex-fast-failclosed",
		Provider: "codex",
		Attributes: map[string]string{
			"api_key":     "sk-test",
			"base_url":    server.URL,
			"fast_models": "*",
		},
	}
	req := cliproxyexecutor.Request{
		Model:   "gpt-5-codex",
		Payload: []byte(`{"model":"gpt-5-codex","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"prompt_cache_key":"fast-failclosed-conv"}`),
	}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("codex")}
	t.Cleanup(func() { exec.CloseExecutionSession(codexFastSessionFallbackID(opts, req)) })

	_, err := exec.Execute(context.Background(), auth, req, opts)
	if err == nil {
		t.Fatal("Execute() error = nil, want fail-closed prewarm error")
	}
	select {
	case <-mainReceived:
		t.Fatal("main turn was sent after prewarm error; fast must fail closed")
	case <-time.After(300 * time.Millisecond):
	}
}

// TestBuildCodexWebsocketPrewarmBody guards the prewarm frame shape: type
// response.create, generate:false, warmup input, inherited previous_response_id
// dropped, and identity/priority fields preserved.
func TestBuildCodexWebsocketPrewarmBody(t *testing.T) {
	t.Parallel()
	upstream := []byte(`{"model":"gpt-5.6","service_tier":"priority","previous_response_id":"client-prev","prompt_cache_key":"pck","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"real prompt"}]}]}`)
	out := buildCodexWebsocketPrewarmBody(upstream)
	if got := gjson.GetBytes(out, "type").String(); got != "response.create" {
		t.Fatalf("prewarm type = %s, want response.create", got)
	}
	if generate := gjson.GetBytes(out, "generate"); !generate.Exists() || generate.Bool() {
		t.Fatalf("prewarm generate = %s, want false", generate.Raw)
	}
	if gjson.GetBytes(out, "previous_response_id").Exists() {
		t.Fatal("prewarm must drop inherited previous_response_id")
	}
	if got := gjson.GetBytes(out, "service_tier").String(); got != "priority" {
		t.Fatalf("prewarm service_tier = %s, want priority", got)
	}
	if got := gjson.GetBytes(out, "prompt_cache_key").String(); got != "pck" {
		t.Fatalf("prewarm prompt_cache_key = %s, want pck (shared with main)", got)
	}
	if got := gjson.GetBytes(out, "input.0.content.0.text").String(); got != codexFastPrewarmWarmupText {
		t.Fatalf("prewarm input text = %q, want warmup %q", got, codexFastPrewarmWarmupText)
	}
}

// TestBuildCodexWebsocketFastMainBody guards the main frame shape: previous_response_id
// set to the prewarm id, generate stripped, real input preserved.
func TestBuildCodexWebsocketFastMainBody(t *testing.T) {
	t.Parallel()
	upstream := []byte(`{"model":"gpt-5.6","service_tier":"priority","generate":false,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"real prompt"}]}]}`)
	out := buildCodexWebsocketFastMainBody(upstream, "resp_prewarm_x")
	if got := gjson.GetBytes(out, "type").String(); got != "response.create" {
		t.Fatalf("main type = %s, want response.create", got)
	}
	if got := gjson.GetBytes(out, "previous_response_id").String(); got != "resp_prewarm_x" {
		t.Fatalf("main previous_response_id = %s, want resp_prewarm_x", got)
	}
	if gjson.GetBytes(out, "generate").Exists() {
		t.Fatal("main frame must not carry generate")
	}
	if got := gjson.GetBytes(out, "input.0.content.0.text").String(); got != "real prompt" {
		t.Fatalf("main input text = %q, want the real prompt", got)
	}
}
