package config

import (
	"bytes"
	"encoding/json"
	"fmt"

	"gopkg.in/yaml.v3"
)

// InjectXSearchEnabled preserves the fork default when the setting is omitted.
func (c XAIConfig) InjectXSearchEnabled() bool {
	if c.injectXSearchPresent {
		return c.InjectXSearch
	}
	return true
}

// SetInjectXSearch records an explicit Go setting. A bare false literal is
// indistinguishable from an omitted field, so callers use this to opt out.
func (c *XAIConfig) SetInjectXSearch(enabled bool) {
	c.InjectXSearch = enabled
	c.injectXSearchPresent = true
}

// UnmarshalYAML keeps the public bool's zero value while separately recording
// whether a non-null, non-empty config value was provided.
func (c *XAIConfig) UnmarshalYAML(node *yaml.Node) error {
	var next XAIConfig
	if node == nil || node.ShortTag() == "!!null" {
		*c = next
		return nil
	}
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("xai config must be a mapping")
	}
	seen := false
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == "inject-x-search" {
			if seen {
				return fmt.Errorf("duplicate xai.inject-x-search")
			}
			seen = true
		}
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		key, value := node.Content[i], node.Content[i+1]
		if key.Value != "inject-x-search" {
			continue
		}
		if value.ShortTag() == "!!null" || (value.Kind == yaml.ScalarNode && value.Value == "") {
			*c = next
			return nil
		}
		if value.Kind != yaml.ScalarNode || value.ShortTag() != "!!bool" {
			return fmt.Errorf("xai.inject-x-search must be a boolean, null, or empty")
		}
		if err := value.Decode(&next.InjectXSearch); err != nil {
			return fmt.Errorf("decode xai.inject-x-search: %w", err)
		}
		next.injectXSearchPresent = true
		break
	}
	*c = next
	return nil
}

func (c XAIConfig) MarshalYAML() (any, error) {
	type wire struct {
		InjectXSearch *bool `yaml:"inject-x-search,omitempty"`
	}
	if !c.injectXSearchPresent && !c.InjectXSearch {
		return wire{}, nil
	}
	enabled := c.InjectXSearch
	return wire{InjectXSearch: &enabled}, nil
}

func (c *XAIConfig) UnmarshalJSON(data []byte) error {
	var next XAIConfig
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		*c = next
		return nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &fields); err != nil {
		return err
	}
	raw, ok := fields["inject-x-search"]
	if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || bytes.Equal(bytes.TrimSpace(raw), []byte(`""`)) {
		*c = next
		return nil
	}
	if err := json.Unmarshal(raw, &next.InjectXSearch); err != nil {
		return fmt.Errorf("xai.inject-x-search must be a boolean, null, or empty: %w", err)
	}
	next.injectXSearchPresent = true
	*c = next
	return nil
}

func (c XAIConfig) MarshalJSON() ([]byte, error) {
	type wire struct {
		InjectXSearch *bool `json:"inject-x-search,omitempty"`
	}
	if !c.injectXSearchPresent && !c.InjectXSearch {
		return []byte("{}"), nil
	}
	enabled := c.InjectXSearch
	return json.Marshal(wire{InjectXSearch: &enabled})
}
