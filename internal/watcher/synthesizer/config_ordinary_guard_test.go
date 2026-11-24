package synthesizer

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestOrdinaryConfigSynthesizerExplicitFastModelAllowlist(t *testing.T) {
	synth := NewConfigSynthesizer()
	on, off := true, false
	auths, err := synth.Synthesize(&SynthesisContext{
		Config: &config.Config{
			CodexKey: []config.CodexKey{
				{APIKey: "synthetic-allowed", FastModels: []string{" MODEL-A ", "", "model-a", "MoDeL-B"}, DisableCodexCloaking: &on},
				{APIKey: "synthetic-default"},
				{APIKey: "synthetic-explicit-false", DisableCodexCloaking: &off},
			},
			ClaudeKey: []config.ClaudeKey{{APIKey: "synthetic-claude", FingerprintProfile: "fixture-profile"}},
			XAIKey:    []config.XAIKey{{APIKey: "synthetic-xai", FastModels: []string{"model-a"}}},
			OpenAICompatibility: []config.OpenAICompatibility{
				{Name: "disabled-fixture", BaseURL: "https://example.invalid", Disabled: true},
			},
		},
		IDGenerator: NewStableIDGenerator(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(auths) != 5 {
		t.Fatalf("expected exactly five enabled fixture entries, got %d", len(auths))
	}
	for _, a := range auths {
		if _, ok := a.Attributes["fingerprint_profile"]; ok {
			t.Fatal("configured profile was published to runtime")
		}
		fast, optedOut := a.Attributes["fast_models"], a.Attributes[coreauth.AttributeCodexDisableCloaking]
		if a.Attributes["api_key"] == "synthetic-allowed" {
			if a.Provider != "codex" || fast != "model-a,model-b" || optedOut != "true" {
				t.Fatal("explicit Codex allowlist or safety opt-out lost")
			}
		} else if a.Provider == "codex" {
			if fast != "" || optedOut != "true" {
				t.Fatal("missing fail-closed default for a Codex key")
			}
		} else if fast != "" || optedOut != "" {
			t.Fatal("unconfigured or other provider received a Codex opt-in")
		}
	}
}
