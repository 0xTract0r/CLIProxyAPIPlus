package registry

import (
	"encoding/json"
	"testing"
)

func TestCodexLegacy208SupplementAndRefresh(t *testing.T) {
	for _, nonempty := range []bool{false, true} {
		data := &staticModelsJSON{}
		tiers := []*[]*ModelInfo{&data.CodexFree, &data.CodexTeam, &data.CodexPlus, &data.CodexPro}
		if nonempty {
			for _, tier := range tiers {
				*tier = []*ModelInfo{{ID: "gpt-future"}}
			}
		}
		for repeat := 0; repeat < 2; repeat++ {
			applyCodexCatalogCompatibility(data)
		}
		for i, tier := range tiers {
			for _, id := range []string{"gpt-5.4", "gpt-5.4-mini"} {
				n := 0
				for _, m := range *tier {
					if m.ID == id {
						n++
						if m.Version != id {
							t.Errorf("%s silently remapped to %s", id, m.Version)
						}
					}
				}
				if n != 1 {
					t.Errorf("tier%d missing/duplicate %s: %d", i, id, n)
				}
			}
			n := 0
			for _, m := range *tier {
				if m.ID == "gpt-5.3-codex-spark" {
					n++
					if m.Version != "gpt-5.3" || m.ContextLength != 128000 {
						t.Error("Spark F metadata changed")
					}
				}
			}
			want := 0
			if i >= 2 {
				want = 1
			}
			if n != want {
				t.Errorf("tier%d Spark=%d want%d", i, n, want)
			}
		}
	}
	current := &ModelInfo{ID: "gpt-5.4", Version: "new-catalog-version", ContextLength: 42, DisplayName: "new definition wins"}
	d := &staticModelsJSON{CodexPro: []*ModelInfo{current}}
	applyCodexCatalogCompatibility(d)
	if d.CodexPro[0] != current || current.Version != "new-catalog-version" || current.ContextLength != 42 {
		t.Fatal("new same-ID definition overwritten")
	}
	original := getModels()
	defer func() {
		modelsCatalogStore.mu.Lock()
		modelsCatalogStore.data = original
		modelsCatalogStore.mu.Unlock()
	}()
	for i := 0; i < 2; i++ {
		raw := []byte(`{"codex-free":[{"id":"gpt-future"}],"codex-team":[{"id":"gpt-future"}],"codex-plus":[{"id":"gpt-future"}],"codex-pro":[{"id":"gpt-future"}]}`)
		if _, err := publishCatalogBytes(raw); err != nil {
			t.Fatal(err)
		}
		for _, plan := range []string{"free", "plus", "pro"} {
			ms := GetCodexModelsForPlan(plan)
			for _, id := range []string{"gpt-5.4", "gpt-5.4-mini"} {
				if findModelInfo(ms, id) == nil {
					t.Errorf("refresh %s lost %s", plan, id)
				}
			}
			spark := findModelInfo(ms, "gpt-5.3-codex-spark") != nil
			if spark != (plan == "pro") {
				t.Errorf("Spark plan gate failed: %s", plan)
			}
		}
	}
}
func TestCodexLegacy208FrozenFMetadata(t *testing.T) {
	var golden staticModelsJSON
	if err := json.Unmarshal([]byte(legacy208Golden), &golden); err != nil {
		t.Fatal(err)
	}
	actual := &staticModelsJSON{}
	applyCodexCatalogCompatibility(actual)
	tiers := [][][]*ModelInfo{{actual.CodexFree, golden.CodexFree}, {actual.CodexTeam, golden.CodexTeam}, {actual.CodexPlus, golden.CodexPlus}, {actual.CodexPro, golden.CodexPro}}
	for _, pair := range tiers {
		for _, want := range pair[1] {
			got := findModelInfo(pair[0], want.ID)
			if got == nil {
				t.Errorf("missing F model %s", want.ID)
				continue
			}
			if got.ID != want.ID || got.Version != want.Version || got.Created != want.Created || got.OwnedBy != want.OwnedBy || got.Type != want.Type || got.Object != want.Object || got.DisplayName != want.DisplayName || got.Description != want.Description || got.ContextLength != want.ContextLength || got.MaxCompletionTokens != want.MaxCompletionTokens {
				t.Errorf("F metadata differs %s", want.ID)
			}
		}
	}
}

