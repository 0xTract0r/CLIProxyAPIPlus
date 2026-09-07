package auth

import (
	"context"
	"net/http"
	"strings"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestManager_Execute_ClaudeSonnetLongContextFailsWithHint(t *testing.T) {
	m := NewManager(nil, nil, nil)
	executor := &authFallbackExecutor{id: "claude"}
	m.RegisterExecutor(executor)

	auth := &Auth{ProxyURL: "http://test-proxy:8080", ID: "auth-claude-long-context", Provider: "claude"}
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
	if statusCodeFromError(errExecute) != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", statusCodeFromError(errExecute), http.StatusBadRequest)
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

	auth := &Auth{ProxyURL: "http://test-proxy:8080", ID: "auth-claude-explicit-1m", Provider: "claude"}
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

func TestManager_ExecuteStream_ClaudeSonnetLongContextFailsWithHint(t *testing.T) {
	m := NewManager(nil, nil, nil)
	executor := &authFallbackExecutor{id: "claude"}
	m.RegisterExecutor(executor)

	auth := &Auth{ProxyURL: "http://test-proxy:8080", ID: "auth-claude-long-context-stream", Provider: "claude"}
	if _, errRegister := m.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	model := "claude-sonnet-4-6"
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth.ID, "claude", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { reg.UnregisterClient(auth.ID) })

	_, errExecute := m.ExecuteStream(context.Background(), []string{"claude"}, cliproxyexecutor.Request{
		Model:   model,
		Payload: longClaudePayload(),
	}, cliproxyexecutor.Options{})
	if errExecute == nil {
		t.Fatal("execute stream error = nil, want long-context policy error")
	}
	if statusCodeFromError(errExecute) != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", statusCodeFromError(errExecute), http.StatusBadRequest)
	}
	if !strings.Contains(errExecute.Error(), "Sonnet 1M requires Claude extra usage") {
		t.Fatalf("error = %q, want extra usage hint", errExecute.Error())
	}
	if calls := executor.StreamCalls(); len(calls) != 0 {
		t.Fatalf("stream calls = %v, want none", calls)
	}
}

func TestManager_ExecuteCount_ClaudeSonnetLongContextAllowsTokenCounting(t *testing.T) {
	m := NewManager(nil, nil, nil)
	executor := &claudeLongContextCountExecutor{id: "claude"}
	m.RegisterExecutor(executor)

	auth := &Auth{ProxyURL: "http://test-proxy:8080", ID: "auth-claude-long-context-count", Provider: "claude"}
	if _, errRegister := m.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	model := "claude-sonnet-4-6"
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth.ID, "claude", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { reg.UnregisterClient(auth.ID) })

	resp, errExecute := m.ExecuteCount(context.Background(), []string{"claude"}, cliproxyexecutor.Request{
		Model:   model,
		Payload: longClaudePayload(),
	}, cliproxyexecutor.Options{})
	if errExecute != nil {
		t.Fatalf("execute count error = %v, want nil", errExecute)
	}
	if string(resp.Payload) != `{"input_tokens":250000}` {
		t.Fatalf("payload = %s, want input_tokens payload", string(resp.Payload))
	}
	if calls := executor.CountCalls(); len(calls) != 1 || calls[0] != auth.ID {
		t.Fatalf("count calls = %v, want [%s]", calls, auth.ID)
	}
	if calls := executor.ExecuteCalls(); len(calls) != 0 {
		t.Fatalf("execute calls = %v, want none", calls)
	}

	updated, ok := m.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatalf("expected auth to be present")
	}
	if updated.Unavailable {
		t.Fatalf("count_tokens should not mark auth unavailable")
	}
	if !updated.NextRetryAfter.IsZero() {
		t.Fatalf("count_tokens should not set auth cooldown, got %v", updated.NextRetryAfter)
	}
}

