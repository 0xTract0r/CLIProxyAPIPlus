package helps

import (
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func resetCodexClientProfileCache() {
	codexClientProfileCacheMu.Lock()
	codexClientProfileCache = make(map[string]codexClientProfileCacheEntry)
	codexClientProfileCacheMu.Unlock()
}

func TestResolveCodexClientProfile_AdoptsFirstObservedProfileThenOnlyBumpsVersionMarkers(t *testing.T) {
	resetCodexClientProfileCache()

	auth := &cliproxyauth.Auth{
		ID:       "codex-profile-auth",
		Provider: "codex",
	}
	cfg := &config.Config{}

	firstProfile := ResolveCodexClientProfile(auth, http.Header{
		"User-Agent": []string{"codex_cli_rs/0.124.0 (Mac OS 15.5.0; arm64) iTerm.app/3.5.0"},
		"Version":    []string{"0.124.0"},
		"Originator": []string{"codex_cli_rs"},
	}, cfg)

	if got := firstProfile.UserAgent; got != "codex_cli_rs/0.124.0 (Mac OS 15.5.0; arm64) iTerm.app/3.5.0" {
		t.Fatalf("first profile User-Agent = %q", got)
	}
	if got := firstProfile.Originator; got != "codex_cli_rs" {
		t.Fatalf("first profile Originator = %q, want %q", got, "codex_cli_rs")
	}
	if got := firstProfile.Version; got != "0.124.0" {
		t.Fatalf("first profile Version = %q, want %q", got, "0.124.0")
	}

	upgradedProfile := ResolveCodexClientProfile(auth, http.Header{
		"User-Agent": []string{"codex_cli_rs/0.125.0 (Mac OS 15.6.0; arm64) Ghostty/1.0.0"},
		"Version":    []string{"0.125.0"},
		"Originator": []string{"codex_cli_rs"},
	}, cfg)

	if got := upgradedProfile.Version; got != "0.125.0" {
		t.Fatalf("upgraded profile Version = %q, want %q", got, "0.125.0")
	}
	if got := upgradedProfile.Originator; got != "codex_cli_rs" {
		t.Fatalf("upgraded profile Originator = %q, want %q", got, "codex_cli_rs")
	}
	if strings.Contains(upgradedProfile.UserAgent, "Ghostty/1.0.0") {
		t.Fatalf("upgraded User-Agent unexpectedly changed terminal fingerprint: %q", upgradedProfile.UserAgent)
	}
	if strings.Contains(upgradedProfile.UserAgent, "Mac OS 15.6.0") {
		t.Fatalf("upgraded User-Agent unexpectedly changed platform fingerprint: %q", upgradedProfile.UserAgent)
	}
	if !strings.Contains(upgradedProfile.UserAgent, "codex_cli_rs/0.125.0") {
		t.Fatalf("upgraded User-Agent did not bump version marker: %q", upgradedProfile.UserAgent)
	}
	if !strings.Contains(upgradedProfile.UserAgent, "Mac OS 15.5.0; arm64") {
		t.Fatalf("upgraded User-Agent lost pinned platform fingerprint: %q", upgradedProfile.UserAgent)
	}
	if !strings.Contains(upgradedProfile.UserAgent, "iTerm.app/3.5.0") {
		t.Fatalf("upgraded User-Agent lost pinned terminal fingerprint: %q", upgradedProfile.UserAgent)
	}
}

func TestCodexManagedHeaders_IncludeStructuredVersionAndOriginator(t *testing.T) {
	headers := CodexManagedHeaders(CodexClientProfile{
		UserAgent:    "codex-tui/0.124.0 (Mac OS 26.3.1; arm64) iTerm.app/3.6.9 (codex-tui; 0.124.0)",
		Version:      "0.124.0",
		Originator:   "codex-tui",
		BetaFeatures: "feature-a",
	})

	if got := headers["Version"]; got != "0.124.0" {
		t.Fatalf("Version = %q, want %q", got, "0.124.0")
	}
	if got := headers["Originator"]; got != "codex-tui" {
		t.Fatalf("Originator = %q, want %q", got, "codex-tui")
	}
	if got := headers["User-Agent"]; got == "" {
		t.Fatal("User-Agent should not be empty")
	}
}
