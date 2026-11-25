package amp

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestMatchModelExplicitTarget(t *testing.T) {

	mappings := []config.AmpModelMapping{
		{From: "claude-opus-4.5", To: "claude-sonnet-4"},
	}

	// Mapping is explicit; validation of the target occurs at dispatch.
	result := MatchModel("claude-opus-4.5", mappings)
	if result != "claude-sonnet-4" {
		t.Errorf("Expected claude-sonnet-4, got %s", result)
	}
}

func TestModelMapper_MapModel_TargetWithThinkingSuffix(t *testing.T) {

	mappings := []config.AmpModelMapping{
		{From: "gpt-5.2-alias", To: "gpt-5.2(xhigh)"},
	}

	result := MatchModel("gpt-5.2-alias", mappings)
	if result != "gpt-5.2(xhigh)" {
		t.Errorf("Expected gpt-5.2(xhigh), got %s", result)
	}
}

func TestModelMapper_MapModel_CaseInsensitive(t *testing.T) {

	mappings := []config.AmpModelMapping{
		{From: "Claude-Opus-4.5", To: "claude-sonnet-4"},
	}

	// Should match case-insensitively
	result := MatchModel("claude-opus-4.5", mappings)
	if result != "claude-sonnet-4" {
		t.Errorf("Expected claude-sonnet-4, got %s", result)
	}
}

func TestModelMapper_MapModel_NotFound(t *testing.T) {
	mappings := []config.AmpModelMapping{
		{From: "claude-opus-4.5", To: "claude-sonnet-4"},
	}

	// Unknown model should return empty
	result := MatchModel("unknown-model", mappings)
	if result != "" {
		t.Errorf("Expected empty for unknown model, got %s", result)
	}
}

func TestModelMapper_MapModel_EmptyInput(t *testing.T) {
	mappings := []config.AmpModelMapping{
		{From: "claude-opus-4.5", To: "claude-sonnet-4"},
	}

	result := MatchModel("", mappings)
	if result != "" {
		t.Errorf("Expected empty for empty input, got %s", result)
	}
}

func TestModelMapper_Regex_MatchBaseWithoutParens(t *testing.T) {

	mappings := []config.AmpModelMapping{
		{From: "^gpt-5$", To: "gemini-2.5-pro", Regex: true},
	}

	// Incoming model has reasoning suffix, regex matches base, suffix is preserved
	result := MatchModel("gpt-5(high)", mappings)
	if result != "gemini-2.5-pro(high)" {
		t.Errorf("Expected gemini-2.5-pro(high), got %s", result)
	}
}

func TestModelMapper_Regex_ExactPrecedence(t *testing.T) {

	mappings := []config.AmpModelMapping{
		{From: "gpt-5", To: "claude-sonnet-4"},                 // exact
		{From: "^gpt-5.*$", To: "gemini-2.5-pro", Regex: true}, // regex
	}

	// Exact match should win over regex
	result := MatchModel("gpt-5", mappings)
	if result != "claude-sonnet-4" {
		t.Errorf("Expected claude-sonnet-4, got %s", result)
	}
}

func TestModelMapper_Regex_InvalidPattern_Skipped(t *testing.T) {
	// Invalid regex should be skipped and not cause panic
	mappings := []config.AmpModelMapping{
		{From: "(", To: "target", Regex: true},
	}

	result := MatchModel("anything", mappings)
	if result != "" {
		t.Errorf("Expected empty result due to invalid regex, got %s", result)
	}
}

func TestModelMapper_Regex_CaseInsensitive(t *testing.T) {

	mappings := []config.AmpModelMapping{
		{From: "^CLAUDE-OPUS-.*$", To: "claude-sonnet-4", Regex: true},
	}

	result := MatchModel("claude-opus-4.5", mappings)
	if result != "claude-sonnet-4" {
		t.Errorf("Expected claude-sonnet-4, got %s", result)
	}
}

func TestModelMapper_SuffixPreservation(t *testing.T) {

	tests := []struct {
		name     string
		mappings []config.AmpModelMapping
		input    string
		want     string
	}{
		{
			name:     "numeric suffix preserved",
			mappings: []config.AmpModelMapping{{From: "g25p", To: "gemini-2.5-pro"}},
			input:    "g25p(8192)",
			want:     "gemini-2.5-pro(8192)",
		},
		{
			name:     "level suffix preserved",
			mappings: []config.AmpModelMapping{{From: "g25p", To: "gemini-2.5-pro"}},
			input:    "g25p(high)",
			want:     "gemini-2.5-pro(high)",
		},
		{
			name:     "no suffix unchanged",
			mappings: []config.AmpModelMapping{{From: "g25p", To: "gemini-2.5-pro"}},
			input:    "g25p",
			want:     "gemini-2.5-pro",
		},
		{
			name:     "config suffix takes priority",
			mappings: []config.AmpModelMapping{{From: "alias", To: "gemini-2.5-pro(medium)"}},
			input:    "alias(high)",
			want:     "gemini-2.5-pro(medium)",
		},
		{
			name:     "regex with suffix preserved",
			mappings: []config.AmpModelMapping{{From: "^g25.*", To: "gemini-2.5-pro", Regex: true}},
			input:    "g25p(8192)",
			want:     "gemini-2.5-pro(8192)",
		},
		{
			name:     "auto suffix preserved",
			mappings: []config.AmpModelMapping{{From: "g25p", To: "gemini-2.5-pro"}},
			input:    "g25p(auto)",
			want:     "gemini-2.5-pro(auto)",
		},
		{
			name:     "none suffix preserved",
			mappings: []config.AmpModelMapping{{From: "g25p", To: "gemini-2.5-pro"}},
			input:    "g25p(none)",
			want:     "gemini-2.5-pro(none)",
		},
		{
			name:     "case insensitive base lookup with suffix",
			mappings: []config.AmpModelMapping{{From: "G25P", To: "gemini-2.5-pro"}},
			input:    "g25p(high)",
			want:     "gemini-2.5-pro(high)",
		},
		{
			name:     "empty suffix filtered out",
			mappings: []config.AmpModelMapping{{From: "g25p", To: "gemini-2.5-pro"}},
			input:    "g25p()",
			want:     "gemini-2.5-pro",
		},
		{
			name:     "incomplete suffix treated as no suffix",
			mappings: []config.AmpModelMapping{{From: "g25p(high", To: "gemini-2.5-pro"}},
			input:    "g25p(high",
			want:     "gemini-2.5-pro",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MatchModel(tt.input, tt.mappings)
			if got != tt.want {
				t.Errorf("MapModel(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
