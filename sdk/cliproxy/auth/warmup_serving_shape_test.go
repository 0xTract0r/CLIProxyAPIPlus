package auth

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// Sanitized observed CLI structures; no production prompts or account data.
func servingShapeOptions(t *testing.T, opts cliproxyexecutor.Options, messages ...any) cliproxyexecutor.Options {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(opts.OriginalRequest, &body); err != nil {
		t.Fatal(err)
	}
	body["messages"] = messages
	var err error
	opts.OriginalRequest, err = json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return opts
}

func servingShapeText(role, text string) map[string]any {
	return map[string]any{"role": role, "content": []any{map[string]any{"type": "text", "text": text}}}
}

func servingShapeDate() map[string]any {
	return map[string]any{"role": "system", "content": []any{map[string]any{
		"type": "text", "text": "Today's date is 2026-09-15.",
		"cache_control": map[string]any{"type": "ephemeral"},
	}}}
}

func servingShapeToolHistory() []any {
	return []any{
		servingShapeText("user", "Inspect the repository and delegate a bounded review."),
		map[string]any{"role": "assistant", "content": []any{map[string]any{
			"type": "tool_use", "id": "toolu_sanitized", "name": "ToolSearch",
			"input":  map[string]any{"query": "Read", "type": "business-operation"},
			"caller": map[string]any{"type": "direct"},
		}}},
		map[string]any{"role": "user", "content": []any{map[string]any{
			"type": "tool_result", "tool_use_id": "toolu_sanitized",
			"content": []any{map[string]any{"type": "tool_reference", "tool_name": "Read"}},
		}}},
	}
}

func TestWarmupServingObservedChildShapesReserve(t *testing.T) {
	for _, tc := range []struct {
		name              string
		parentTools, date bool
	}{
		{"date_auxiliary", false, true},
		{"parent_direct_and_tool_reference", true, false},
		{"combined_real_shape", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, now, auths := servingFixture(t)
			parent := servingOptions("root", "", "parent instructions", "parent history")
			if tc.parentTools {
				parent = servingShapeOptions(t, parent, servingShapeToolHistory()...)
			}
			s.cache.Set("claude::claude:root::", "a-mature")
			servingPick(t, s, parent, auths)
			child := servingOptions("root", "shape-child", "independent reviewer instructions", "Review the retry policy.")
			if tc.date {
				child = servingShapeOptions(t, child, servingShapeText("user", "Review the retry policy."), servingShapeDate())
			}
			calls := 0
			s.rng = func() float64 { calls++; return 0 }
			before := s.servingOrder
			got := servingPick(t, s, child, auths)
			entry, ok := s.servingEntry(warmupChildKey("claude::claude:root::", "shape-child"), *now)
			if got != "b-cold" || !ok || entry.serving == nil || !entry.serving.reserved || entry.serving.parentAffine || s.servingOrder != before+1 || calls == 0 {
				t.Fatalf("fresh reserve path not reached: picked=%s bound=%t state=%+v reserveOrder=%d->%d rngCalls=%d", got, ok, entry.serving, before, s.servingOrder, calls)
			}
			*now = now.Add(time.Minute)
			resume := servingOptions("root", "shape-child", "independent reviewer instructions", "Review the retry policy.", "Review complete.", "Check cancellation too.")
			if got := servingPick(t, s, resume, auths); got != "b-cold" {
				t.Fatalf("resume changed child binding: %s", got)
			}
			if s.servingOrder != before+1 {
				t.Fatal("resume consumed another reserve opportunity")
			}
			if got := servingPick(t, s, parent, auths); got != "a-mature" {
				t.Fatalf("child overwrote parent: %s", got)
			}
		})
	}
}

