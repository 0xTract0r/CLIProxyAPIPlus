package executor

import (
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"

	"testing"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
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

func TestCodexBaseModelNameStripsSuffix(t *testing.T) {
	t.Parallel()
	if got := thinking.ParseSuffix("gpt-5.6(high)").ModelName; got != "gpt-5.6" {
		t.Fatalf("thinking.ParseSuffix(gpt-5.6(high)).ModelName = %q, want gpt-5.6", got)
	}
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"fast_models": "gpt-5.6"}}
	if !codexFastEnabled(auth, thinking.ParseSuffix("gpt-5.6(high)").ModelName) {
		t.Fatal("fast should be enabled for base model gpt-5.6 when request model is gpt-5.6(high)")
	}
}

func receiveFrame(t *testing.T, ch chan []byte, label string) []byte {
	t.Helper()
	select {
	case payload := <-ch:
		return payload
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s frame", label)
		return nil
	}
}
