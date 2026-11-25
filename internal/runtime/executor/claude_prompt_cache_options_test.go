package executor

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestOrdinaryClaudePromptCacheOptionsOnWire(t *testing.T) {
	const withToolAndSystem = `"tools":[{"name":"read_value","input_schema":{"type":"object"}}],"system":[{"type":"text","text":"cacheable system"}]`
	const turns = `"messages":[{"role":"user","content":"ordinary request"}]`
	for _, tc := range []struct {
		name, options string
		wantTTL       string
		wantMarker    bool
	}{
		{"explicit no automatic markers", `,"prompt_cache_options":{"mode":"explicit"}`, "", false},
		{"implicit automatic markers", "", "", true},
		{"explicit caller system marker", `,"prompt_cache_options":{"mode":"explicit"},"system":[{"type":"text","text":"caller system","cache_control":{"type":"ephemeral","ttl":"1h"}}]`, "1h", true},
	} {
		for _, mode := range []string{"sync", "stream", "count"} {
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", ordinaryClaudeTestRoundTripper(func(r *http.Request) (*http.Response, error) {
					body, err := io.ReadAll(r.Body)
					if err != nil {
						return nil, err
					}
					doc := ojson.NewDoc(body)
					if doc.Get("prompt_cache_options").Exists() {
						t.Error("local cache options reached the provider")
					}
					if tc.wantTTL != "" && doc.Get("system.0.cache_control.ttl").String() != tc.wantTTL {
						t.Errorf("caller TTL changed: %s", body)
					}
					wantMarker := tc.wantMarker && (mode != "count" || tc.wantTTL != "")
					if got := strings.Contains(string(body), `"cache_control"`); got != wantMarker {
						t.Errorf("cache marker=%v want %v: %s", got, wantMarker, body)
					}
					payload := `{"id":"fixture","type":"message","model":"claude-opus-5","role":"assistant","content":[{"type":"text","text":"ok"}]}`
					headers := http.Header{"Content-Type": {"application/json"}}
					if mode == "count" {
						if r.URL.Path != "/v1/messages/count_tokens" {
							t.Errorf("count reached %q", r.URL.Path)
						}
						payload = `{"input_tokens":15}`
					} else if mode == "stream" {
						headers.Set("Content-Type", "text/event-stream")
						payload = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"fixture\",\"model\":\"claude-opus-5\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
					}
					return &http.Response{StatusCode: 200, Header: headers, Body: io.NopCloser(strings.NewReader(payload)), Request: r}, nil
				}))
				base := "https://fixture.invalid"
				if mode == "count" {
					base = "https://api.anthropic.com"
				}
				auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "fixture", "base_url": base}}
				payload := "{\"max_tokens\":12," + withToolAndSystem + "," + turns + tc.options + "}"
				if tc.wantTTL != "" {
					payload = "{\"max_tokens\":12," + turns + tc.options + "}"
				}
				req := cliproxyexecutor.Request{Model: "claude-opus-5", Payload: []byte(payload)}
				opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude}
				e := NewClaudeExecutor(&config.Config{})
				switch mode {
				case "sync":
					if _, err := e.Execute(ctx, auth, req, opts); err != nil {
						t.Fatal(err)
					}
				case "count":
					if r, err := e.CountTokens(ctx, auth, req, opts); err != nil || ojson.GetBytesNoCopy(r.Payload, "input_tokens").Int() != 15 {
						t.Fatalf("count=%s err=%v", r.Payload, err)
					}
				case "stream":
					r, err := e.ExecuteStream(ctx, auth, req, opts)
					if err != nil {
						t.Fatal(err)
					}
					for ch := range r.Chunks {
						if ch.Err != nil {
							t.Fatal(ch.Err)
						}
					}
				}
			})
		}
	}
}
