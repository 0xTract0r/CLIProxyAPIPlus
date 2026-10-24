package executor

import (
	"context"
	"encoding/json"
	"fmt"
	xx "github.com/pierrec/xxHash/xxHash64"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	authpkg "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	ex "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	trans "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

func serving226(t *testing.T, stream bool, cfg *config.Config, auth *authpkg.Auth, literal string) ([]byte, http.Header) {
	t.Helper()
	type captured struct {
		body    []byte
		headers http.Header
	}
	ch := make(chan captured, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		ch <- captured{body, r.Header.Clone()}
		if stream {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg226\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-3-5-haiku\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
		} else {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"id":"msg226","type":"message","role":"assistant","model":"claude-3-5-haiku","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`)
		}
	}))
	defer server.Close()
	auth.ProxyURL = "direct"
	auth.Attributes["base_url"] = server.URL
	cfg.ClaudeKey[0].BaseURL = server.URL
	e := NewClaudeExecutor(cfg)
	req := ex.Request{Model: "claude-3-5-haiku", Payload: []byte(literal)}
	opts := ex.Options{SourceFormat: trans.FormatClaude}
	if stream {
		res, err := e.ExecuteStream(context.Background(), auth, req, opts)
		if err != nil {
			t.Fatal(err)
		}
		for c := range res.Chunks {
			if c.Err != nil {
				t.Fatal(c.Err)
			}
		}
	} else {
		if _, err := e.Execute(context.Background(), auth, req, opts); err != nil {
			t.Fatal(err)
		}
	}
	got := <-ch
	return got.body, got.headers
}
func literal226(stream bool, system string) string {
	return fmt.Sprintf(`{"model":"claude-3-5-haiku","stream":%v,"max_tokens":32,"system":[{"type":"text","text":%q,"cache_control":{"type":"ephemeral"}}],"tools":[{"name":"caller_tool","description":"caller tool","input_schema":{"type":"object","properties":{}},"cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":[{"type":"text","text":"literal cch=00000 in caller text","cache_control":{"type":"ephemeral"}}]}]}`, stream, system)
}
func TestClaudeUncloak226ExperimentalCannotRewriteCallerBody(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, experimental := range []bool{false, true} {
			for _, billing := range []bool{false, true} {
				t.Run(fmt.Sprintf("stream=%v/exp=%v/billing=%v", stream, experimental, billing), func(t *testing.T) {
					key := "sk-ant-api03-owner226"
					auth := &authpkg.Auth{ID: t.Name(), Attributes: map[string]string{"api_key": key, "cloak_mode": "uncloak"}}
					cfg := &config.Config{ClaudeKey: []config.ClaudeKey{{APIKey: key, ExperimentalCCHSigning: experimental}}}
					system := "caller system"
					if billing {
						system = "x-anthropic-billing-header: cc_version=2.1.60; cc_entrypoint=sdk-cli; cch=abc12; cc_prompt_id=00000000-0000-4000-8000-000000000001;"
					}
					literal := literal226(stream, system)
					got, _ := serving226(t, stream, cfg, auth, literal)
					var actual, expected any
					if err := json.Unmarshal(got, &actual); err != nil {
						t.Fatal(err)
					}
					if err := json.Unmarshal([]byte(literal), &expected); err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(actual, expected) {
						t.Fatalf("caller-owned full body changed: got %s want %s", got, literal)
					}
				})
			}
		}
	}
}
func TestClaudeUncloak226RetainsOtherModesFinalCCH(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, mode := range []string{"default", "auto", "never", "always", "cli-profile", "oauth", "config-always"} {
			t.Run(fmt.Sprintf("stream=%v/%s", stream, mode), func(t *testing.T) {
				key := "sk-ant-api03-sign226"
				auth := &authpkg.Auth{ID: t.Name(), Attributes: map[string]string{"api_key": key}}
				experimental := true
				if mode == "oauth" {
					auth = directClaudeOAuthAuth()
					auth.ID = t.Name()
					key = auth.Attributes["api_key"]
					experimental = false
					auth.Attributes["cloak_mode"] = "uncloak"
				} else if mode == "cli-profile" {
					auth.Attributes["fingerprint_profile"] = "claude-code-cli"
					auth.Attributes["cloak_mode"] = "uncloak"
				} else if mode == "config-always" {
					auth.Attributes["cloak_mode"] = "uncloak"
				} else if mode != "default" {
					auth.Attributes["cloak_mode"] = mode
				}
				cfg := &config.Config{ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{UserAgent: "claude-cli/2.1.211 (external, cli)", PinSoftwareBaseline: true}, ClaudeKey: []config.ClaudeKey{{APIKey: key, ExperimentalCCHSigning: experimental}}}
				if mode == "config-always" {
					cfg.ClaudeKey[0].Cloak = &config.CloakConfig{Mode: "always"}
				}
				got, headers := serving226(t, stream, cfg, auth, literal226(stream, "caller system"))
				if !strings.Contains(headers.Get("User-Agent"), "2.1.211") {
					t.Fatalf("expected pinned legacy hash fingerprint: %s", headers.Get("User-Agent"))
				}
				// 从实际终字节独立复算固定F算法；不调用生产sign/normalize/placeholder helper。
				re := regexp.MustCompile(`cch=([0-9a-f]{5});`)
				loc := re.FindSubmatchIndex(got)
				if loc == nil {
					t.Fatalf("mode %s lost actual CCH: %s", mode, got)
				}
				unsigned := append([]byte(nil), got...)
				copy(unsigned[loc[2]:loc[3]], "00000")
				expected := fmt.Sprintf("%05x", xx.Checksum(unsigned, uint64(0x6E52736AC806831E))&0xfffff)
				if string(got[loc[2]:loc[3]]) != expected {
					t.Fatalf("final wire CCH=%s want independent fixed-seed %s", got[loc[2]:loc[3]], expected)
				}
			})
		}
	}
}