func TestManager_Execute_ClaudeContextAboveOneMillionRequiresCompact(t *testing.T) {
	m := NewManager(nil, nil, nil)
	executor := &authFallbackExecutor{id: "claude"}
	m.RegisterExecutor(executor)

	auth := &Auth{ProxyURL: "http://test-proxy:8080", ID: "auth-claude-above-1m", Provider: "claude"}
	if _, errRegister := m.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	model := "claude-opus-4-7"
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth.ID, "claude", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { reg.UnregisterClient(auth.ID) })

	_, errExecute := m.Execute(context.Background(), []string{"claude"}, cliproxyexecutor.Request{
		Model:   model,
		Payload: hugeClaudePayload(),
	}, cliproxyexecutor.Options{})
	if errExecute == nil {
		t.Fatal("execute error = nil, want request-too-large error")
	}
	if statusCodeFromError(errExecute) != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d", statusCodeFromError(errExecute), http.StatusRequestEntityTooLarge)
	}
	message := errExecute.Error()
	if !strings.Contains(message, "request_too_large") {
		t.Fatalf("error = %q, want request_too_large code", message)
	}
	if !strings.Contains(message, "Compact or clear context") {
		t.Fatalf("error = %q, want compact hint", message)
	}
	if !strings.Contains(message, `requested model "claude-opus-4-7" was not changed`) {
		t.Fatalf("error = %q, want requested model preserved", message)
	}
	if calls := executor.ExecuteCalls(); len(calls) != 0 {
		t.Fatalf("execute calls = %v, want none", calls)
	}

	updated, ok := m.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatalf("expected auth to be present")
	}
	if updated.Unavailable {
		t.Fatalf("request-too-large guard should not mark auth unavailable")
	}
	if !updated.NextRetryAfter.IsZero() {
		t.Fatalf("request-too-large guard should not set auth cooldown, got %v", updated.NextRetryAfter)
	}
}

func longClaudePayload() []byte {
	return []byte(`{"messages":[{"role":"user","content":"` + strings.Repeat("abcd ", 190000) + `"}]}`)
}

func TestClaudeSonnet5LongContextPolicyBoundaries(t *testing.T) {
	tests := []struct {
		name, model, requested string
		tokens                 int
		wantStatus             int
	}{
		{name: "native 1m", model: "claude-sonnet-5", tokens: 212779},
		{name: "explicit 1m", model: "claude-sonnet-5[1m]", tokens: 212779},
		{name: "thinking suffix", model: "claude-sonnet-5(high)", tokens: 212779},
		{name: "both suffixes", model: "claude-sonnet-5[1m](high)", tokens: 212779},
		{name: "client alias metadata", model: "claude-sonnet-5", requested: "sonnet[1m]", tokens: 212779},
		{name: "1m boundary", model: "claude-sonnet-5", tokens: 1000000},
		{name: "above 1m", model: "claude-sonnet-5", tokens: 1000001, wantStatus: 413},
		{name: "old sonnet", model: "claude-sonnet-4-6", tokens: 212779, wantStatus: 400},
		{name: "metadata cannot unlock old sonnet", model: "claude-sonnet-4-6", requested: "claude-sonnet-5", tokens: 212779, wantStatus: 400},
		{name: "unresolved alias", model: "sonnet[1m]", tokens: 212779, wantStatus: 400},
		{name: "unknown version", model: "claude-sonnet-50", tokens: 212779, wantStatus: 400},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := NewManager(nil, nil, nil)
			payload := []byte(`{"messages":[{"role":"user","content":"` + strings.Repeat("abcd", tt.tokens) + `"}]}`)
			for _, policy := range []string{
				internalconfig.ClaudeSonnetLongContextPolicyFailWithHint,
				internalconfig.ClaudeSonnetLongContextPolicyCompact,
				internalconfig.ClaudeSonnetLongContextPolicyRouteToOpus1M,
			} {
				m.SetConfig(&internalconfig.Config{Claude: internalconfig.ClaudeConfig{SonnetLongContextPolicy: policy}})
				err := m.guardClaudeLongContextPolicy([]string{"claude"},
					cliproxyexecutor.Request{Model: tt.model, Payload: payload},
					cliproxyexecutor.Options{Metadata: map[string]any{cliproxyexecutor.RequestedModelMetadataKey: tt.requested}})
				if tt.wantStatus == 0 {
					if err != nil {
						t.Fatalf("policy %s rejected native 1M: %v", policy, err)
					}
				} else if err == nil || statusCodeFromError(err) != tt.wantStatus {
					t.Fatalf("policy %s error = %v, want status %d", policy, err, tt.wantStatus)
				}
			}
		})
	}
}

