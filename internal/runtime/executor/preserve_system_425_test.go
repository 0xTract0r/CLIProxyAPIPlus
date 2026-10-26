package executor

import (
	"bytes"
	"context"
	claudeauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/claude"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	auth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
	"strings"
	"sync"
	"testing"
)

func TestPreserveSystem425Boundaries(t *testing.T) {
	for _, mode := range []string{"default", "false", "attr", "metadata", "config-true", "config-false", "attr-false", "strict", "farm", "farm-numeric", "farm-bound", "never", "caller-spoof", "apikey", "cli-profile"} {
		t.Run(mode, func(t *testing.T) {
			key := "sk-ant-oat-test425"
			a := &auth.Auth{Provider: "claude", Attributes: map[string]string{"api_key": key, "cloak_mode": "always"}, Metadata: map[string]any{"sibling": "keep"}}
			cfg := &config.Config{}
			want := false
			switch mode {
			case "attr":
				a.Attributes["cloak_preserve_caller_system"] = "true"
				want = true
			case "metadata":
				a.Metadata["cloak_preserve_caller_system"] = "true"
				want = true
			case "false":
				a.Attributes["cloak_preserve_caller_system"] = "false"
			case "config-true", "config-false":
				v := mode == "config-true"
				a.Attributes["cloak_preserve_caller_system"] = "true"
				cfg.ClaudeKey = []config.ClaudeKey{{APIKey: key, Cloak: &config.CloakConfig{PreserveCallerSystem: &v}}}
				want = v
			case "attr-false":
				a.Attributes["cloak_preserve_caller_system"] = "false"
				a.Metadata["cloak_preserve_caller_system"] = "true"
			case "strict":
				a.Attributes["cloak_preserve_caller_system"] = "true"
				a.Attributes["cloak_strict_mode"] = "true"
			case "farm", "farm-numeric":
				a.Attributes["cloak_preserve_caller_system"] = "true"
				a.Metadata[auth.FarmEnrolledMetadataKey] = true
				if mode == "farm-numeric" {
					a.Metadata[auth.FarmEnrolledMetadataKey] = float64(1)
				}
			case "farm-bound":
				a.Attributes["cloak_preserve_caller_system"] = "true"
				a.Attributes[auth.ClaudeDeviceIDAttributeKey] = strings.Repeat("a", 64)
			case "never":
				a.Attributes["cloak_preserve_caller_system"] = "true"
				a.Attributes["cloak_mode"] = "never"
				want = true
			case "apikey", "cli-profile":
				key = "plain425"
				a.Attributes["api_key"] = key
				a.Attributes["cloak_preserve_caller_system"] = "true"
				want = true
				if mode == "cli-profile" {
					a.Attributes["fingerprint_profile"] = "claude-code-cli"
				}
			}
			p := []byte(`{"model":"claude-opus-5","system":"TASK425 use staging only","messages":[{"role":"user","content":"USER425"}],"tools":[{"name":"audit425"}],"cloak_preserve_caller_system":true,"metadata":{"cloak_preserve_caller_system":"true"}}`)
			out, _, err := applyCloaking(context.Background(), cfg, a, p, key, false, true)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(out), "TASK425") != want {
				t.Fatalf("task preservation=%v want=%v", strings.Contains(string(out), "TASK425"), want)
			}
			if !strings.Contains(string(out), "USER425") || gjson.GetBytes(out, "tools.0.name").String() != "audit425" || claudeauth.ReadMetadataString(&a.Metadata, "sibling") != "keep" {
				t.Fatal("sibling/user/tool corrupted")
			}
		})
	}
}
func TestPreserveSystem425MetadataLock(t *testing.T) {
	a := &auth.Auth{Attributes: map[string]string{"api_key": "sk-ant-oat-425", "cloak_mode": "always"}, Metadata: map[string]any{"sibling": "keep"}}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			claudeauth.StoreMetadataString(&a.Metadata, "cloak_preserve_caller_system", "true")
			claudeauth.StoreMetadataValue(&a.Metadata, auth.FarmEnrolledMetadataKey, i%2 == 0)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			resolveClaudeWirePolicy(&config.Config{}, a, "sk-ant-oat-425", false)
		}
	}()
	wg.Wait()
}
func assertPreserve425FinalCCH(t *testing.T, body []byte) {
	t.Helper()
	signed, err := finalizeAnthropicMessagesBodyCCH(body, "")
	if err != nil || !bytes.Equal(signed, body) {
		t.Fatalf("final wire CCH invalid: %v", err)
	}
}
