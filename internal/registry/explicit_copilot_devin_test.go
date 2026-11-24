package registry

import (
	"reflect"
	"testing"
)

func TestExplicitCopilotDevinQualifiedCatalogProvenance(t *testing.T) {
	for _, tc := range []struct{ provider, nativeID, qualified string }{
		{"copilot", "gpt-6-sol", "Copilot/GPT-6-SOL"},
		{"devin", "swe-2-lightning", "Devin/SWE-2-LIGHTNING"},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			var want *ModelInfo
			if tc.provider == "devin" {
				want = LookupDevinModel(tc.nativeID)
			} else {
				want = LookupStaticModelInfoByChannel(tc.nativeID, tc.provider)
			}
			if want == nil {
				t.Fatal("fixture not found")
			}
			want.ID = tc.provider + "/" + tc.nativeID
			for _, got := range []*ModelInfo{LookupStaticModelInfo(tc.qualified), LookupStaticModelInfoByChannel(tc.qualified, tc.provider)} {
				if !reflect.DeepEqual(got, want) {
					t.Errorf("qualified route = %+v, want %+v", got, want)
				}
			}
		})
	}
	if got := LookupStaticModelInfoByChannel("copilot/gpt-6-sol", "devin"); got != nil {
		t.Fatal("crossed provider catalogs")
	}
	if got := LookupStaticModelInfo("copilot/NOT-A-MODEL"); got != nil {
		t.Fatal("unexpected model")
	}
	for _, m := range getModels().CodexPro {
		if m.ID == "gpt-6-sol" {
			if got := LookupStaticModelInfo(m.ID); !reflect.DeepEqual(got, m) {
				t.Fatal("bare ID lookup precedence changed")
			}
			return
		}
	}
	t.Fatal("missing bare Codex fixture")
}
