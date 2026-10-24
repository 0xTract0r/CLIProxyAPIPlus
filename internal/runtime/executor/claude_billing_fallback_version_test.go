package executor

import (
	"bytes"
	"context"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClaudeCCHFallbackUsesResolvedVersionWithoutChangingLegacyCallers(t *testing.T) {
	payload := []byte(`{"messages":[{"role":"user","content":"hello"}]}`)
	for _, tc := range []struct {
		name, baseline, want string
		resolved             []string
	}{
		{name: "legacy_default", want: "2.1.211"},
		{name: "resolved_request", resolved: []string{"2.1.262"}, want: "2.1.262"},
		{name: "empty_resolved_uses_config", baseline: "claude-cli/2.1.220 (external, cli)", resolved: []string{" "}, want: "2.1.220"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{UserAgent: tc.baseline}}
			got := claudeCCHFallbackBillingHeader(context.Background(), cfg, payload, "cli", tc.resolved...)
			if !strings.Contains(got, "cc_version="+tc.want+".") {
				t.Fatalf("fallback=%q want version %s", got, tc.want)
			}
		})
	}
}
func TestClaudeServingFallbackBillingUsesSelectedFingerprint(t *testing.T) {
	for _, tc := range []struct {
		name        string
		stream, pin bool
		want        string
	}{
		{name: "stream_observed", stream: true, want: "2.1.262"},
		{name: "http_explicit_pin", pin: true, want: "2.1.220"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetClaudeDeviceProfileCache()
			stable := true
			captured := make(chan struct {
				body []byte
				ua   string
			}, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				captured <- struct {
					body []byte
					ua   string
				}{body, r.Header.Get("User-Agent")}
				if tc.stream {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_fixture\",\"type\":\"message\",\"model\":\"claude-sonnet-4-6\",\"role\":\"assistant\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
				} else {
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"id":"msg_fixture","type":"message","model":"claude-sonnet-4-6","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`)
				}
			}))
			defer server.Close()
			cfg := &config.Config{ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{StabilizeDeviceProfile: &stable, PinSoftwareBaseline: tc.pin}}
			if tc.pin {
				cfg.ClaudeHeaderDefaults.UserAgent = "claude-cli/2.1.220 (external, cli)"
			}
			auth := &cliproxyauth.Auth{ID: tc.name, FileName: tc.name + ".json", Provider: "claude", ProxyURL: "direct", Attributes: map[string]string{"api_key": "sk-ant-oat-synthetic", "base_url": server.URL}, Metadata: claudeOAuthTestMetadata()}
			req := cliproxyexecutor.Request{Model: "claude-sonnet-4-6", Payload: []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, Headers: http.Header{"User-Agent": []string{"claude-cli/2.1.262 (external, cli)"}, "X-Stainless-Package-Version": []string{"0.94.0"}, "X-Stainless-Runtime-Version": []string{"v24.3.0"}}}
			ex := NewClaudeExecutor(cfg)
			if tc.stream {
				result, err := ex.ExecuteStream(context.Background(), auth, req, opts)
				if err != nil {
					t.Fatal(err)
				}
				for chunk := range result.Chunks {
					if chunk.Err != nil {
						t.Fatal(chunk.Err)
					}
				}
			} else if _, err := ex.Execute(context.Background(), auth, req, opts); err != nil {
				t.Fatal(err)
			}
			got := <-captured
			if !strings.HasPrefix(got.ua, "claude-cli/"+tc.want+" ") {
				t.Fatalf("outbound UA=%q", got.ua)
			}
			if version := billingVersionFromBody(t, got.body); version != tc.want {
				t.Fatalf("billing=%q want %s", version, tc.want)
			}
			request, err := http.NewRequest(http.MethodPost, server.URL, bytes.NewReader(got.body))
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("User-Agent", got.ua)
			resigned, err := signClaudeHTTPRequestCCH(request, got.body)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(resigned, got.body) {
				t.Fatal("wire CCH not final-byte idempotent for selected UA")
			}
		})
	}
}
