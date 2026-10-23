package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestParseConfigBytesXAIConfig(t *testing.T) {
	defaultCfg, errDefault := ParseConfigBytes([]byte(`{}`))
	if errDefault != nil {
		t.Fatalf("ParseConfigBytes(default) error = %v", errDefault)
	}
	if defaultCfg.XAI.InjectXSearch {
		t.Fatal("xai.inject-x-search = true by default, want false")
	}
	if !defaultCfg.XAI.InjectXSearchEnabled() {
		t.Fatal("omitted xai.inject-x-search effective value = false, want legacy true")
	}

	enabledCfg, errEnabled := ParseConfigBytes([]byte(`xai:
  inject-x-search: true
`))
	if errEnabled != nil {
		t.Fatalf("ParseConfigBytes(enabled) error = %v", errEnabled)
	}
	if !enabledCfg.XAI.InjectXSearch {
		t.Fatal("xai.inject-x-search = false, want true")
	}
}

func TestParseConfigBytesXAIAPIKeyMatchesCodexShape(t *testing.T) {
	cfg, errParse := ParseConfigBytes([]byte(`xai-api-key:
  - api-key: " xai-key "
    priority: 3
    weight: 5
    prefix: " team-xai "
    base-url: " https://api.x.ai/v1 "
    websockets: true
    proxy-url: " http://proxy.local "
    headers:
      X-Custom: value
    models:
      - name: grok-4.5
        alias: grok-latest
        display-name: Grok Latest
        force-mapping: true
    excluded-models:
      - " grok-3-* "
    disable-cooling: true
    request-retry: 0
  - api-key: dropped
    base-url: " "
`))
	if errParse != nil {
		t.Fatalf("ParseConfigBytes() error = %v", errParse)
	}
	if len(cfg.XAIKey) != 1 {
		t.Fatalf("xai-api-key count = %d, want 1", len(cfg.XAIKey))
	}
	entry := cfg.XAIKey[0]
	if entry.APIKey != " xai-key " {
		t.Fatalf("api-key = %q, want original Codex-compatible value", entry.APIKey)
	}
	if entry.Priority != 3 {
		t.Fatalf("priority = %d, want 3", entry.Priority)
	}
	if entry.Weight == nil || *entry.Weight != 5 {
		t.Fatalf("weight = %v, want 5", entry.Weight)
	}
	if entry.Prefix != "team-xai" {
		t.Fatalf("prefix = %q, want team-xai", entry.Prefix)
	}
	if entry.BaseURL != "https://api.x.ai/v1" {
		t.Fatalf("base-url = %q, want https://api.x.ai/v1", entry.BaseURL)
	}
	if !entry.Websockets {
		t.Fatal("websockets = false, want true")
	}
	if entry.ProxyURL != " http://proxy.local " {
		t.Fatalf("proxy-url = %q, want original Codex-compatible value", entry.ProxyURL)
	}
	if entry.DisableCooling == nil || !*entry.DisableCooling {
		t.Fatalf("disable-cooling = %v, want true", entry.DisableCooling)
	}
	if entry.RequestRetry == nil || *entry.RequestRetry != 0 {
		t.Fatalf("request-retry = %v, want 0", entry.RequestRetry)
	}
	if entry.Headers["X-Custom"] != "value" {
		t.Fatalf("X-Custom header = %q, want value", entry.Headers["X-Custom"])
	}
	if len(entry.Models) != 1 {
		t.Fatalf("model count = %d, want 1", len(entry.Models))
	}
	model := entry.Models[0]
	if model.Name != "grok-4.5" || model.Alias != "grok-latest" || model.DisplayName != "Grok Latest" || !model.ForceMapping {
		t.Fatalf("unexpected model mapping: %+v", model)
	}
	if len(entry.ExcludedModels) != 1 || entry.ExcludedModels[0] != "grok-3-*" {
		t.Fatalf("excluded-models = %#v, want [grok-3-*]", entry.ExcludedModels)
	}
}

