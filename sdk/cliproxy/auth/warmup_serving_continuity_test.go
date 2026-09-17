package auth

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestWarmupServingNativeNoticeContinuity(t *testing.T) {
	data, err := os.ReadFile("testdata/warmup_continuity_structures.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Name          string
			Parent, Child map[string]json.RawMessage
		}
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.Cases) != 2 {
		t.Fatalf("native fixture cases=%d", len(fixture.Cases))
	}
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			s, _, now, auths := servingFixture(t)
			parent := warmupCapabilityOptions(t, "", tc.Parent)
			child := warmupCapabilityOptions(t, "native-child", tc.Child)
			s.cache.Set("claude::claude:root::", "a-mature")
			servingPick(t, s, parent, auths)
			calls := 0
			s.rng = func() float64 { calls++; return 0 }
			got := servingPick(t, s, child, auths)
			entry, ok := s.servingEntry(warmupChildKey("claude::claude:root::", "native-child"), *now)
			if got != "b-cold" || !ok || entry.serving == nil || !entry.serving.reserved || entry.serving.parentAffine || s.servingOrder != 1 || calls == 0 {
				t.Fatalf("native child missed reserve: picked=%s order=%d rng=%d", got, s.servingOrder, calls)
			}
			*now = now.Add(time.Minute)
			before := calls
			if got := servingPick(t, s, child, auths); got != "b-cold" || calls != before || s.servingOrder != 1 {
				t.Fatal("native resume redrew reserve")
			}
			if got := servingPick(t, s, parent, auths); got != "a-mature" {
				t.Fatal("native child overwrote parent")
			}
		})
	}
}

func TestWarmupServingCompleteNoticePackaging(t *testing.T) {
	tools := warmupDeferredToolsNotice + "\nRead\nSearch"
	skills := warmupSkillsNotice + "\n\n- pkg:review: Review behavior.\nUnindented opaque description."
	date := "Today's date is 2026-09-17."
	for _, tc := range []struct {
		name   string
		groups [][]string
		valid  bool
	}{
		{"skills_no_date", [][]string{{skills}}, true},
		{"string_date", [][]string{{date}}, true},
		{"tools_no_date", [][]string{{tools}}, true},
		{"blocks_skills_tools_date", [][]string{{skills, tools, date}}, true},
		{"messages_date_skills_tools", [][]string{{date}, {skills}, {tools}}, true},
		{"combined_date_tools_skills", [][]string{{date + "\n\n" + tools + "\n\n" + skills}}, true},
		{"combined_skills_tools", [][]string{{skills + "\n\n" + tools}}, true},
		{"duplicate_catalog", [][]string{{skills}, {skills}}, false},
		{"duplicate_date", [][]string{{date}, {date}}, false},
		{"conflicting_date", [][]string{{date}, {"Today's date is 2026-09-18."}}, false},
		{"split_skill_header", [][]string{{warmupSkillsNotice}, {"- review: Description."}}, false},
		{"split_tool_header", [][]string{{warmupDeferredToolsNotice, "Read"}}, false},
		{"unknown_independent_paragraph", [][]string{{skills + "\n\nUnknown standalone paragraph."}}, false},
		{"unknown_block", [][]string{{tools, "Unknown standalone paragraph."}}, false},
		{"empty_block", [][]string{{tools, ""}}, false},
		{"oversize_aux", [][]string{{warmupSkillsNotice + "\n\n- review: " + strings.Repeat("x", warmupAuxiliaryMaxBytes)}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, now, auths := servingFixture(t)
			parent := servingOptions("root", "", "parent", "Parent task.")
			s.cache.Set("claude::claude:root::", "a-mature")
			servingPick(t, s, parent, auths)
			messages := []any{servingShapeText("user", "Independent delegated task.")}
			for _, group := range tc.groups {
				blocks := make([]any, 0, len(group))
				for _, text := range group {
					blocks = append(blocks, map[string]any{"type": "text", "text": text, "cache_control": map[string]any{"type": "ephemeral"}})
				}
				var content any = blocks
				if tc.name == "string_date" {
					content = group[0]
				}
				messages = append(messages, map[string]any{"role": "system", "content": content})
			}
			child := servingShapeOptions(t, servingOptions("root", "packaging-child", "independent", "task"), messages...)
			original := append([]byte(nil), child.OriginalRequest...)
			calls := 0
			s.rng = func() float64 { calls++; return 0 }
			got := servingPick(t, s, child, auths)
			entry, _ := s.servingEntry(warmupChildKey("claude::claude:root::", "packaging-child"), *now)
			if entry.serving == nil || entry.serving.reserved != tc.valid || entry.serving.parentAffine == tc.valid {
				t.Fatalf("packaging classification got=%s state=%+v", got, entry.serving)
			}
			if tc.valid && (got != "b-cold" || s.servingOrder != 1 || calls == 0) {
				t.Fatal("complete notices missed reserve")
			}
			if !tc.valid && (got != "a-mature" || s.servingOrder != 0 || calls != 0) {
				t.Fatal("unknown notices gained reserve")
			}
			summary := summarizeWarmupRequest(child.OriginalRequest)
			if summary.messages != len(messages) || summary.inputCost != len(original)+1024+32*len(messages) || summary.cacheTTL != 5*time.Minute || !reflect.DeepEqual(original, child.OriginalRequest) {
				t.Fatal("classification changed accounting or payload")
			}
		})
	}
	if !warmupAuxiliaryMessage(map[string]any{"role": "system", "content": skills}) {
		t.Fatal("complete string notice rejected")
	}
}

