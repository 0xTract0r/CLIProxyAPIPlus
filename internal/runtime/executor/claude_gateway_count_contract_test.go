package executor

import (
	"bytes"
	"context"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func captureGatewayCountContract(t *testing.T, cfg *config.Config, auth *cliproxyauth.Auth, payload []byte, headers http.Header) ([]byte, string) {
	t.Helper()
	captured := make(chan struct {
		body []byte
		ua   string
	}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages/count_tokens" {
			t.Errorf("wrong endpoint %s", r.URL.Path)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		captured <- struct {
			body []byte
			ua   string
		}{body, r.Header.Get("User-Agent")}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"input_tokens":42}`)
	}))
	defer server.Close()
	auth.ProxyURL = "direct"
	auth.Attributes["base_url"] = server.URL
	if _, err := NewClaudeExecutor(cfg).CountTokens(context.Background(), auth, cliproxyexecutor.Request{Model: "claude-3-5-sonnet-20241022", Payload: payload}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, Headers: headers}); err != nil {
		t.Fatal(err)
	}
	got := <-captured
	return got.body, got.ua
}
func TestClaudeGatewayCountLegacySHAGoldenPreimage(t *testing.T) {
	for _, tc := range []struct{ system, payload, want string }{{system: "rule", payload: `{"model":"claude-3-5-sonnet-20241022","system":"rule","messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]}`, want: "2f304"}, {system: "other rule", payload: `{"model":"claude-3-5-sonnet-20241022","system":"other rule","messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]}`, want: "dd1be"}} {
		t.Run(tc.system, func(t *testing.T) {
			resetClaudeDeviceProfileCache()
			auth := &cliproxyauth.Auth{ID: t.Name(), Provider: "claude", Attributes: map[string]string{"api_key": "plain-key"}}
			body, _ := captureGatewayCountContract(t, &config.Config{}, auth, []byte(tc.payload), nil)
			if got := cchFromBody(t, body); got != tc.want {
				t.Fatalf("legacy SHA=%s want frozen F-preimage %s", got, tc.want)
			}
			if gjson.GetBytes(body, "metadata").Exists() {
				t.Fatal("Count fabricated metadata")
			}
			if got := gjson.GetBytes(body, "system.2.text").String(); got != tc.system {
				t.Fatalf("effective caller=%q want %q", got, tc.system)
			}
		})
	}
}
func TestClaudeGatewayCountKeepsModeAndNativeProfileBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, mode                   string
		native, profile, pin, apiKey bool
		wantCCH                      bool
	}{
		{name: "oauth_auto", wantCCH: true},
		{name: "oauth_never", mode: "never"},
		{name: "native_auto", native: true, wantCCH: true},
		{name: "native_always", native: true, mode: "always", wantCCH: true},
		{name: "explicit_native_profile", profile: true},
		{name: "explicit_key_native_profile", profile: true, apiKey: true},
		{name: "pinned_220", pin: true, wantCCH: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetClaudeDeviceProfileCache()
			cfg := &config.Config{}
			if tc.pin {
				cfg.ClaudeHeaderDefaults = config.ClaudeHeaderDefaults{UserAgent: "claude-cli/2.1.220 (external, cli)", PinSoftwareBaseline: true}
			}
			auth := &cliproxyauth.Auth{ID: tc.name, Provider: "claude", Attributes: map[string]string{"api_key": "sk-ant-oat-synthetic", "cloak_mode": tc.mode}}
			if tc.apiKey {
				auth.Attributes["api_key"] = "plain-key"
			}
			if tc.profile {
				auth.Attributes["fingerprint_profile"] = "claude-code-cli"
			}
			var headers http.Header
			if tc.native {
				headers = http.Header{"User-Agent": []string{"claude-cli/2.1.211 (external, cli)"}}
			}
			payload := []byte(`{"model":"claude-3-5-sonnet-20241022","system":"PRIVATE_CALLER_RULE","messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]}`)
			if tc.name == "native_auto" {
				payload, _ = sjson.SetRawBytes(payload, "tools", []byte(`[{"name":"caller_tool","input_schema":{"type":"object"}}]`))
			}
			body, ua := captureGatewayCountContract(t, cfg, auth, payload, headers)
			_, hasCCH := claudeBillingCCHDigitsOffset(body)
			if hasCCH != tc.wantCCH {
				t.Fatalf("CCH=%t want %t", hasCCH, tc.wantCCH)
			}
			if tc.wantCCH {
				if tc.name == "native_auto" {
					for _, field := range []string{"messages", "tools"} {
						if gjson.GetBytes(body, field).Raw != gjson.GetBytes(payload, field).Raw {
							t.Fatalf("native auto %s changed", field)
						}
					}
					if !bytes.Contains(body, []byte("PRIVATE_CALLER_RULE")) || bytes.Contains(body, []byte("Use the available tools when needed")) {
						t.Fatal("native auto whole cloak changed caller body")
					}
				} else if bytes.Contains(body, []byte("PRIVATE_CALLER_RULE")) || !bytes.Contains(body, []byte("Use the available tools when needed")) {
					t.Fatal("OAuth effective neutralization lost")
				}
				request, _ := http.NewRequest(http.MethodPost, "http://127.0.0.1", bytes.NewReader(body))
				request.Header.Set("User-Agent", ua)
				signed, err := signClaudeHTTPRequestCCH(request, body)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(signed, body) {
					t.Fatal("Count final CCH is not idempotent under actual UA")
				}
				if tc.pin && !strings.HasPrefix(ua, "claude-cli/2.1.220 ") {
					t.Fatalf("pin UA=%q", ua)
				}
			} else if !tc.profile && gjson.GetBytes(body, "system").String() != "PRIVATE_CALLER_RULE" {
				t.Fatal("native/never caller changed")
			}
			if tc.apiKey && !bytes.Contains(body, []byte("PRIVATE_CALLER_RULE")) {
				t.Fatal("API-key explicit profile lost caller content")
			}
			if gjson.GetBytes(body, "metadata").Exists() {
				t.Fatal("Count fabricated metadata")
			}
		})
	}
}
func TestClaudeFirstPartyCountStillUsesNativeShape(t *testing.T) {
	if !codexRefreshSandboxChild(t) {
		return
	}
	body := countClaudeFirstPartyThroughLoopbackTLS(t, []byte(`{"model":"claude-sonnet-4-6","system":"PRIVATE_CALLER_RULE","messages":[{"role":"user","content":"official count"}]}`))
	if gjson.GetBytes(body, "metadata").Exists() || bytes.Contains(body, []byte("cch=")) || bytes.Contains(body, []byte("x-anthropic-billing-header:")) {
		t.Fatal("official native Count gained serving identity")
	}
	if !bytes.Contains(body, []byte("official count")) {
		t.Fatal("official Count lost user content")
	}
}