const legacy208Golden = `{
  "codex-free": [
    {
      "id": "gpt-5.4",
      "object": "model",
      "created": 1772668800,
      "owned_by": "openai",
      "type": "openai",
      "display_name": "GPT 5.4",
      "version": "gpt-5.4",
      "description": "Stable version of GPT 5.4",
      "context_length": 1050000,
      "max_completion_tokens": 128000,
      "supported_parameters": [
        "tools"
      ],
      "thinking": {
        "levels": [
          "low",
          "medium",
          "high",
          "xhigh"
        ]
      }
    },
    {
      "id": "gpt-5.4-mini",
      "object": "model",
      "created": 1773705600,
      "owned_by": "openai",
      "type": "openai",
      "display_name": "GPT 5.4 Mini",
      "version": "gpt-5.4-mini",
      "description": "GPT-5.4 mini brings the strengths of GPT-5.4 to a faster, more efficient model designed for high-volume workloads.",
      "context_length": 400000,
      "max_completion_tokens": 128000,
      "supported_parameters": [
        "tools"
      ],
      "thinking": {
        "levels": [
          "low",
          "medium",
          "high",
          "xhigh"
        ]
      }
    }
  ],
  "codex-team": [
    {
      "id": "gpt-5.4",
      "object": "model",
      "created": 1772668800,
      "owned_by": "openai",
      "type": "openai",
      "display_name": "GPT 5.4",
      "version": "gpt-5.4",
      "description": "Stable version of GPT 5.4",
      "context_length": 1050000,
      "max_completion_tokens": 128000,
      "supported_parameters": [
        "tools"
      ],
      "thinking": {
        "levels": [
          "low",
          "medium",
          "high",
          "xhigh"
        ]
      }
    },
    {
      "id": "gpt-5.4-mini",
      "object": "model",
      "created": 1773705600,
      "owned_by": "openai",
      "type": "openai",
      "display_name": "GPT 5.4 Mini",
      "version": "gpt-5.4-mini",
      "description": "GPT-5.4 mini brings the strengths of GPT-5.4 to a faster, more efficient model designed for high-volume workloads.",
      "context_length": 400000,
      "max_completion_tokens": 128000,
      "supported_parameters": [
        "tools"
      ],
      "thinking": {
        "levels": [
          "low",
          "medium",
          "high",
          "xhigh"
        ]
      }
    }
  ],
  "codex-plus": [
    {
      "id": "gpt-5.3-codex-spark",
      "object": "model",
      "created": 1770912000,
      "owned_by": "openai",
      "type": "openai",
      "display_name": "GPT 5.3 Codex Spark",
      "version": "gpt-5.3",
      "description": "Ultra-fast coding model.",
      "context_length": 128000,
      "max_completion_tokens": 128000,
      "supported_parameters": [
        "tools"
      ],
      "thinking": {
        "levels": [
          "low",
          "medium",
          "high",
          "xhigh"
        ]
      }
    },
    {
      "id": "gpt-5.4",
      "object": "model",
      "created": 1772668800,
      "owned_by": "openai",
      "type": "openai",
      "display_name": "GPT 5.4",
      "version": "gpt-5.4",
      "description": "Stable version of GPT 5.4",
      "context_length": 1050000,
      "max_completion_tokens": 128000,
      "supported_parameters": [
        "tools"
      ],
      "thinking": {
        "levels": [
          "low",
          "medium",
          "high",
          "xhigh"
        ]
      }
    },
    {
      "id": "gpt-5.4-mini",
      "object": "model",
      "created": 1773705600,
      "owned_by": "openai",
      "type": "openai",
      "display_name": "GPT 5.4 Mini",
      "version": "gpt-5.4-mini",
      "description": "GPT-5.4 mini brings the strengths of GPT-5.4 to a faster, more efficient model designed for high-volume workloads.",
      "context_length": 400000,
      "max_completion_tokens": 128000,
      "supported_parameters": [
        "tools"
      ],
      "thinking": {
        "levels": [
          "low",
          "medium",
          "high",
          "xhigh"
        ]
      }
    }
  ],
  "codex-pro": [
    {
      "id": "gpt-5.3-codex-spark",
      "object": "model",
      "created": 1770912000,
      "owned_by": "openai",
      "type": "openai",
      "display_name": "GPT 5.3 Codex Spark",
      "version": "gpt-5.3",
      "description": "Ultra-fast coding model.",
      "context_length": 128000,
      "max_completion_tokens": 128000,
      "supported_parameters": [
        "tools"
      ],
      "thinking": {
        "levels": [
          "low",
          "medium",
          "high",
          "xhigh"
        ]
      }
    },
    {
      "id": "gpt-5.4",
      "object": "model",
      "created": 1772668800,
      "owned_by": "openai",
      "type": "openai",
      "display_name": "GPT 5.4",
      "version": "gpt-5.4",
      "description": "Stable version of GPT 5.4",
      "context_length": 1050000,
      "max_completion_tokens": 128000,
      "supported_parameters": [
        "tools"
      ],
      "thinking": {
        "levels": [
          "low",
          "medium",
          "high",
          "xhigh"
        ]
      }
    },
    {
      "id": "gpt-5.4-mini",
      "object": "model",
      "created": 1773705600,
      "owned_by": "openai",
      "type": "openai",
      "display_name": "GPT 5.4 Mini",
      "version": "gpt-5.4-mini",
      "description": "GPT-5.4 mini brings the strengths of GPT-5.4 to a faster, more efficient model designed for high-volume workloads.",
      "context_length": 400000,
      "max_completion_tokens": 128000,
      "supported_parameters": [
        "tools"
      ],
      "thinking": {
        "levels": [
          "low",
          "medium",
          "high",
          "xhigh"
        ]
      }
    }
  ]
}
`
