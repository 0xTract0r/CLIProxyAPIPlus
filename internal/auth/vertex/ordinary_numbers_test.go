package vertex

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestVertexStoragePreservesNumbersAndMetadataPriority(t *testing.T) {
	for _, override := range []bool{false, true} {
		t.Run(map[bool]string{false: "without_metadata", true: "metadata_wins"}[override], func(t *testing.T) {
			account := map[string]any{"nested": map[string]any{"n": json.Number("9007199254740993"), "huge": json.Number("1e10000")}}
			s := &VertexCredentialStorage{ServiceAccount: account, ProjectID: "source", Email: "fixture@example.invalid"}
			if override {
				s.SetMetadata(map[string]any{"project_id": "override", "extra": json.Number("9007199254740995"), "service_account": map[string]any{"override": json.Number("9007199254740997")}})
			}
			p := filepath.Join(t.TempDir(), "fixture.json")
			if err := s.SaveTokenToFile(p); err != nil {
				t.Fatal(err)
			}
			b, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			var got map[string]json.RawMessage
			if err = json.Unmarshal(b, &got); err != nil {
				t.Fatal(err)
			}
			if _, ok := got["location"]; ok {
				t.Fatal("omitempty lost")
			}
			if _, ok := got["prefix"]; ok {
				t.Fatal("omitempty lost")
			}
			var sa map[string]json.RawMessage
			json.Unmarshal(got["service_account"], &sa)
			if override {
				if string(got["project_id"]) != `"override"` || string(got["extra"]) != "9007199254740995" || string(sa["override"]) != "9007199254740997" {
					t.Fatal("metadata priority/precision")
				}
			} else {
				var nested map[string]json.RawMessage
				json.Unmarshal(sa["nested"], &nested)
				if string(nested["n"]) != "9007199254740993" || string(nested["huge"]) != "1e10000" {
					t.Fatal("source precision")
				}
			}
			if account["nested"].(map[string]any)["n"] != json.Number("9007199254740993") {
				t.Fatal("source mutated")
			}
		})
	}
}