func TestClaudeGatewayCountLegacySHAUsesRewrittenDeviceIdentity(t *testing.T) {
	resetClaudeDeviceProfileCache()
	cfg := &config.Config{}
	auth := &cliproxyauth.Auth{ID: t.Name(), Provider: "claude", Attributes: map[string]string{"api_key": "plain-key"}}
	payload := []byte(`{"model":"claude-3-5-sonnet-20241022","system":"rule","messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}],"metadata":{"user_id":"{\"device_id\":\"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\",\"account_uuid\":\"\",\"session_id\":\"75d661c0-0394-49a9-9bd9-84718c20ba71\"}"}}`)
	body, _ := captureGatewayCountContract(t, cfg, auth, payload, nil)
	rewritten := helps.InjectAccountDeviceIDWithOptions(payload, cfg.AuthDir, auth, "plain-key", false)
	if string(rewritten) == string(payload) {
		t.Fatal("fixture failed to trigger account identity rewrite")
	}
	if strings.Contains(gjson.GetBytes(body, "metadata.user_id").String(), strings.Repeat("a", 64)) {
		t.Fatal("wire did not rewrite device ID")
	}
	want := claudeLegacyCountPayloadCCH(rewritten, false)
	got := cchFromBody(t, body)
	if got != want {
		t.Fatalf("legacy F preimage captured before account rewrite: got=%s want(after identity)=%s raw-input=%s", got, want, claudeLegacyCountPayloadCCH(payload, false))
	}
}