func TestXAIConfigYAMLPresence(t *testing.T) {
	cases := []struct {
		name          string
		yaml          string
		wantRaw       bool
		wantEffective bool
		wantPresent   bool
	}{
		{name: "omitted", yaml: "xai: {}\n", wantEffective: true},
		{name: "true", yaml: "xai:\n  inject-x-search: true\n", wantRaw: true, wantEffective: true, wantPresent: true},
		{name: "false", yaml: "xai:\n  inject-x-search: false\n", wantEffective: false, wantPresent: true},
		{name: "null", yaml: "xai:\n  inject-x-search: null\n", wantEffective: true},
		{name: "empty", yaml: "xai:\n  inject-x-search:\n", wantEffective: true},
		{name: "quoted empty", yaml: "xai:\n  inject-x-search: \"\"\n", wantEffective: true},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := ParseConfigBytes([]byte(tt.yaml))
			if err != nil {
				t.Fatalf("ParseConfigBytes() error = %v", err)
			}
			got := cfg.XAI
			if got.InjectXSearch != tt.wantRaw || got.InjectXSearchEnabled() != tt.wantEffective || got.injectXSearchPresent != tt.wantPresent {
				t.Fatalf("XAI config = {raw:%v effective:%v present:%v}, want {raw:%v effective:%v present:%v}", got.InjectXSearch, got.InjectXSearchEnabled(), got.injectXSearchPresent, tt.wantRaw, tt.wantEffective, tt.wantPresent)
			}
		})
	}
	var invalid XAIConfig
	if err := yaml.Unmarshal([]byte(`inject-x-search: yes`), &invalid); err == nil {
		t.Fatal("non-boolean YAML value was accepted")
	}
}

func TestXAIConfigJSONPresenceAndSerialization(t *testing.T) {
	cases := []struct {
		name          string
		json          string
		wantRaw       bool
		wantEffective bool
		wantPresent   bool
	}{
		{name: "omitted", json: `{}`, wantEffective: true},
		{name: "true", json: `{"inject-x-search":true}`, wantRaw: true, wantEffective: true, wantPresent: true},
		{name: "false", json: `{"inject-x-search":false}`, wantEffective: false, wantPresent: true},
		{name: "null", json: `{"inject-x-search":null}`, wantEffective: true},
		{name: "empty", json: `{"inject-x-search":""}`, wantEffective: true},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			var got XAIConfig
			if err := json.Unmarshal([]byte(tt.json), &got); err != nil {
				t.Fatalf("json.Unmarshal() error = %v", err)
			}
			if got.InjectXSearch != tt.wantRaw || got.InjectXSearchEnabled() != tt.wantEffective || got.injectXSearchPresent != tt.wantPresent {
				t.Fatalf("XAI config = {raw:%v effective:%v present:%v}, want {raw:%v effective:%v present:%v}", got.InjectXSearch, got.InjectXSearchEnabled(), got.injectXSearchPresent, tt.wantRaw, tt.wantEffective, tt.wantPresent)
			}
		})
	}
	var invalid XAIConfig
	if err := json.Unmarshal([]byte(`{"inject-x-search":"false"}`), &invalid); err == nil {
		t.Fatal("non-boolean JSON value was accepted")
	}

	var zero XAIConfig
	encoded, err := json.Marshal(zero)
	if err != nil {
		t.Fatalf("json.Marshal(zero) error = %v", err)
	}
	if string(encoded) != `{}` {
		t.Fatalf("omitted JSON config = %s, want {}", encoded)
	}
	zero.SetInjectXSearch(false)
	encoded, err = json.Marshal(zero)
	if err != nil {
		t.Fatalf("json.Marshal(explicit false) error = %v", err)
	}
	if string(encoded) != `{"inject-x-search":false}` {
		t.Fatalf("explicit false JSON = %s", encoded)
	}
}

