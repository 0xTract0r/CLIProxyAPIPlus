package auth

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	homekv "github.com/router-for-me/CLIProxyAPI/v8/internal/home"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
)

type antigravityCreditsFallbackExecutor struct {
	executeCreditsRequested []bool
	streamCreditsRequested  []bool
}

func (e *antigravityCreditsFallbackExecutor) Identifier() string { return "antigravity" }

func (e *antigravityCreditsFallbackExecutor) Execute(ctx context.Context, _ *Auth, req cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	creditsRequested := AntigravityCreditsRequested(ctx)
	e.executeCreditsRequested = append(e.executeCreditsRequested, creditsRequested)
	if !creditsRequested {
		return cliproxyexecutor.Response{}, &Error{HTTPStatus: http.StatusTooManyRequests, Message: "quota exhausted"}
	}
	return cliproxyexecutor.Response{
		Payload: []byte("credits fallback"),
		Headers: http.Header{"X-Credits": {req.Model}},
	}, nil
}

func (e *antigravityCreditsFallbackExecutor) ExecuteStream(ctx context.Context, _ *Auth, req cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	creditsRequested := AntigravityCreditsRequested(ctx)
	e.streamCreditsRequested = append(e.streamCreditsRequested, creditsRequested)
	ch := make(chan cliproxyexecutor.StreamChunk, 1)
	if !creditsRequested {
		ch <- cliproxyexecutor.StreamChunk{Err: &Error{HTTPStatus: http.StatusTooManyRequests, Message: "quota exhausted"}}
		close(ch)
		return &cliproxyexecutor.StreamResult{Headers: http.Header{"X-Initial": {req.Model}}, Chunks: ch}, nil
	}
	ch <- cliproxyexecutor.StreamChunk{Payload: []byte("credits fallback")}
	close(ch)
	return &cliproxyexecutor.StreamResult{Headers: http.Header{"X-Credits": {req.Model}}, Chunks: ch}, nil
}

func (e *antigravityCreditsFallbackExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *antigravityCreditsFallbackExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, &Error{HTTPStatus: http.StatusNotImplemented, Message: "CountTokens not implemented"}
}

func (e *antigravityCreditsFallbackExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, &Error{HTTPStatus: http.StatusNotImplemented, Message: "HttpRequest not implemented"}
}

type codexOnlyFailureExecutor struct{}

func (codexOnlyFailureExecutor) Identifier() string { return "codex" }

func (codexOnlyFailureExecutor) Execute(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, &Error{HTTPStatus: http.StatusTooManyRequests, Message: "codex quota exhausted"}
}

func (codexOnlyFailureExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, &Error{HTTPStatus: http.StatusTooManyRequests, Message: "codex quota exhausted"}
}