func TestWarmupServingParentSystemHistoryReserve(t *testing.T) {
	for _, name := range []string{"middle_string", "multiple_strings", "trailing_text_blocks"} {
		t.Run(name, func(t *testing.T) {
			s, _, now, auths := servingFixture(t)
			history := servingShapeToolHistory()
			notice := map[string]any{"role": "system", "content": "Sanitized CLI task reminder."}
			switch name {
			case "middle_string":
				history = append([]any{history[0], notice}, history[1:]...)
			case "multiple_strings":
				history = []any{history[0], notice, history[1], notice, history[2], notice}
			case "trailing_text_blocks":
				history = append(history, servingShapeText("system", "Sanitized summary notification."))
			}
			parent := servingShapeOptions(t, servingOptions("root", "", "parent instructions", "history"), history...)
			s.cache.Set("claude::claude:root::", "a-mature")
			servingPick(t, s, parent, auths)
			child := servingShapeOptions(t, servingOptions("root", "system-history-child", "independent reviewer instructions", "task"), servingShapeText("user", "Review retry policy."), servingShapeDate())
			calls := 0
			s.rng = func() float64 { calls++; return 0 }
			got := servingPick(t, s, child, auths)
			entry, ok := s.servingEntry(warmupChildKey("claude::claude:root::", "system-history-child"), *now)
			if got != "b-cold" || !ok || entry.serving == nil || !entry.serving.reserved || entry.serving.parentAffine || s.servingOrder != 1 || calls == 0 {
				t.Fatalf("parent system history prevented reserve: picked=%s state=%+v order=%d rng=%d", got, entry.serving, s.servingOrder, calls)
			}
			summary := summarizeWarmupRequest(parent.OriginalRequest)
			if !summary.identityKnown || summary.messages != len(history) || summary.inputCost != len(parent.OriginalRequest)+1024+32*len(history) {
				t.Fatalf("system history lost identity/accounting: %+v", summary)
			}
		})
	}
}

func TestWarmupServingChildShapeConservativeBoundaries(t *testing.T) {
	cases := []string{"arbitrary-system", "date-extra-text", "invalid-date", "string-date", "multiple-systems", "system-before-user", "history", "fork", "media-child", "unknown-child", "result-only-child", "media-parent", "unknown-parent", "media-system-parent", "unknown-system-parent", "extra-field-system-parent", "unknown-caller", "caller-extra-field", "malformed-reference", "reference-extra-field", "reference-outside-result", "identity-conflict", "missing-parent"}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			s, _, now, auths := servingFixture(t)
			history := servingShapeToolHistory()
			parent := servingOptions("root", "", "parent instructions", "history")
			child := servingOptions("root", "boundary-child", "independent reviewer instructions", "Review the retry policy.")
			messages := []any{servingShapeText("user", "Review the retry policy."), servingShapeDate()}
			switch name {
			case "arbitrary-system":
				messages[1] = servingShapeText("system", "Inherited instructions")
			case "date-extra-text":
				messages[1] = servingShapeText("system", "Today's date is 2026-09-15. Continue the parent's work.")
			case "invalid-date":
				messages[1] = servingShapeText("system", "Today's date is 2026-02-30.")
			case "string-date":
				messages[1] = map[string]any{"role": "system", "content": "Today's date is 2026-09-15."}
			case "multiple-systems":
				messages = append(messages, servingShapeDate())
			case "system-before-user":
				messages[0], messages[1] = messages[1], messages[0]
			case "history":
				messages = append(messages[:1], servingShapeText("assistant", "Prior answer"), servingShapeText("user", "Continue"))
			case "fork":
				messages[0] = servingShapeText("user", "<fork-boilerplate>Inherited fork</fork-boilerplate>")
			case "media-child", "unknown-child":
				kind := "image"
				if name == "unknown-child" {
					kind = "future-block"
				}
				messages[0] = map[string]any{"role": "user", "content": []any{map[string]any{"type": kind}}}
			case "result-only-child":
				messages[0] = history[2]
			case "media-parent", "unknown-parent":
				kind := "image"
				if name == "unknown-parent" {
					kind = "future-block"
				}
				history = append(history, map[string]any{"role": "user", "content": []any{map[string]any{"type": kind}}})
			case "media-system-parent", "unknown-system-parent":
				kind := "image"
				if name == "unknown-system-parent" {
					kind = "future-block"
				}
				history = append(history, map[string]any{"role": "system", "content": []any{map[string]any{"type": kind}}})
			case "extra-field-system-parent":
				history = append(history, map[string]any{"role": "system", "content": "Task notice", "unknown": "opaque"})
			case "unknown-caller":
				history[1].(map[string]any)["content"].([]any)[0].(map[string]any)["caller"] = map[string]any{"type": "code_execution_20260101"}
			case "caller-extra-field":
				history[1].(map[string]any)["content"].([]any)[0].(map[string]any)["caller"] = map[string]any{"type": "direct", "unknown": "opaque"}
			case "malformed-reference":
				history[2].(map[string]any)["content"].([]any)[0].(map[string]any)["content"] = []any{map[string]any{"type": "tool_reference", "tool_name": 42}}
			case "reference-extra-field":
				history[2].(map[string]any)["content"].([]any)[0].(map[string]any)["content"] = []any{map[string]any{"type": "tool_reference", "tool_name": "Read", "unknown": "opaque"}}
			case "reference-outside-result":
				history[2].(map[string]any)["content"] = []any{map[string]any{"type": "tool_reference", "tool_name": "Read"}}
			case "identity-conflict":
				child.Headers.Set("X-Claude-Code-Session-Id", "conflicting-root")
			}
			parent = servingShapeOptions(t, parent, history...)
			child = servingShapeOptions(t, child, messages...)
			s.cache.Set("claude::claude:root::", "a-mature")
			if name != "missing-parent" {
				servingPick(t, s, parent, auths)
			}
			calls := 0
			s.rng = func() float64 { calls++; return 0 }
			before := s.servingOrder
			got := servingPick(t, s, child, auths)
			entry, ok := s.servingEntry(warmupChildKey("claude::claude:root::", "boundary-child"), *now)
			if got != "a-mature" || !ok || entry.serving == nil || !entry.serving.parentAffine || entry.serving.reserved || s.servingOrder != before || calls != 0 {
				t.Fatalf("unsafe shape gained reserve: picked=%s bound=%t state=%+v order=%d->%d rng=%d", got, ok, entry.serving, before, s.servingOrder, calls)
			}
			// Becoming recognizable later must not redraw an existing parent-affine binding.
			*now = now.Add(time.Minute)
			fixed := servingShapeOptions(t, servingOptions("root", "boundary-child", "independent reviewer instructions", "task"), servingShapeText("user", "task"), servingShapeDate())
			if got := servingPick(t, s, fixed, auths); got != "a-mature" {
				t.Fatalf("existing child reclassified: %s", got)
			}
			if s.servingOrder != before {
				t.Fatal("existing child gained a new opportunity")
			}
		})
	}
}

