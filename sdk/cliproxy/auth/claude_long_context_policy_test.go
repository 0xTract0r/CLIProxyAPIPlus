package auth

import (
	"context"
	"net/http"
	"strings"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestManager_Execute_ClaudeSonnetLongContextFailsWithHint(t *testing.T) {
	m := NewManager(nil, nil, nil)
	executor := &authFallbackExecutor{id: "claude"}
	m.RegisterExecutor(executor)

	auth := &Auth{ID: "auth-claude-long-context", Provider: "claude"}
	if _, errRegister := m.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	model := "claude-sonnet-4-6"
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth.ID, "claude", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { reg.UnregisterClient(auth.ID) })

	_, errExecute := m.Execute(context.Background(), []string{"claude"}, cliproxyexecutor.Request{
		Model:   model,
		Payload: longClaudePayload(),
	}, cliproxyexecutor.Options{})
	if errExecute == nil {
		t.Fatal("execute error = nil, want long-context policy error")
	}
	if statusCodeFromError(errExecute) != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want %d", statusCodeFromError(errExecute), http.StatusUnprocessableEntity)
	}
	message := errExecute.Error()
	if !strings.Contains(message, "Sonnet 1M requires Claude extra usage") {
		t.Fatalf("error = %q, want extra usage hint", message)
	}
	if !strings.Contains(message, "opus[1m]") {
		t.Fatalf("error = %q, want opus[1m] hint", message)
	}
	if calls := executor.ExecuteCalls(); len(calls) != 0 {
		t.Fatalf("execute calls = %v, want none", calls)
	}

	updated, ok := m.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatalf("expected auth to be present")
	}
	if updated.Unavailable {
		t.Fatalf("policy failure should not mark auth unavailable")
	}
	if !updated.NextRetryAfter.IsZero() {
		t.Fatalf("policy failure should not set auth cooldown, got %v", updated.NextRetryAfter)
	}
}

func TestManager_Execute_ExplicitSonnet1MAliasIsNotRoutedToOpus(t *testing.T) {
	m := NewManager(nil, nil, nil)
	m.SetConfig(&internalconfig.Config{
		Claude: internalconfig.ClaudeConfig{
			SonnetLongContextPolicy: internalconfig.ClaudeSonnetLongContextPolicyRouteToOpus1M,
		},
	})
	executor := &authFallbackExecutor{id: "claude"}
	m.RegisterExecutor(executor)

	auth := &Auth{ID: "auth-claude-explicit-1m", Provider: "claude"}
	if _, errRegister := m.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	m.SetOAuthModelAlias(map[string][]internalconfig.OAuthModelAlias{
		"claude": {
			{Name: "claude-sonnet-4-6", Alias: "sonnet[1m]", Fork: true},
			{Name: "claude-opus-4-7", Alias: "opus[1m]", Fork: true},
		},
	})

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth.ID, "claude", []*registry.ModelInfo{
		{ID: "claude-sonnet-4-6"},
		{ID: "claude-opus-4-7"},
	})
	t.Cleanup(func() { reg.UnregisterClient(auth.ID) })

	_, errExecute := m.Execute(context.Background(), []string{"claude"}, cliproxyexecutor.Request{
		Model:   "sonnet[1m]",
		Payload: longClaudePayload(),
	}, cliproxyexecutor.Options{
		Metadata: map[string]any{cliproxyexecutor.RequestedModelMetadataKey: "sonnet[1m]"},
	})
	if errExecute == nil {
		t.Fatal("execute error = nil, want long-context policy error")
	}
	message := errExecute.Error()
	if !strings.Contains(message, "route_to_opus_1m is recognized") {
		t.Fatalf("error = %q, want route_to_opus_1m guard", message)
	}
	if !strings.Contains(message, `Requested model "sonnet[1m]" was not changed`) {
		t.Fatalf("error = %q, want explicit requested model preserved", message)
	}
	if strings.Contains(strings.ToLower(message), "claude-opus") {
		t.Fatalf("error = %q, should not rewrite explicit Sonnet alias to Opus", message)
	}
	if calls := executor.ExecuteCalls(); len(calls) != 0 {
		t.Fatalf("execute calls = %v, want none", calls)
	}
}

func longClaudePayload() []byte {
	return []byte(`{"messages":[{"role":"user","content":"` + strings.Repeat("abcd ", 190000) + `"}]}`)
}
