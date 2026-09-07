package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestClaudeContextEntitlementSelection(t *testing.T) {
	const opus = "claude-opus-4-8"
	cases := []struct {
		name, model, target, beta, want string
		max, pinned, retry, perAuth     bool
	}{
		{name: "pro-only", model: opus, beta: "context-1m-2025-08-07"},
		{name: "mixed", model: opus, beta: "claude-code-20250219, CONTEXT-1M-2025-08-07", max: true, want: "max"},
		{name: "ordinary-opus", model: opus, want: "pro"},
		{name: "pinned-pro", model: opus, beta: "context-1m-2025-08-07", max: true, pinned: true},
		{name: "neutral-alias", model: "reasoner", target: opus, beta: "context-1m-2025-08-07", max: true, want: "max"},
		{name: "per-auth-alias", model: "reasoner", target: opus, beta: "context-1m-2025-08-07", max: true, want: "max", perAuth: true},
		{name: "alias-to-1m", model: "reasoner", target: opus + "[1m]", max: true, want: "max"},
		{name: "sonnet5", model: "claude-sonnet-5", beta: "context-1m-2025-08-07", want: "pro"},
		{name: "opus-name-to-sonnet5", model: opus, target: "claude-sonnet-5", beta: "context-1m-2025-08-07", want: "pro"},
		{name: "retry-max-only", model: opus, beta: "context-1m-2025-08-07", max: true, retry: true, want: "max2"},
	}
	for _, tc := range cases {
		for _, legacy := range []bool{false, true} {
			for _, stream := range []bool{false, true} {
				t.Run(tc.name+map[bool]string{false: "/fast", true: "/legacy"}[legacy]+map[bool]string{false: "/sync", true: "/stream"}[stream], func(t *testing.T) {
					var selector Selector = &RoundRobinSelector{}
					if legacy {
						selector = &trackingSelector{}
					}
					m := NewManager(nil, selector, nil)
					executor := &authFallbackExecutor{id: "claude"}
					m.RegisterExecutor(executor)
					aliases := []internalconfig.OAuthModelAlias{{Name: tc.target, Alias: tc.model, Fork: true}}
					if tc.target != "" && !tc.perAuth {
						m.SetOAuthModelAlias(map[string][]internalconfig.OAuthModelAlias{"claude": aliases})
					}
					plans := []string{"pro"}
					if tc.max {
						plans = append(plans, "max")
					}
					if tc.retry {
						plans = append(plans, "max2")
						executor.executeErrors = map[string]error{"max": &Error{HTTPStatus: 503, Message: "temporary upstream failure"}}
						executor.streamFirstErrors = executor.executeErrors
					}
					reg := registry.GetGlobalRegistry()
					for _, id := range plans {
						plan := id
						if id == "max2" {
							plan = "max"
						}
						a := &Auth{ID: id, Provider: "claude", Status: StatusActive, ProxyURL: "http://test-proxy:8080", Attributes: map[string]string{"plan_type": plan, "priority": map[string]string{"pro": "100", "max": "10", "max2": "0"}[id]}, Metadata: map[string]any{"extra_usage_enabled": true}}
						if tc.perAuth {
							SetOAuthModelAliasesAttribute(a, aliases)
						}
						if _, err := m.Register(context.Background(), a); err != nil {
							t.Fatal(err)
						}
						models := []*registry.ModelInfo{{ID: tc.model}}
						if tc.target != "" {
							models = append(models, &registry.ModelInfo{ID: tc.target})
						}
						reg.RegisterClient(id, "claude", models)
						t.Cleanup(func() { reg.UnregisterClient(id) })
					}
					selected := ""
					meta := map[string]any{cliproxyexecutor.SelectedAuthCallbackMetadataKey: func(id string) { selected = id }}
					if tc.pinned {
						meta[cliproxyexecutor.PinnedAuthMetadataKey] = "pro"
					}
					opts := cliproxyexecutor.Options{Headers: http.Header{"anthropic-beta": []string{tc.beta}}, Metadata: meta}
					req := cliproxyexecutor.Request{Model: tc.model, Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)}
					var err error
					if stream {
						var result *cliproxyexecutor.StreamResult
						result, err = m.ExecuteStream(context.Background(), []string{"claude"}, req, opts)
						if err == nil {
							for chunk := range result.Chunks {
								if chunk.Err != nil {
									err = chunk.Err
								}
							}
						}
					} else {
						_, err = m.Execute(context.Background(), []string{"claude"}, req, opts)
					}
					calls := executor.ExecuteCalls()
					if stream {
						calls = executor.StreamCalls()
					}
					if tc.want == "" {
						if err == nil || len(calls) != 0 {
							t.Fatalf("expected local rejection without provider call: err=%v calls=%v", err, calls)
						}
					} else if err != nil || selected != tc.want {
						t.Fatalf("selected=%q want=%q err=%v calls=%v", selected, tc.want, err, calls)
					}
					for _, id := range calls {
						if tc.want != "pro" && id == "pro" {
							t.Fatal("Pro was sent an Opus 1M request")
						}
					}
					pro, _ := m.GetByID("pro")
					if tc.want != "pro" && (pro.Unavailable || !pro.NextRetryAfter.IsZero()) {
						t.Fatal("eligibility filtering polluted Pro health")
					}
				})
			}
		}
	}
}