func TestWarmupServingChildIdentityDoesNotExpandMigration(t *testing.T) {
	for _, kind := range []string{"tool-shape", "image", "future-block"} {
		t.Run(kind, func(t *testing.T) {
			s, cfg, now, auths := servingFixture(t)
			cfg.WarmupServingMaxBindingAgeSeconds = 60
			cfg.WarmupServingMigrationTokenBudget = 1000000
			messages := servingShapeToolHistory()
			if kind != "tool-shape" {
				messages = append(messages, map[string]any{"role": "user", "content": []any{map[string]any{"type": kind}}})
			}
			parent := servingShapeOptions(t, servingOptions("root", "", "parent instructions", "history"), messages...)
			summary := summarizeWarmupRequest(parent.OriginalRequest)
			if summary.known || summary.identityKnown != (kind == "tool-shape") {
				t.Fatalf("identity/cost gates not independent: %+v", summary)
			}
			s.cache.Set("claude::claude:root::", "a-mature")
			servingPick(t, s, parent, auths)
			*now = now.Add(61 * time.Second)
			if got := servingPick(t, s, parent, auths); got != "a-mature" {
				t.Fatalf("opaque input authorized migration: %s", got)
			}
			// A text reset still cannot migrate when the previous input was not estimable.
			textOnly := servingOptions("root", "", "parent instructions", "short summary")
			if got := servingPick(t, s, textOnly, auths); got != "a-mature" {
				t.Fatalf("unknown previous input authorized reset: %s", got)
			}
			if len(s.servingCharges) != 0 {
				t.Fatal("unestimated input spent migration budget")
			}
		})
	}
}

func TestWarmupServingDateKeepsSummaryAccounting(t *testing.T) {
	base := servingOptions("root", "child", "independent reviewer instructions", "Review retry policy")
	// Only the date carries cache_control; it must still establish the 5m TTL.
	var body map[string]any
	if err := json.Unmarshal(base.OriginalRequest, &body); err != nil {
		t.Fatal(err)
	}
	body["system"] = "independent reviewer instructions"
	base.OriginalRequest, _ = json.Marshal(body)
	before := summarizeWarmupRequest(base.OriginalRequest)
	withDate := servingShapeOptions(t, base, servingShapeText("user", "Review retry policy"), servingShapeDate())
	after := summarizeWarmupRequest(withDate.OriginalRequest)
	if !after.singleUser || !after.identityKnown || !after.known || after.messages != 2 || after.first != before.first || after.inputCost != len(withDate.OriginalRequest)+1024+64 || after.inputCost <= before.inputCost || after.cacheTTL != 5*time.Minute || before.cacheTTL != time.Hour {
		t.Fatalf("date altered non-identity accounting: before=%+v after=%+v", before, after)
	}
}