func TestWarmupServingParentTaskPrefixSafety(t *testing.T) {
	for _, name := range []string{"same_task_rewrapped", "whitespace_block", "task_extension", "flattened_history", "shared_large_context", "shared_catalog", "opaque_parent_task", "fork"} {
		t.Run(name, func(t *testing.T) {
			s, _, now, auths := servingFixture(t)
			parentFirst := servingShapeText("user", "Review the cancellation policy.")
			childFirst := servingShapeText("user", "Review the cancellation policy. Also inspect retries.")
			fresh := false
			switch name {
			case "whitespace_block":
				childFirst["content"] = []any{map[string]any{"type": "text", "text": "Review the"}, map[string]any{"type": "text", "text": " "}, map[string]any{"type": "text", "text": "cancellation policy."}}
			case "same_task_rewrapped":
				childFirst["content"] = "Review  the\ncancellation policy."
			case "flattened_history":
				childFirst["content"] = "Review the cancellation policy. Assistant prior answer. Continue the old task."
			case "shared_large_context":
				context := strings.Repeat("Shared context text. ", 4000)
				parentFirst["content"] = []any{map[string]any{"type": "text", "text": context}, map[string]any{"type": "text", "text": "Review parent policy."}}
				childFirst["content"] = []any{map[string]any{"type": "text", "text": context}, map[string]any{"type": "text", "text": "Inspect independent child behavior."}}
				fresh = true
			case "shared_catalog":
				catalog := warmupSkillsNotice + "\n\n- review: Shared description."
				parentFirst["content"] = []any{map[string]any{"type": "text", "text": catalog}, map[string]any{"type": "text", "text": "Parent task."}}
				childFirst["content"] = []any{map[string]any{"type": "text", "text": catalog}, map[string]any{"type": "text", "text": "Independent task."}}
				fresh = true
			case "opaque_parent_task":
				parentFirst["content"] = strings.Repeat("x", warmupTaskMaxBytes+1)
			case "fork":
				childFirst["content"] = "<fork-boilerplate>Inherited context</fork-boilerplate> independent-looking task"
			}
			parent := servingShapeOptions(t, servingOptions("root", "", "parent instructions", "task"), parentFirst)
			child := servingShapeOptions(t, servingOptions("root", "prefix-child", "changed child instructions", "task"), childFirst)
			s.cache.Set("claude::claude:root::", "a-mature")
			servingPick(t, s, parent, auths)
			calls := 0
			s.rng = func() float64 { calls++; return 0 }
			got := servingPick(t, s, child, auths)
			entry, _ := s.servingEntry(warmupChildKey("claude::claude:root::", "prefix-child"), *now)
			if entry.serving == nil || entry.serving.reserved != fresh || entry.serving.parentAffine == fresh {
				t.Fatalf("prefix classification got=%s state=%+v", got, entry.serving)
			}
			if fresh && (got != "b-cold" || calls == 0) {
				t.Fatal("shared context blocked independent task")
			}
			if !fresh && (got != "a-mature" || calls != 0) {
				t.Fatal("inherited task gained reserve")
			}
			if name == "same_task_rewrapped" {
				p, c := summarizeWarmupRequest(parent.OriginalRequest), summarizeWarmupRequest(child.OriginalRequest)
				if p.first == c.first || p.task != c.task || p.taskBytes != c.taskBytes {
					t.Fatal("canonical task overwrote raw hash or missed wrapping")
				}
			}
		})
	}
}
