package config

import (
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestMergePreservesScalarValueTypesAndPresentation(t *testing.T) {
	for _, tc := range []struct {
		name, old string
		new       any
		style     yaml.Style
	}{
		{"single quoted string to bool", "'yes'", false, 0},
		{"double quoted string to bool", `"off"`, true, 0},
		{"tagged quoted string to bool", `!!str "off"`, true, 0},
		{"quoted string to int", `"012"`, 42, 0},
		{"quoted string to float", "'1.5'", 2.5, 0},
		{"bool to ambiguous string", "true", "false", yaml.DoubleQuotedStyle},
		{"int to ambiguous string", "27", "42", yaml.DoubleQuotedStyle},
		{"same string preserves single quote", "'old'", "true", yaml.SingleQuotedStyle},
		{"same string preserves double quote", `"old"`, "42", yaml.DoubleQuotedStyle},
		{"same tagged int preserves quotes", `!!int "27"`, 42, yaml.TaggedStyle | yaml.DoubleQuotedStyle},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var doc yaml.Node
			before := "# heading\nvalue: &fixture " + tc.old + " # value note\nreference: *fixture\n"
			if err := yaml.Unmarshal([]byte(before), &doc); err != nil {
				t.Fatal(err)
			}
			value := doc.Content[0].Content[1]
			var generated yaml.Node
			if err := generated.Encode(tc.new); err != nil {
				t.Fatal(err)
			}
			mergeNodePreserve(value, &generated)
			if doc.Content[0].Content[3].Alias != value || value.Anchor != "fixture" {
				t.Fatal("lost destination anchor relationship")
			}
			encoded, err := yaml.Marshal(&doc)
			if err != nil {
				t.Fatal(err)
			}
			var decoded map[string]any
			if err := yaml.Unmarshal(encoded, &decoded); err != nil {
				t.Fatalf("encoded scalar does not reparse: %v", err)
			}
			if !reflect.DeepEqual(decoded["value"], tc.new) || !reflect.DeepEqual(decoded["reference"], tc.new) {
				t.Errorf("value=%T(%v), alias=%T(%v), want %T(%v)", decoded["value"], decoded["value"], decoded["reference"], decoded["reference"], tc.new, tc.new)
			}
			var reparsed yaml.Node
			if err := yaml.Unmarshal(encoded, &reparsed); err != nil {
				t.Fatal(err)
			}
			if got := reparsed.Content[0].Content[1].Style; got != tc.style {
				t.Errorf("style=%v, want %v: %s", got, tc.style, encoded)
			}
			if !strings.Contains(string(encoded), "# heading") || !strings.Contains(string(encoded), "# value note") {
				t.Fatalf("comments lost: %s", encoded)
			}
		})
	}
}