func TestWarmupServingObservedShapesDisabledNoOp(t *testing.T) {
	s, cfg, _, auths := servingFixture(t)
	cfg.WarmupServingReserve = 0
	parent := servingShapeOptions(t, servingOptions("root", "", "parent", "history"), servingShapeToolHistory()...)
	s.cache.Set("claude::claude:root::", "a-mature")
	servingPick(t, s, parent, auths)
	child := servingShapeOptions(t, servingOptions("root", "disabled-child", "independent", "task"), servingShapeText("user", "task"), servingShapeDate())
	calls := 0
	s.rng = func() float64 { calls++; return 0 }
	if got := servingPick(t, s, child, auths); got != "a-mature" {
		t.Fatalf("disabled route changed: %s", got)
	}
	if calls != 0 || s.servingActive.Load() || s.servingOrder != 0 || len(s.servingAccounts) != 0 {
		t.Fatal("disabled request used opt-in state/RNG")
	}
	if _, ok := s.cache.Get(warmupChildKey("claude::claude:root::", "disabled-child")); ok {
		t.Fatal("disabled request created child binding")
	}
}

func TestWarmupServingObservedFullHistoriesReserve(t *testing.T) {
	data, err := os.ReadFile("testdata/warmup_observed_child_structures.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Name                         string
			Parent, Child                map[string]json.RawMessage
			ExpectedParentMessages       int `json:"expected_parent_messages"`
			ExpectedParentSystemMessages int `json:"expected_parent_system_messages"`
		}
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.Cases) != 3 {
		t.Fatalf("full-history fixture cases=%d, want 3", len(fixture.Cases))
	}
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			s, _, now, auths := servingFixture(t)
			options := func(agent string, shape map[string]json.RawMessage) cliproxyexecutor.Options {
				opts := servingOptions("root", agent, "placeholder", "placeholder")
				var body map[string]json.RawMessage
				if err := json.Unmarshal(opts.OriginalRequest, &body); err != nil {
					t.Fatal(err)
				}
				for key, raw := range shape {
					body[key] = raw
				}
				opts.OriginalRequest, err = json.Marshal(body)
				if err != nil {
					t.Fatal(err)
				}
				return opts
			}
			parent, child := options("", tc.Parent), options("observed-child", tc.Child)
			var messages []struct{ Role string }
			if err := json.Unmarshal(tc.Parent["messages"], &messages); err != nil {
				t.Fatal(err)
			}
			systems := 0
			for _, message := range messages {
				if message.Role == "system" {
					systems++
				}
			}
			if len(messages) != tc.ExpectedParentMessages || systems != tc.ExpectedParentSystemMessages {
				t.Fatalf("fixture history changed: messages=%d system=%d", len(messages), systems)
			}
			s.cache.Set("claude::claude:root::", "a-mature")
			servingPick(t, s, parent, auths)
			before, ok := s.servingEntry("claude::claude:root::", *now)
			if !ok || before.serving == nil || !before.serving.summary.identityKnown {
				t.Fatal("full parent history could not establish identity")
			}
			calls := 0
			s.rng = func() float64 { calls++; return 0 }
			got := servingPick(t, s, child, auths)
			entry, ok := s.servingEntry(warmupChildKey("claude::claude:root::", "observed-child"), *now)
			if got != "b-cold" || !ok || entry.serving == nil || !entry.serving.reserved || entry.serving.parentAffine || s.servingOrder != 1 || calls == 0 {
				t.Fatalf("full-history fresh reserve not reached: picked=%s state=%+v order=%d rng=%d", got, entry.serving, s.servingOrder, calls)
			}
			after, ok := s.servingEntry("claude::claude:root::", *now)
			if !ok || after.authID != before.authID || after.serving == nil || *after.serving != *before.serving {
				t.Fatal("child changed parent account or retained summary")
			}
			if got := servingPick(t, s, parent, auths); got != "a-mature" {
				t.Fatalf("parent did not keep its account: %s", got)
			}
		})
	}
}