func TestManager_ClaudeSonnet5LongContextReachesExecutor(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "non-stream", true: "stream"}[stream], func(t *testing.T) {
			m := NewManager(nil, nil, nil)
			executor := &authFallbackExecutor{id: "claude"}
			m.RegisterExecutor(executor)
			auth := &Auth{
				ID: "sonnet5-long-context", Provider: "claude", ProxyURL: "http://test-proxy:8080",
				Metadata: map[string]any{"plan_type": "max", "extra_usage_enabled": false},
			}
			if _, err := m.Register(context.Background(), auth); err != nil {
				t.Fatal(err)
			}
			const model = "claude-sonnet-5"
			reg := registry.GetGlobalRegistry()
			reg.RegisterClient(auth.ID, "claude", []*registry.ModelInfo{{ID: model}})
			t.Cleanup(func() { reg.UnregisterClient(auth.ID) })
			req := cliproxyexecutor.Request{Model: model, Payload: longClaudePayload()}
			opts := cliproxyexecutor.Options{Metadata: map[string]any{cliproxyexecutor.RequestedModelMetadataKey: "sonnet[1m]"}}
			if stream {
				result, err := m.ExecuteStream(context.Background(), []string{"claude"}, req, opts)
				if err != nil {
					t.Fatal(err)
				}
				var output string
				for chunk := range result.Chunks {
					if chunk.Err != nil {
						t.Fatal(chunk.Err)
					}
					output += string(chunk.Payload)
				}
				if output != auth.ID || len(executor.StreamCalls()) != 1 {
					t.Fatalf("unexpected stream result %q, calls %v", output, executor.StreamCalls())
				}
			} else {
				result, err := m.Execute(context.Background(), []string{"claude"}, req, opts)
				if err != nil {
					t.Fatal(err)
				}
				if string(result.Payload) != auth.ID || len(executor.ExecuteCalls()) != 1 {
					t.Fatalf("unexpected result %q, calls %v", result.Payload, executor.ExecuteCalls())
				}
			}
		})
	}
}

func hugeClaudePayload() []byte {
	return []byte(`{"messages":[{"role":"user","content":"` + strings.Repeat("abcd ", 850000) + `"}]}`)
}

type claudeLongContextCountExecutor struct {
	id           string
	executeCalls []string
	countCalls   []string
}

func (e *claudeLongContextCountExecutor) Identifier() string {
	return e.id
}

func (e *claudeLongContextCountExecutor) Execute(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.executeCalls = append(e.executeCalls, auth.ID)
	return cliproxyexecutor.Response{Payload: []byte(auth.ID)}, nil
}

func (e *claudeLongContextCountExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, &Error{HTTPStatus: http.StatusInternalServerError, Message: "stream not implemented"}
}

func (e *claudeLongContextCountExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *claudeLongContextCountExecutor) CountTokens(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.countCalls = append(e.countCalls, auth.ID)
	return cliproxyexecutor.Response{Payload: []byte(`{"input_tokens":250000}`)}, nil
}

func (e *claudeLongContextCountExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func (e *claudeLongContextCountExecutor) ExecuteCalls() []string {
	return append([]string(nil), e.executeCalls...)
}

func (e *claudeLongContextCountExecutor) CountCalls() []string {
	return append([]string(nil), e.countCalls...)
}
