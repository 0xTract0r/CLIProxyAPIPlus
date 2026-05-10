package auth

import "testing"

func TestToolPrefixDisabled(t *testing.T) {
	var a *Auth
	if a.ToolPrefixDisabled() {
		t.Error("nil auth should return false")
	}

	a = &Auth{}
	if a.ToolPrefixDisabled() {
		t.Error("empty auth should return false")
	}

	a = &Auth{Metadata: map[string]any{"tool_prefix_disabled": true}}
	if !a.ToolPrefixDisabled() {
		t.Error("should return true when set to true")
	}

	a = &Auth{Metadata: map[string]any{"tool_prefix_disabled": "true"}}
	if !a.ToolPrefixDisabled() {
		t.Error("should return true when set to string 'true'")
	}

	a = &Auth{Metadata: map[string]any{"tool-prefix-disabled": true}}
	if !a.ToolPrefixDisabled() {
		t.Error("should return true with kebab-case key")
	}

	a = &Auth{Metadata: map[string]any{"tool_prefix_disabled": false}}
	if a.ToolPrefixDisabled() {
		t.Error("should return false when set to false")
	}
}

func TestRefreshDisabled(t *testing.T) {
	var a *Auth
	if a.RefreshDisabled() {
		t.Error("nil auth should not disable refresh")
	}

	a = &Auth{Metadata: map[string]any{"refresh_disabled": true}}
	if !a.RefreshDisabled() {
		t.Error("refresh_disabled=true should disable refresh")
	}

	a = &Auth{Metadata: map[string]any{"disable_refresh": "true"}}
	if !a.RefreshDisabled() {
		t.Error("disable_refresh=true should disable refresh")
	}

	a = &Auth{Metadata: map[string]any{"refresh_enabled": false}}
	if !a.RefreshDisabled() {
		t.Error("refresh_enabled=false should disable refresh")
	}

	a = &Auth{Metadata: map[string]any{
		"account_settings": map[string]any{"refresh_enabled": false},
	}}
	if !a.RefreshDisabled() {
		t.Error("account_settings.refresh_enabled=false should disable refresh")
	}

	a = &Auth{Metadata: map[string]any{
		"account_settings": map[string]string{"auto_refresh": "false"},
	}}
	if !a.RefreshDisabled() {
		t.Error("account_settings.auto_refresh=false should disable refresh")
	}

	a = &Auth{Attributes: map[string]string{"refresh_disabled": "true"}}
	if !a.RefreshDisabled() {
		t.Error("attribute refresh_disabled=true should disable refresh")
	}

	a = &Auth{Metadata: map[string]any{"refresh_disabled": false, "refresh_enabled": true}}
	if a.RefreshDisabled() {
		t.Error("explicit false disabled keys and true enabled keys should keep refresh enabled")
	}
}

func TestEnsureIndexUsesCredentialIdentity(t *testing.T) {
	t.Parallel()

	geminiAuth := &Auth{
		Provider: "gemini",
		Attributes: map[string]string{
			"api_key": "shared-key",
			"source":  "config:gemini[abc123]",
		},
	}
	compatAuth := &Auth{
		Provider: "bohe",
		Attributes: map[string]string{
			"api_key":      "shared-key",
			"compat_name":  "bohe",
			"provider_key": "bohe",
			"source":       "config:bohe[def456]",
		},
	}
	geminiAltBase := &Auth{
		Provider: "gemini",
		Attributes: map[string]string{
			"api_key":  "shared-key",
			"base_url": "https://alt.example.com",
			"source":   "config:gemini[ghi789]",
		},
	}
	geminiDuplicate := &Auth{
		Provider: "gemini",
		Attributes: map[string]string{
			"api_key": "shared-key",
			"source":  "config:gemini[abc123-1]",
		},
	}

	geminiIndex := geminiAuth.EnsureIndex()
	compatIndex := compatAuth.EnsureIndex()
	altBaseIndex := geminiAltBase.EnsureIndex()
	duplicateIndex := geminiDuplicate.EnsureIndex()

	if geminiIndex == "" {
		t.Fatal("gemini index should not be empty")
	}
	if compatIndex == "" {
		t.Fatal("compat index should not be empty")
	}
	if altBaseIndex == "" {
		t.Fatal("alt base index should not be empty")
	}
	if duplicateIndex == "" {
		t.Fatal("duplicate index should not be empty")
	}
	if geminiIndex == compatIndex {
		t.Fatalf("shared api key produced duplicate auth_index %q", geminiIndex)
	}
	if geminiIndex == altBaseIndex {
		t.Fatalf("same provider/key with different base_url produced duplicate auth_index %q", geminiIndex)
	}
	if geminiIndex == duplicateIndex {
		t.Fatalf("duplicate config entries should be separated by source-derived seed, got %q", geminiIndex)
	}
}
