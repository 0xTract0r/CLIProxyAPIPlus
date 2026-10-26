package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestClaudeUncloak222ServingIdentity(t *testing.T) {
	const saved = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	const caller = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	for _, stream := range []bool{false, true} {
		for _, identity := range []bool{false, true} {
			for _, mode := range []string{"uncloak", "default", "never", "always", "native", "cli-profile", "oauth", "config-always"} {
				t.Run(fmt.Sprintf("stream=%v/id=%v/%s", stream, identity, mode), func(t *testing.T) {
					captured := make(chan []byte, 1)
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						raw, err := io.ReadAll(r.Body)
						if err != nil {
							t.Error(err)
						}
						captured <- raw
						if stream {
							w.Header().Set("Content-Type", "text/event-stream")
							fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg222\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-3-5-haiku\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
						} else {
							w.Header().Set("Content-Type", "application/json")
							fmt.Fprint(w, `{"id":"msg222","type":"message","role":"assistant","model":"claude-3-5-haiku","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`)
						}
					}))
					defer server.Close()
					cfg := &config.Config{}
					key := "sk-ant-api03-serving222"
					auth := &cliproxyauth.Auth{ID: t.Name(), ProxyURL: "direct", Attributes: map[string]string{"api_key": key, "base_url": server.URL, cliproxyauth.ClaudeDeviceIDAttributeKey: saved}}
					if mode == "oauth" {
						auth = directClaudeOAuthAuth()
						auth.ID = t.Name()
						auth.ProxyURL = "direct"
						key = auth.Attributes["api_key"]
						auth.Attributes["base_url"] = server.URL
						auth.Attributes[cliproxyauth.ClaudeDeviceIDAttributeKey] = saved
					}
					headers := http.Header{}
					switch mode {
					case "default":
					case "native":
						headers.Set("User-Agent", "claude-cli/2.1.211 (external, cli)")
					case "cli-profile":
						auth.Attributes["fingerprint_profile"] = "claude-code-cli"
						auth.Attributes["cloak_mode"] = "uncloak"
					case "oauth", "config-always":
						auth.Attributes["cloak_mode"] = "uncloak"
					default:
						auth.Attributes["cloak_mode"] = mode
					}
					if mode == "config-always" {
						cfg.ClaudeKey = []config.ClaudeKey{{APIKey: key, BaseURL: server.URL, Cloak: &config.CloakConfig{Mode: "always"}}}
					}
					meta := ""
					if identity {
						meta = `,"metadata":{"user_id":"{\"device_id\":\"` + caller + `\",\"account_uuid\":\"account222\",\"session_id\":\"75d661c0-0394-49a9-9bd9-84718c20ba71\"}"}`
					}
					literal := fmt.Sprintf(`{"model":"claude-3-5-haiku","stream":%v,"max_tokens":32,"system":[{"type":"text","text":"caller system","cache_control":{"type":"ephemeral"}}],"tools":[{"name":"caller_tool","description":"caller tool","input_schema":{"type":"object","properties":{}},"cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":[{"type":"text","text":"hello","cache_control":{"type":"ephemeral"}}]}]%s}`, stream, meta)
					exec := NewClaudeExecutor(cfg)
					req := cliproxyexecutor.Request{Model: "claude-3-5-haiku", Payload: []byte(literal)}
					opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, Headers: headers}
					if stream {
						res, err := exec.ExecuteStream(context.Background(), auth, req, opts)
						if err != nil {
							t.Fatal(err)
						}
						for chunk := range res.Chunks {
							if chunk.Err != nil {
								t.Fatal(chunk.Err)
							}
						}
					} else {
						if _, err := exec.Execute(context.Background(), auth, req, opts); err != nil {
							t.Fatal(err)
						}
					}
					got := <-captured
					if mode == "uncloak" {
						var actual, expected any
						if err := json.Unmarshal(got, &actual); err != nil {
							t.Fatal(err)
						}
						if err := json.Unmarshal([]byte(literal), &expected); err != nil {
							t.Fatal(err)
						}
						if !reflect.DeepEqual(actual, expected) {
							t.Fatalf("caller-owned full body changed:\ngot %s\nwant %s", got, literal)
						}
					} else {
						inner := gjson.GetBytes(got, "metadata.user_id").String()
						if gjson.Get(inner, "device_id").String() != saved {
							t.Fatalf("%s lost fixed saved identity: %s", mode, got)
						}
						if strings.Contains(inner, caller) {
							t.Fatal("caller identity escaped binding")
						}
					}
				})
			}
		}
	}
}
