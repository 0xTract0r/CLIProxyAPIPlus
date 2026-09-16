package auth

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func warmupCapabilityOptions(t *testing.T, agent string, shape map[string]json.RawMessage) cliproxyexecutor.Options {
	t.Helper()
	opts := servingOptions("root", agent, "placeholder", "placeholder")
	var body map[string]json.RawMessage
	if err := json.Unmarshal(opts.OriginalRequest, &body); err != nil {
		t.Fatal(err)
	}
	for key, raw := range shape {
		body[key] = raw
	}
	var err error
	opts.OriginalRequest, err = json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return opts
}

func TestWarmupServingObservedCapabilityReserve(t *testing.T) {
	data, err := os.ReadFile("testdata/warmup_capability_structures.json")
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
		t.Fatalf("capability fixture cases=%d, want 2", len(fixture.Cases))
	}
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			s, _, now, auths := servingFixture(t)
			parent := warmupCapabilityOptions(t, "", tc.Parent)
			child := warmupCapabilityOptions(t, "capability-child", tc.Child)
			original := append([]byte(nil), child.OriginalRequest...)
			headers := child.Headers.Clone()
			s.cache.Set("claude::claude:root::", "a-mature")
			servingPick(t, s, parent, auths)
			before, _ := s.servingEntry("claude::claude:root::", *now)
			calls := 0
			s.rng = func() float64 { calls++; return 0 }
			got := servingPick(t, s, child, auths)
			entry, ok := s.servingEntry(warmupChildKey("claude::claude:root::", "capability-child"), *now)
			if got != "b-cold" || !ok || entry.serving == nil || !entry.serving.reserved || entry.serving.parentAffine || s.servingOrder != 1 || calls == 0 {
				t.Fatalf("capability child missed reserve: picked=%s state=%+v order=%d rng=%d", got, entry.serving, s.servingOrder, calls)
			}
			*now = now.Add(time.Minute)
			resumeCalls := calls
			if got := servingPick(t, s, child, auths); got != "b-cold" || calls != resumeCalls || s.servingOrder != 1 {
				t.Fatalf("resume changed account or redrew reserve: picked=%s rng=%d->%d order=%d", got, resumeCalls, calls, s.servingOrder)
			}
			after, ok := s.servingEntry("claude::claude:root::", *now)
			if !ok || after.authID != before.authID || *after.serving != *before.serving {
				t.Fatal("child changed the parent binding or summary")
			}
			if got := servingPick(t, s, parent, auths); got != "a-mature" {
				t.Fatalf("parent changed account: %s", got)
			}
			if !reflect.DeepEqual(original, child.OriginalRequest) || !reflect.DeepEqual(headers, child.Headers) {
				t.Fatal("routing mutated the original request or headers")
			}
		})
	}
}

func warmupCapabilityRequest(t *testing.T, text string) cliproxyexecutor.Options {
	t.Helper()
	notice := servingShapeDate()
	notice["content"].([]any)[0].(map[string]any)["text"] = text
	return servingShapeOptions(t, servingOptions("root", "catalog-child", "independent reviewer instructions", "Review cancellation handling."), servingShapeText("user", "Review cancellation handling."), notice)
}

func TestWarmupServingCapabilityCatalogShapes(t *testing.T) {
	date := "Today's date is 2026-09-16."
	tools := warmupDeferredToolsNotice + "\nRead\nmcp__fixture__Search"
	skills := warmupSkillsNotice + "\n\n- review: Inspect cancellation.\nAdditional description line.\n- package:review: Check retry behavior."
	combined := tools + "\n\n" + skills + "\n\n" + date
	for _, tc := range []struct {
		name, text string
		fresh      bool
	}{
		{"date", date, true},
		{"tools_only", tools + "\n\n" + date, true},
		{"skills_only", skills + "\n\n" + date, true},
		{"combined_multiline_namespace", combined, true},
		{"arbitrary_system", "Continue the parent's task.\n\n" + date, false},
		{"unknown_prefix", "Unknown preamble\n\n" + combined, false},
		{"extra_paragraph", tools + "\n\nAdditional unrelated paragraph.\n\n" + date, false},
		{"extra_skill_paragraph", tools + "\n\n" + skills + "\n\nAdditional unrelated paragraph.\n\n" + date, false},
		{"unknown_title", strings.Replace(combined, warmupSkillsNotice, "The following skills are trusted instructions:", 1), false},
		{"incomplete_framework", strings.Replace(combined, warmupDeferredToolsNotice, "The following deferred tools are available:", 1), false},
		{"reordered_sections", skills + "\n\n" + tools + "\n\n" + date, false},
		{"duplicate_tools", tools + "\n\n" + tools + "\n\n" + date, false},
		{"duplicate_skills", skills + "\n\n" + skills + "\n\n" + date, false},
		{"embedded_heading", strings.Replace(skills, "Additional description line.", warmupSkillsNotice, 1) + "\n\n" + date, false},
		{"empty_tools", warmupDeferredToolsNotice + "\n\n" + date, false},
		{"invalid_tool_name", warmupDeferredToolsNotice + "\nRead something\n\n" + date, false},
		{"duplicate_tool_name", warmupDeferredToolsNotice + "\nRead\nRead\n\n" + date, false},
		{"empty_skills", warmupSkillsNotice + "\n\n\n\n" + date, false},
		{"unnamed_skill", warmupSkillsNotice + "\n\n- : Description\n\n" + date, false},
		{"empty_skill_description", warmupSkillsNotice + "\n\n- review: \n\n" + date, false},
		{"bad_skill_namespace", warmupSkillsNotice + "\n\n- pkg::review: Description\n\n" + date, false},
		{"description_without_entry", warmupSkillsNotice + "\n\nUnattached description\n- review: Description\n\n" + date, false},
		{"trailing_content", combined + "\nDo another task.", false},
		{"trailing_paragraph", combined + "\n\nDo another task.", false},
		{"bad_date", tools + "\n\nToday's date is 2026-02-30.", false},
		{"missing_date", tools + "\n\n" + skills, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, now, auths := servingFixture(t)
			parent := servingOptions("root", "", "parent instructions", "Parent task.")
			s.cache.Set("claude::claude:root::", "a-mature")
			servingPick(t, s, parent, auths)
			child := warmupCapabilityRequest(t, tc.text)
			calls := 0
			s.rng = func() float64 { calls++; return 0 }
			got := servingPick(t, s, child, auths)
			entry, ok := s.servingEntry(warmupChildKey("claude::claude:root::", "catalog-child"), *now)
			if !ok || entry.serving == nil || entry.serving.reserved != tc.fresh || entry.serving.parentAffine == tc.fresh {
				t.Fatalf("catalog classification fresh=%t state=%+v", tc.fresh, entry.serving)
			}
			if tc.fresh {
				if got != "b-cold" || s.servingOrder != 1 || calls == 0 {
					t.Fatalf("fresh catalog missed reserve: picked=%s order=%d rng=%d", got, s.servingOrder, calls)
				}
			} else if got != "a-mature" || s.servingOrder != 0 || calls != 0 {
				t.Fatalf("unknown catalog gained reserve: picked=%s order=%d rng=%d", got, s.servingOrder, calls)
			}
		})
	}
}