func TestXAIConfigYAMLSerializationAndSaveReloadPresence(t *testing.T) {
	var zero XAIConfig
	encoded, err := yaml.Marshal(zero)
	if err != nil {
		t.Fatalf("yaml.Marshal(zero) error = %v", err)
	}
	if strings.Contains(string(encoded), "inject-x-search") {
		t.Fatalf("omitted YAML config materialized inject-x-search: %s", encoded)
	}
	zero.SetInjectXSearch(false)
	encoded, err = yaml.Marshal(zero)
	if err != nil {
		t.Fatalf("yaml.Marshal(explicit false) error = %v", err)
	}
	if !strings.Contains(string(encoded), "inject-x-search: false") {
		t.Fatalf("explicit false YAML missing key: %s", encoded)
	}

	cases := []struct {
		name          string
		initial       string
		wantPresent   bool
		wantRaw       bool
		wantEffective bool
	}{
		{name: "omitted", initial: "{}\n", wantEffective: true},
		{name: "explicit false", initial: "xai:\n  inject-x-search: false\n", wantPresent: true},
		{name: "explicit true", initial: "xai:\n  inject-x-search: true\n", wantPresent: true, wantRaw: true, wantEffective: true},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tt.initial), 0600); err != nil {
				t.Fatalf("write config: %v", err)
			}
			cfg, err := ParseConfigBytes([]byte(tt.initial))
			if err != nil {
				t.Fatalf("ParseConfigBytes() error = %v", err)
			}
			if err := SaveConfigPreserveComments(path, cfg); err != nil {
				t.Fatalf("SaveConfigPreserveComments() error = %v", err)
			}
			saved, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read saved config: %v", err)
			}
			hasKey := strings.Contains(string(saved), "inject-x-search:")
			if hasKey != tt.wantPresent {
				t.Fatalf("saved presence = %v, want %v; config:\n%s", hasKey, tt.wantPresent, saved)
			}
			reloaded, err := ParseConfigBytes(saved)
			if err != nil {
				t.Fatalf("reload saved config: %v", err)
			}
			if got := reloaded.XAI; got.InjectXSearch != tt.wantRaw || got.InjectXSearchEnabled() != tt.wantEffective || got.injectXSearchPresent != tt.wantPresent {
				t.Fatalf("reloaded XAI config = {raw:%v effective:%v present:%v}, want {raw:%v effective:%v present:%v}", got.InjectXSearch, got.InjectXSearchEnabled(), got.injectXSearchPresent, tt.wantRaw, tt.wantEffective, tt.wantPresent)
			}
		})
	}
}

func TestXAIConfigZeroValueAndSetter(t *testing.T) {
	var zero XAIConfig
	if zero.InjectXSearch || !zero.InjectXSearchEnabled() {
		t.Fatalf("Go zero value raw/effective = %v/%v, want false/true", zero.InjectXSearch, zero.InjectXSearchEnabled())
	}
	zero.SetInjectXSearch(false)
	if zero.InjectXSearch || zero.InjectXSearchEnabled() || !zero.injectXSearchPresent {
		t.Fatalf("explicit Go false raw/effective/present = %v/%v/%v, want false/false/true", zero.InjectXSearch, zero.InjectXSearchEnabled(), zero.injectXSearchPresent)
	}
	literalTrue := XAIConfig{InjectXSearch: true}
	if !literalTrue.InjectXSearchEnabled() {
		t.Fatal("Go literal true effective value = false, want true")
	}
}

func TestXAIConfigRejectsNonScalarAndDuplicateSwitch(t *testing.T) {
	for _, raw := range []string{"inject-x-search: {}", "inject-x-search: []", "inject-x-search: {nested: true}", "inject-x-search: true\ninject-x-search: false"} {
		var cfg XAIConfig
		if err := yaml.Unmarshal([]byte(raw), &cfg); err == nil {
			t.Fatalf("accepted malformed switch: %s", raw)
		}
	}
}

func TestXAIConfigV8AliasPresence(t *testing.T) {
	for _, tc := range []struct {
		name, source string
		want         bool
	}{
		{"canonical false", "config-version: 8\nupstream:\n  xai:\n    inject-x-search: false\n", false},
		{"canonical true", "config-version: 8\nupstream:\n  xai:\n    inject-x-search: true\n", true},
		{"old OAuth false", "oauth:\n  providers:\n    xai:\n      inject-x-search: false\n", false},
		{"canonical wins", "config-version: 8\nupstream:\n  xai:\n    inject-x-search: false\noauth:\n  providers:\n    xai:\n      inject-x-search: true\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := ParseConfigBytes([]byte(tc.source))
			if err != nil {
				t.Fatal(err)
			}
			if got := cfg.XAI.InjectXSearchEnabled(); got != tc.want {
				t.Fatalf("effective=%v want=%v", got, tc.want)
			}
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tc.source), 0600); err != nil {
				t.Fatal(err)
			}
			if err := SaveConfigPreserveComments(path, cfg); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			reloaded, err := ParseConfigBytes(data)
			if err != nil {
				t.Fatal(err)
			}
			if got := reloaded.XAI.InjectXSearchEnabled(); got != tc.want {
				t.Fatalf("reloaded=%v want=%v; %s", got, tc.want, data)
			}
		})
	}
}
