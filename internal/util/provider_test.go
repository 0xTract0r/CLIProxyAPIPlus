package util_test

import (
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
)

// TestMaskSensitiveQueryMasksValue covers the related half of issue #6666:
// deleting a client API key by value passes the secret as ?value=, so the
// request logger must mask it like other secret-bearing parameters.
func TestMaskSensitiveQueryMasksValue(t *testing.T) {
	secretKey := "sk-secret-client-key"

	t.Run("value parameter is masked", func(t *testing.T) {
		masked := util.MaskSensitiveQuery("value=" + secretKey)
		if strings.Contains(masked, secretKey) {
			t.Fatalf("value parameter leaked in full: %s", masked)
		}
		if masked != "value=sk-s...-key" {
			t.Fatalf("masked=%q want the HideAPIKey form", masked)
		}
	})

	t.Run("index parameter is untouched", func(t *testing.T) {
		masked := util.MaskSensitiveQuery("index=3")
		if masked != "index=3" {
			t.Fatalf("masked=%q want %q", masked, "index=3")
		}
	})

	t.Run("known secret parameters stay masked", func(t *testing.T) {
		masked := util.MaskSensitiveQuery("key=" + secretKey + "&access_token=" + secretKey)
		if strings.Contains(masked, secretKey) {
			t.Fatalf("secret parameter leaked in full: %s", masked)
		}
	})
}

func TestGetProviderNameFallsBackToClaudePrefix(t *testing.T) {
	providers := util.GetProviderName("claude-opus-4-7")
	if len(providers) != 1 || providers[0] != "claude" {
		t.Fatalf("expected claude fallback for claude-opus-4-7, got %v", providers)
	}
}

func TestGetProviderNameDoesNotGuessGeminiPrefixedClaudeAliases(t *testing.T) {
	providers := util.GetProviderName("gemini-claude-opus-4-7-thinking")
	if len(providers) != 0 {
		t.Fatalf("expected no heuristic provider for gemini-prefixed alias without registry entry, got %v", providers)
	}
}
