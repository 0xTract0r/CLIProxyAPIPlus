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

// TestCodexAutoExecutorFastRoutesHTTPDownstreamToWebsocket is the Commit A routing
// guard: a fast-enabled credential must route a PLAIN HTTP downstream request (no
// WithDownstreamWebsocket) to the websocket upstream, and the outbound frame must
// carry service_tier=priority. If routing fell back to the HTTP executor the upstream
// would receive a POST rather than a websocket upgrade, which the handler rejects.
func TestCodexAutoExecutorFastRoutesHTTPDownstreamToWebsocket(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	captured := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

		_, payload, errRead := conn.ReadMessage()
		if errRead != nil {
			t.Errorf("read upstream frame: %v", errRead)
			return
		}
		captured <- bytes.Clone(payload)

		completed := []byte(`{"type":"response.completed","response":{"id":"resp-fast","output":[],"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}}`)
		if errWrite := conn.WriteMessage(websocket.TextMessage, completed); errWrite != nil {
			t.Errorf("write completed frame: %v", errWrite)
			return
		}
		// Keep the connection open (session path reuses it) until the client/server tears down.
		for {
			if _, _, errDrain := conn.ReadMessage(); errDrain != nil {
				return
			}
		}
	}))
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

	// NOTE: no WithDownstreamWebsocket -> plain HTTP downstream. Fast must still route to ws.
	if _, err := exec.Execute(context.Background(), auth, req, opts); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	t.Cleanup(func() { exec.CloseExecutionSession(codexFastSessionFallbackID(opts, req)) })

	select {
	case payload := <-captured:
		if got := gjson.GetBytes(payload, "type").String(); got != "response.create" {
			t.Fatalf("upstream type = %s, want response.create; payload=%s", got, payload)
		}
		if got := gjson.GetBytes(payload, "service_tier").String(); got != "priority" {
			t.Fatalf("upstream service_tier = %s, want priority; payload=%s", got, payload)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for upstream websocket frame; fast routing may have used the HTTP executor")
	}
}
