package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestClaudeCustomLegacyDatePolicyBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, provider, endpoint              string
		oauth, pin, profile, stable, wantDate bool
	}{
		{name: "legacy_empty_provider", endpoint: "http://127.0.0.1:1"},
		{name: "legacy_claude_provider", provider: "claude", endpoint: "http://127.0.0.1:1"},
		{name: "kimi_unchanged", provider: "kimi", endpoint: "http://127.0.0.1:1", wantDate: true},
		{name: "official_unchanged", provider: "claude", endpoint: "https://api.anthropic.com", wantDate: true},
		{name: "default_endpoint_unchanged", provider: "claude", wantDate: true},
		{name: "oauth_unchanged", provider: "claude", endpoint: "http://127.0.0.1:1", oauth: true, wantDate: true},
		{name: "pin_unchanged", provider: "claude", endpoint: "http://127.0.0.1:1", pin: true, wantDate: true},
		{name: "native_profile_unchanged", provider: "claude", endpoint: "http://127.0.0.1:1", profile: true, wantDate: true},
		{name: "stabilization_unchanged", provider: "claude", endpoint: "http://127.0.0.1:1", stable: true, wantDate: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetClaudeDeviceProfileCache()
			key := "synthetic-key"
			if tc.oauth {
				key = "sk-ant-oat01-synthetic"
			}
			cfg := &config.Config{ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{PinSoftwareBaseline: tc.pin, StabilizeDeviceProfile: &tc.stable}}
			auth := &cliproxyauth.Auth{ID: tc.name, Provider: tc.provider, ProxyURL: "direct", Attributes: map[string]string{"api_key": key, "base_url": tc.endpoint}}
			if tc.profile {
				auth.Attributes["fingerprint_profile"] = "claude-code-cli"
			}
			payload := []byte(`{"model":"claude-3-5-sonnet-20241022","messages":[{"role":"user","content":[{"type":"text","text":"literal cch=00000"}]}]}`)
			out, cloaked, err := applyCloaking(context.Background(), cfg, auth, payload, key, false, false)
			if err != nil {
				t.Fatal(err)
			}
			if !cloaked {
				t.Fatal("expected normal auto cloak")
			}
			content := gjson.GetBytes(out, "messages.0.content").Array()
			wantIndex := 0
			if tc.wantDate {
				wantIndex = 1
				if len(content) != 2 {
					t.Fatalf("content count=%d", len(content))
				}
				assertClaudeCodeCurrentDateBlock(t, content[0])
			} else if len(content) != 1 {
				t.Fatalf("legacy content count=%d", len(content))
			}
			if got := content[wantIndex].Get("text").String(); got != "literal cch=00000" {
				t.Fatalf("caller literal changed: %q", got)
			}
			if got := gjson.GetBytes(out, "system.0.text").String(); !strings.HasPrefix(got, "x-anthropic-billing-header:") {
				t.Fatalf("billing contract lost: %q", got)
			}
		})
	}
}

func TestClaudeCustomLegacyPreservesCallerDate(t *testing.T) {
	for _, shape := range []string{"string", "blocks"} {
		t.Run(shape, func(t *testing.T) {
			auth := &cliproxyauth.Auth{Provider: "claude", ProxyURL: "direct", Attributes: map[string]string{"api_key": "synthetic-key", "base_url": "http://127.0.0.1:1"}}
			reminder := claudeCodeCurrentDateReminder("2020-01-02")
			payload := []byte(`{"model":"claude-3-5-sonnet-20241022","messages":[{"role":"user","content":""}]}`)
			var err error
			if shape == "string" {
				payload, err = sjson.SetBytes(payload, "messages.0.content", reminder+"caller original")
			} else {
				payload, err = sjson.SetBytes(payload, "messages.0.content", []map[string]string{{"type": "text", "text": reminder}, {"type": "text", "text": "caller original"}})
			}
			if err != nil {
				t.Fatal(err)
			}
			out, _, err := applyCloaking(context.Background(), &config.Config{}, auth, payload, "synthetic-key", false, false)
			if err != nil {
				t.Fatal(err)
			}
			if gjson.GetBytes(out, "messages").Raw != gjson.GetBytes(payload, "messages").Raw {
				t.Fatal("caller date or message shape changed")
			}
		})
	}
}

func TestClaudeCustomExplicitUncloakKeepsCallerBodyWithoutExperimentalSigning(t *testing.T) {
	captured := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		captured <- body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_fixture","type":"message","model":"claude-3-5-sonnet","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer server.Close()
	auth := &cliproxyauth.Auth{Provider: "claude", ProxyURL: "direct", Attributes: map[string]string{"api_key": "synthetic-key", "base_url": server.URL, "cloak_mode": "uncloak"}}
	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"literal cch=00000"}]}]}`)
	_, err := NewClaudeExecutor(&config.Config{}).Execute(context.Background(), auth, cliproxyexecutor.Request{Model: "claude-3-5-sonnet-20241022", Payload: payload}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude})
	if err != nil {
		t.Fatal(err)
	}
	body := <-captured
	if strings.Contains(string(body), "x-anthropic-billing-header:") {
		t.Fatal("explicit caller-owned path gained billing")
	}
	if _, ok := claudeBillingCCHDigitsOffset(body); ok {
		t.Fatal("explicit caller-owned path gained CCH")
	}
	if got := gjson.GetBytes(body, "messages.0.content.#").Int(); got != 1 {
		t.Fatalf("caller content count=%d", got)
	}
	if got := gjson.GetBytes(body, "messages.0.content.0.text").String(); got != "literal cch=00000" {
		t.Fatalf("caller text=%q", got)
	}
}
