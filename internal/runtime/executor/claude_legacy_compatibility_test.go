package executor

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestClaudeLegacyHeadersKeepExplicitProfileAndPinBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, key                        string
		pin, profile                     bool
		wantUA, wantPackage, wantRuntime string
	}{
		{name: "default_api_key", key: "synthetic-key", wantUA: "claude-cli/2.1.62 (external, cli)", wantPackage: "0.74.0", wantRuntime: "v24.3.0"},
		{name: "default_oauth", key: "sk-ant-oat01-synthetic", wantUA: "claude-cli/2.1.62 (external, cli)", wantPackage: "0.74.0", wantRuntime: "v24.3.0"},
		{name: "explicit_pin", key: "synthetic-key", pin: true, wantUA: "claude-cli/2.1.60 (external, cli)", wantPackage: "0.70.0", wantRuntime: "v22.0.0"},
		{name: "explicit_native_profile", key: "synthetic-key", profile: true, wantUA: "claude-cli/2.1.60 (external, cli)", wantPackage: "0.70.0", wantRuntime: "v22.0.0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetClaudeDeviceProfileCache()
			off := false
			cfg := &config.Config{ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{UserAgent: "claude-cli/2.1.60 (external, cli)", PackageVersion: "0.70.0", RuntimeVersion: "v22.0.0", StabilizeDeviceProfile: &off, PinSoftwareBaseline: tc.pin}}
			auth := &cliproxyauth.Auth{ID: tc.name, ProxyURL: "direct", Attributes: map[string]string{"api_key": tc.key}}
			if tc.profile {
				auth.Attributes["fingerprint_profile"] = "claude-code-cli"
			}
			req := newClaudeHeaderTestRequest(t, http.Header{"User-Agent": []string{"claude-cli/2.1.62 (external, cli)"}, "X-Stainless-Package-Version": []string{"0.74.0"}, "X-Stainless-Runtime-Version": []string{"v24.3.0"}, "X-Stainless-Os": []string{"Linux"}, "X-Stainless-Arch": []string{"x64"}})
			if err := applyClaudeHeaders(req, auth, tc.key, false, nil, nil, cfg, nil, false); err != nil {
				t.Fatal(err)
			}
			assertClaudeFingerprint(t, req.Header, tc.wantUA, tc.wantPackage, tc.wantRuntime, "Linux", "x64")
		})
	}
}