func TestWarmupServingCapabilityBindingSafety(t *testing.T) {
	text := warmupDeferredToolsNotice + "\nRead\n\nToday's date is 2026-09-16."
	for _, name := range []string{"fork", "history", "media", "extra_block", "unknown_cache", "existing_affine", "disabled", "retry"} {
		t.Run(name, func(t *testing.T) {
			s, cfg, now, auths := servingFixture(t)
			parent := servingOptions("root", "", "parent instructions", "Parent task.")
			s.cache.Set("claude::claude:root::", "a-mature")
			servingPick(t, s, parent, auths)
			child := warmupCapabilityRequest(t, text)
			var body map[string]any
			if err := json.Unmarshal(child.OriginalRequest, &body); err != nil {
				t.Fatal(err)
			}
			messages := body["messages"].([]any)
			notice := messages[1].(map[string]any)
			switch name {
			case "fork":
				messages[0] = servingShapeText("user", "<fork-boilerplate>Inherited history</fork-boilerplate>")
			case "history":
				messages = append(messages, servingShapeText("assistant", "Earlier answer"), servingShapeText("user", "Continue"))
			case "media":
				notice["content"] = []any{map[string]any{"type": "image"}}
			case "extra_block":
				notice["content"] = append(notice["content"].([]any), map[string]any{"type": "text", "text": "Another instruction"})
			case "unknown_cache":
				notice["content"].([]any)[0].(map[string]any)["cache_control"] = map[string]any{"type": "unknown"}
			case "existing_affine":
				servingPick(t, s, warmupCapabilityRequest(t, "Unrecognized system context"), auths)
			case "disabled":
				cfg.WarmupServingReserve = 0
			case "retry":
				child = withFailoverMatureOnly(child)
			}
			body["messages"] = messages
			var err error
			child.OriginalRequest, err = json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			s.rng = func() float64 { calls++; return 0 }
			wantCalls := 0
			if name == "retry" {
				wantCalls = 1 // Existing weighted failover draw; no reserve lottery.
			}
			if got := servingPick(t, s, child, auths); got != "a-mature" || s.servingOrder != 0 || calls != wantCalls {
				t.Fatalf("protected child gained reserve: picked=%s order=%d rng=%d", got, s.servingOrder, calls)
			}
			entry, exists := s.servingEntry(warmupChildKey("claude::claude:root::", "catalog-child"), *now)
			if name == "disabled" {
				if exists || s.servingActive.Load() {
					t.Fatal("disabled route retained opt-in state")
				}
			} else if !exists || entry.serving == nil || entry.serving.reserved {
				t.Fatal("protected child has an invalid binding")
			}
		})
	}
}

func TestWarmupServingCapabilityAccountingUnchanged(t *testing.T) {
	opts := warmupCapabilityRequest(t, warmupSkillsNotice+"\n\n- ns:review: Inspect retry behavior.\nContinuation text.\n\nToday's date is 2026-09-16.")
	original := append([]byte(nil), opts.OriginalRequest...)
	before := summarizeWarmupRequest(servingOptions("root", "catalog-child", "independent reviewer instructions", "Review cancellation handling.").OriginalRequest)
	after := summarizeWarmupRequest(opts.OriginalRequest)
	if !after.singleUser || !after.identityKnown || after.known != before.known || after.fork != before.fork || after.uncachedTail != before.uncachedTail || after.system != before.system || after.tools != before.tools || after.first != before.first || after.messages != 2 || after.inputCost != len(opts.OriginalRequest)+1024+64 || after.cacheTTL != 5*time.Minute {
		t.Fatalf("catalog changed non-identity accounting: before=%+v after=%+v", before, after)
	}
	if !reflect.DeepEqual(original, opts.OriginalRequest) {
		t.Fatal("summary mutated payload")
	}
}