func TestClaudeContextEntitlementMetadataAndScope(t *testing.T) {
	opts := cliproxyexecutor.Options{Headers: http.Header{"Anthropic-Beta": []string{" context-1m-2025-08-07 "}}, Metadata: map[string]any{"keep": "value"}}
	preserved := withClaudeContext1M("claude-opus-4-8", opts)
	preserved.Headers = nil
	if !claudeContext1MRequested("renamed", preserved) {
		t.Fatal("1M capability lost after header rewrite")
	}
	if _, ok := opts.Metadata[claudeContext1MMetadataKey]; ok {
		t.Fatal("caller metadata mutated")
	}
	for _, provider := range []string{"codex", "kiro"} {
		if !authAllowsClaudeContext(&Auth{Provider: provider}, "claude-opus-4-8", preserved) {
			t.Fatalf("affected provider %s", provider)
		}
	}
	if claudeContext1MRequested("claude-opus-4-8", cliproxyexecutor.Options{Headers: http.Header{"Anthropic-Beta": []string{"not-context-1m-2025-08-07"}}}) {
		t.Fatal("matched a beta substring")
	}
	if !strings.Contains(claudeContextEntitlementError().Error(), "subscription") {
		t.Fatal("missing explanation")
	}
}

func TestClaudeContextEntitlementHomeSelection(t *testing.T) {
	for _, plan := range []string{"pro", "max", ""} {
		t.Run(plan, func(t *testing.T) {
			payload, err := json.Marshal(map[string]any{
				"auth":  &Auth{ID: "home-context", Provider: "claude", Attributes: map[string]string{"plan_type": plan}},
				"model": "claude-opus-4-8",
			})
			if err != nil {
				t.Fatal(err)
			}
			dispatcher := &fixtureHomeDispatcher{payload: payload}
			m := newHomeSelectionTestManager(t, dispatcher)
			m.RegisterExecutor(&authFallbackExecutor{id: "claude"})
			opts := cliproxyexecutor.Options{Headers: http.Header{"Anthropic-Beta": []string{"context-1m-2025-08-07"}}}
			selection, err := m.pickHomeDispatchSelection(context.Background(), "claude-opus-4-8", opts)
			if plan == "max" {
				if err != nil || selection == nil {
					t.Fatalf("Max selection=%v err=%v", selection, err)
				}
				selection.End("test_complete")
			} else if err == nil || selection != nil {
				t.Fatalf("ineligible Home auth selected: %v err=%v", selection, err)
			}
			if dispatcher.closedForAmbiguity {
				t.Fatal("entitlement rejection must not invalidate the Home transport")
			}
		})
	}
}