func TestClaudeStrictLegacyTemplateAndModernPolicyBoundaries(t *testing.T) {
	wantStatic := strings.Join([]string{helps.ClaudeCodeIntro, helps.ClaudeCodeSystem, helps.ClaudeCodeDoingTasks, helps.ClaudeCodeToneAndStyle, helps.ClaudeCodeOutputEfficiency}, "\n\n")
	for _, tc := range []struct {
		name                                   string
		oauth, strict, pin, profile, stabilize bool
	}{
		{name: "legacy_api_key", strict: true},
		{name: "legacy_oauth", strict: true, oauth: true},
		{name: "explicit_pin", strict: true, pin: true},
		{name: "explicit_native_profile", strict: true, profile: true},
		{name: "stabilization_enabled", strict: true, stabilize: true},
		{name: "ordinary_api_key"},
		{name: "ordinary_oauth", oauth: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetClaudeDeviceProfileCache()
			key := "synthetic-key"
			if tc.oauth {
				key = "sk-ant-oat01-synthetic"
			}
			entry := config.ClaudeKey{APIKey: key, Cloak: &config.CloakConfig{StrictMode: tc.strict}}
			if tc.profile {
				entry.FingerprintProfile = "claude-code-cli"
			}
			cfg := &config.Config{ClaudeKey: []config.ClaudeKey{entry}, ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{PinSoftwareBaseline: tc.pin, StabilizeDeviceProfile: &tc.stabilize}}
			auth := &cliproxyauth.Auth{ID: tc.name, ProxyURL: "direct", Attributes: map[string]string{"api_key": key}}
			payload := []byte(`{"model":"claude-3-5-sonnet-20241022","system":"private caller policy sentinel","messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]}`)
			out, cloaked, err := applyCloaking(context.Background(), cfg, auth, payload, key, false, true)
			if err != nil {
				t.Fatal(err)
			}
			if !cloaked {
				t.Fatal("expected cloaking")
			}
			legacy := tc.strict && !tc.pin && !tc.profile && !tc.stabilize
			if legacy {
				if got := gjson.GetBytes(out, "system.#").Int(); got != 3 {
					t.Fatalf("system blocks=%d", got)
				}
				if got := gjson.GetBytes(out, "system.1.text").String(); got != "You are Claude Code, Anthropic's official CLI for Claude." {
					t.Fatalf("legacy identity=%q", got)
				}
				if got := gjson.GetBytes(out, "system.2.text").String(); got != wantStatic {
					t.Fatal("legacy template differs from retained fork constants")
				}
				for _, block := range gjson.GetBytes(out, "system").Array() {
					if block.Get("cache_control").Exists() {
						t.Fatal("legacy system block unexpectedly gained cache_control")
					}
				}
				if got := gjson.GetBytes(out, "messages.0.content.#").Int(); got != 1 {
					t.Fatalf("legacy user blocks=%d", got)
				}
				if got := gjson.GetBytes(out, "messages.0.content.0.text").String(); got != "hello" {
					t.Fatalf("legacy user text=%q", got)
				}
			} else {
				wantBlocks := int64(2)
				if !tc.strict && !tc.oauth {
					wantBlocks = 3 // 普通 API key 保留 caller system 为独立顶层块。
					if got := gjson.GetBytes(out, "system.2.text").String(); got != "private caller policy sentinel" {
						t.Fatalf("caller system block = %q", got)
					}
				}
				if got := gjson.GetBytes(out, "system.#").Int(); got != wantBlocks {
					t.Fatalf("modern system blocks=%d, want %d", got, wantBlocks)
				}
				for _, block := range gjson.GetBytes(out, "system").Array() {
					if block.Get("text").String() == wantStatic {
						t.Fatal("legacy template leaked into modern policy")
					}
				}
				content := gjson.GetBytes(out, "messages.0.content").Array()
				if len(content) != 2 {
					t.Fatalf("modern user blocks=%d", len(content))
				}
				assertClaudeCodeCurrentDateBlock(t, content[0])
				if got := content[1].Get("cache_control.type").String(); got != "ephemeral" {
					t.Fatalf("modern user cache=%q", got)
				}
			}
			containsCaller := bytes.Contains(out, []byte("private caller policy sentinel"))
			if tc.strict || tc.oauth {
				if containsCaller {
					t.Fatal("strict/OAuth caller policy leaked")
				}
			} else if !containsCaller {
				t.Fatal("ordinary API-key caller policy lost")
			}
			if !tc.strict && tc.oauth && !bytes.Contains(out, []byte("Use the available tools when needed")) {
				t.Fatal("OAuth neutral reminder lost")
			}
			if _, ok := claudeBillingCCHDigitsOffset(out); !ok {
				t.Fatal("billing CCH placeholder missing")
			}
			signed, err := finalizeAnthropicMessagesBodyCCH(out, "")
			if err != nil {
				t.Fatal(err)
			}
			resigned, err := finalizeAnthropicMessagesBodyCCH(signed, "")
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(signed, resigned) {
				t.Fatal("final CCH not byte-idempotent")
			}
			mutated, err := sjson.SetBytes(signed, "messages.0.content.0.text", "changed final user bytes")
			if err != nil {
				t.Fatal(err)
			}
			updated, err := finalizeAnthropicMessagesBodyCCH(mutated, "")
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Equal(mutated, updated) {
				t.Fatal("final CCH did not respond to body mutation")
			}
		})
	}
}
