package util

import "testing"

func TestGetProviderNameFallsBackToClaudePrefix(t *testing.T) {
	providers := GetProviderName("claude-opus-4-7")
	if len(providers) != 1 || providers[0] != "claude" {
		t.Fatalf("expected claude fallback for claude-opus-4-7, got %v", providers)
	}
}

func TestGetProviderNameDoesNotGuessGeminiPrefixedClaudeAliases(t *testing.T) {
	providers := GetProviderName("gemini-claude-opus-4-7-thinking")
	if len(providers) != 0 {
		t.Fatalf("expected no heuristic provider for gemini-prefixed alias without registry entry, got %v", providers)
	}
}