func (codexOnlyFailureExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (codexOnlyFailureExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, &Error{HTTPStatus: http.StatusTooManyRequests, Message: "codex quota exhausted"}
}

func (codexOnlyFailureExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, &Error{HTTPStatus: http.StatusTooManyRequests, Message: "codex quota exhausted"}
}

type captureLogHook struct {
	messages []string
}

func (h *captureLogHook) Levels() []log.Level {
	return log.AllLevels
}

func (h *captureLogHook) Fire(entry *log.Entry) error {
	h.messages = append(h.messages, entry.Message)
	return nil
}

func TestManagerExecuteStream_AntigravityPreservesBootstrapRefusal(t *testing.T) {
	const model = "claude-opus-4-6-thinking"
	executor := &antigravityCreditsFallbackExecutor{}
	manager := NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	registry.GetGlobalRegistry().RegisterClient("ag-ordinary-refusal", "antigravity", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient("ag-ordinary-refusal") })
	if _, err := manager.Register(context.Background(), &Auth{ID: "ag-ordinary-refusal", Provider: "antigravity"}); err != nil {
		t.Fatal(err)
	}
	stream, err := manager.ExecuteStream(context.Background(), []string{"antigravity"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if err == nil {
		for chunk := range stream.Chunks {
			if len(chunk.Payload) != 0 || err != nil || chunk.Err == nil {
				t.Fatalf("unexpected chunk: %+v", chunk)
			}
			err = chunk.Err
		}
	}
	if statusCodeFromError(err) != http.StatusTooManyRequests || !strings.Contains(err.Error(), "quota exhausted") {
		t.Fatalf("want original 429, got %v", err)
	}
	if calls := executor.streamCreditsRequested; len(calls) != 1 || calls[0] {
		t.Fatalf("want one standard call, got %v", calls)
	}
}

func TestManagerExecuteStream_AntigravityCreditsHomeModeFailsClosedWithoutDispatch(t *testing.T) {
	const model = "claude-opus-4-6-thinking"
	executor := &antigravityCreditsFallbackExecutor{}
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{
		Home:          internalconfig.HomeConfig{Enabled: true},
		QuotaExceeded: internalconfig.QuotaExceeded{AntigravityCredits: true},
	})
	manager.RegisterExecutor(executor)
	registry.GetGlobalRegistry().RegisterClient("ag-credits-home-kv", "antigravity", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient("ag-credits-home-kv") })
	homekv.SetCurrent(homekv.New(internalconfig.HomeConfig{Enabled: false}))
	t.Cleanup(homekv.ClearCurrent)
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "ag-credits-home-kv", Provider: "antigravity"}); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	_, errExecute := manager.ExecuteStream(context.Background(), []string{"antigravity"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if errExecute == nil {
		t.Fatal("ExecuteStream() error = nil, want home kv unavailable error")
	}
	if status := statusCodeFromError(errExecute); status != http.StatusServiceUnavailable {
		t.Fatalf("ExecuteStream() status = %d, want %d; err=%v", status, http.StatusServiceUnavailable, errExecute)
	}
	if !strings.Contains(errExecute.Error(), "home dispatch bundle unavailable") {
		t.Fatalf("ExecuteStream() error = %v, want home dispatch bundle unavailable", errExecute)
	}
}

func TestManagerExecuteStream_CodexOnlyDoesNotEnterAntigravityCreditsFallback(t *testing.T) {
	const model = "gpt-5.5"
	logger := log.StandardLogger()
	oldLevel := logger.GetLevel()
	oldHooks := logger.ReplaceHooks(make(log.LevelHooks))
	hook := &captureLogHook{}
	logger.SetLevel(log.DebugLevel)
	logger.AddHook(hook)
	t.Cleanup(func() {
		logger.SetLevel(oldLevel)
		logger.ReplaceHooks(oldHooks)
	})

	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{
		QuotaExceeded: internalconfig.QuotaExceeded{AntigravityCredits: true},
	})
	manager.RegisterExecutor(codexOnlyFailureExecutor{})
	manager.RegisterExecutor(&antigravityCreditsFallbackExecutor{})
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient("codex-only", "codex", []*registry.ModelInfo{{ID: model}})
	reg.RegisterClient("ag-unrelated", "antigravity", []*registry.ModelInfo{{ID: "gemini-3-flash"}})
	t.Cleanup(func() {
		reg.UnregisterClient("codex-only")
		reg.UnregisterClient("ag-unrelated")
	})
	if _, errRegister := manager.Register(context.Background(), &Auth{ProxyURL: "http://test-proxy:8080", ID: "codex-only", Provider: "codex"}); errRegister != nil {
		t.Fatalf("register codex auth: %v", errRegister)
	}
	if _, errRegister := manager.Register(context.Background(), &Auth{ProxyURL: "http://test-proxy:8080", ID: "ag-unrelated", Provider: "antigravity"}); errRegister != nil {
		t.Fatalf("register antigravity auth: %v", errRegister)
	}

	_, errExecute := manager.ExecuteStream(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if errExecute == nil {
		t.Fatal("expected codex execution failure")
	}

	for _, message := range hook.messages {
		if strings.Contains(message, "shouldAttemptAntigravityCreditsFallback") {
			t.Fatalf("codex-only request entered antigravity credits fallback gate; messages=%v", hook.messages)
		}
	}
}

func TestStatusCodeFromError_UnwrapsStreamBootstrap429(t *testing.T) {
	bootstrapErr := newStreamBootstrapError(&Error{HTTPStatus: http.StatusTooManyRequests, Message: "quota exhausted"}, nil)
	wrappedErr := fmt.Errorf("conductor stream failed: %w", bootstrapErr)

	if status := statusCodeFromError(wrappedErr); status != http.StatusTooManyRequests {
		t.Fatalf("statusCodeFromError() = %d, want %d", status, http.StatusTooManyRequests)
	}
}

func TestIsAuthBlockedForModel_ClaudeWithCreditsStillBlockedDuringCooldown(t *testing.T) {
	auth := &Auth{ProxyURL: "http://test-proxy:8080",
		ID:       "ag-1",
		Provider: "antigravity",
		ModelStates: map[string]*ModelState{
			"claude-sonnet-4-6": {
				Unavailable:    true,
				NextRetryAfter: time.Now().Add(10 * time.Minute),
				Quota: QuotaState{
					Exceeded:      true,
					NextRecoverAt: time.Now().Add(10 * time.Minute),
				},
			},
		},
	}

	SetAntigravityCreditsHint(auth.ID, AntigravityCreditsHint{
		Known:     true,
		Available: true,
		UpdatedAt: time.Now(),
	})

	blocked, reason, _ := isAuthBlockedForModel(auth, "claude-sonnet-4-6", time.Now())
	if !blocked || reason != blockReasonCooldown {
		t.Fatalf("expected auth to be blocked during cooldown even with credits, got blocked=%v reason=%v", blocked, reason)
	}
}

func TestIsAuthBlockedForModel_KeepsGeminiBlockedWithoutCreditsBypass(t *testing.T) {
	auth := &Auth{ProxyURL: "http://test-proxy:8080",
		ID:       "ag-2",
		Provider: "antigravity",
		ModelStates: map[string]*ModelState{
			"gemini-3-flash": {
				Unavailable:    true,
				NextRetryAfter: time.Now().Add(10 * time.Minute),
				Quota: QuotaState{
					Exceeded:      true,
					NextRecoverAt: time.Now().Add(10 * time.Minute),
				},
			},
		},
	}

	SetAntigravityCreditsHint(auth.ID, AntigravityCreditsHint{
		Known:     true,
		Available: true,
		UpdatedAt: time.Now(),
	})

	blocked, reason, _ := isAuthBlockedForModel(auth, "gemini-3-flash", time.Now())
	if !blocked || reason != blockReasonCooldown {
		t.Fatalf("expected gemini model to remain blocked, got blocked=%v reason=%v", blocked, reason)
	}
}

func TestManagerExecuteAndStream_ExpiredNegativeCreditsHintRespectsModelCooldown(t *testing.T) {
	for _, stream := range []bool{false, true} {
		name := "execute"
		if stream {
			name = "execute_stream"
		}
		t.Run(name, func(t *testing.T) {
			authID := "ag-expired-negative-hint-" + name
			const model = "claude-sonnet-4-6"
			executor := &antigravityCreditsFallbackExecutor{}
			manager := NewManager(nil, nil, nil)
			manager.SetConfig(&internalconfig.Config{
				QuotaExceeded: internalconfig.QuotaExceeded{AntigravityCredits: true},
			})
			manager.RegisterExecutor(executor)
			registry.GetGlobalRegistry().RegisterClient(authID, "antigravity", []*registry.ModelInfo{{ID: model}})
			t.Cleanup(func() {
				registry.GetGlobalRegistry().UnregisterClient(authID)
				SetAntigravityCreditsHint(authID, AntigravityCreditsHint{})
			})

			cooldownUntil := time.Now().Add(30 * time.Minute)
			if _, errRegister := manager.Register(context.Background(), &Auth{
				ID:       authID,
				Provider: "antigravity",
				ModelStates: map[string]*ModelState{
					model: {
						Unavailable:    true,
						NextRetryAfter: cooldownUntil,
						Quota: QuotaState{
							Exceeded:      true,
							NextRecoverAt: cooldownUntil,
						},
					},
				},
			}); errRegister != nil {
				t.Fatalf("register auth: %v", errRegister)
			}

			// Fresh negative hint must still suppress credits fallback while model is in cooldown.
			SetAntigravityCreditsHint(authID, AntigravityCreditsHint{
				Known:     true,
				Available: false,
				UpdatedAt: time.Now(),
			})
			if stream {
				if _, errStream := manager.ExecuteStream(context.Background(), []string{"antigravity"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{}); errStream == nil {
					t.Fatal("ExecuteStream() succeeded with fresh negative credits hint, want cooldown error")
				}
			} else {
				if _, errExec := manager.Execute(context.Background(), []string{"antigravity"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{}); errExec == nil {
					t.Fatal("Execute() succeeded with fresh negative credits hint, want cooldown error")
				}
			}
			if len(executor.executeCreditsRequested) != 0 || len(executor.streamCreditsRequested) != 0 {
				t.Fatalf("executor calls during fresh negative hint = (%d, %d), want (0, 0)", len(executor.executeCreditsRequested), len(executor.streamCreditsRequested))
			}

			// An expired hint leaves the existing cooldown in force.
			SetAntigravityCreditsHint(authID, AntigravityCreditsHint{
				Known:     true,
				Available: false,
				UpdatedAt: time.Now().Add(-AntigravityCreditsHintRefreshInterval - time.Second),
			})
			var err error
			req := cliproxyexecutor.Request{Model: model}
			if stream {
				_, err = manager.ExecuteStream(context.Background(), []string{"antigravity"}, req, cliproxyexecutor.Options{})
			} else {
				_, err = manager.Execute(context.Background(), []string{"antigravity"}, req, cliproxyexecutor.Options{})
			}
			if statusCodeFromError(err) != http.StatusTooManyRequests || SafeResponseHeaders(err).Get("Retry-After") == "" {
				t.Fatalf("want original cooldown and deadline, got %v", err)
			}
			if len(executor.executeCreditsRequested) != 0 || len(executor.streamCreditsRequested) != 0 {
				t.Fatalf("cooldown dispatched: execute=%v stream=%v", executor.executeCreditsRequested, executor.streamCreditsRequested)
			}
		})
	}
}
