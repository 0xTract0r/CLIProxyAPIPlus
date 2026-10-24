package management

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

type testMessageCaptureExecutor struct {
	provider string
	authID   string
	model    string
	payload  string
}

func (e *testMessageCaptureExecutor) Identifier() string { return e.provider }

func (e *testMessageCaptureExecutor) Execute(_ context.Context, auth *coreauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if auth != nil {
		e.authID = auth.ID
	}
	e.model = req.Model
	e.payload = string(req.Payload)
	return cliproxyexecutor.Response{Payload: []byte(`{"choices":[{"message":{"content":"OK from pinned auth"}}]}`)}, nil
}

func (e *testMessageCaptureExecutor) ExecuteStream(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}

func (e *testMessageCaptureExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *testMessageCaptureExecutor) CountTokens(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *testMessageCaptureExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

type testMessagePacingExecutor struct {
	testMessageCaptureExecutor
	sends int
}

func (*testMessagePacingExecutor) SupportsHTTPAttemptGate() bool { return true }

func (e *testMessagePacingExecutor) Execute(ctx context.Context, auth *coreauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	gate := cliproxyexecutor.HTTPAttemptGateFromContext(ctx)
	if gate == nil {
		return cliproxyexecutor.Response{}, errors.New("test message bypassed pacing gate")
	}
	permit, err := gate.Before(ctx, cliproxyexecutor.HTTPAttemptInfo{Provider: "claude", Model: req.Model, EstimateKnown: true})
	if err != nil {
		return cliproxyexecutor.Response{}, &cliproxyexecutor.HTTPAttemptGateError{Cause: err}
	}
	if err := permit.MarkSent(ctx); err != nil {
		_ = permit.CancelUnsent(ctx)
		return cliproxyexecutor.Response{}, &cliproxyexecutor.HTTPAttemptGateError{Cause: err}
	}
	e.sends++
	resp, err := e.testMessageCaptureExecutor.Execute(ctx, auth, req, opts)
	if errFinish := permit.Finish(ctx, cliproxyexecutor.HTTPAttemptResult{Complete: true, StatusCode: http.StatusOK}); errFinish != nil {
		return cliproxyexecutor.Response{}, errFinish
	}
	return resp, err
}

func TestTestAuthFileMessageWarmingClaudePacing(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)
	now := time.Now()
	cfg := &config.Config{AuthDir: t.TempDir(), AccountScheduling: config.DefaultAccountSchedulingConfig()}
	cfg.AccountScheduling.WarmupTrafficPacing.Enabled = true
	cfg.AccountScheduling.WarmupTrafficPacing.RequestBurst = 2
	cfg.AccountScheduling.WarmupTrafficPacing.MinAdmissionRequests = 1
	for i := range cfg.AccountScheduling.WarmupCurve {
		cfg.AccountScheduling.WarmupCurve[i].DailyBudget = 2
		cfg.AccountScheduling.WarmupCurve[i].RPMLimit = 100
	}
	selector := coreauth.NewAdaptiveSelector(coreauth.AdaptiveSelectorConfig{Scheduling: cfg.AccountScheduling, SessionAffinity: true}, coreauth.WithAdaptiveClock(func() time.Time { return now }))
	t.Cleanup(selector.Stop)
	manager := coreauth.NewManager(nil, selector, nil)
	manager.SetConfig(cfg)
	t.Cleanup(func() { coreauth.RegisterAccountPacingUsageSink(nil) })
	executor := &testMessagePacingExecutor{testMessageCaptureExecutor: testMessageCaptureExecutor{provider: "claude"}}
	manager.RegisterExecutor(executor)
	const model = "claude-test-message-pacing"
	ids := []string{"claude-probe-one.json", "claude-probe-two.json"}
	for _, id := range ids {
		registry.GetGlobalRegistry().RegisterClient(id, "claude", []*registry.ModelInfo{{ID: model}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
		_, err := manager.Register(context.Background(), &coreauth.Auth{
			ID: id, FileName: id, Provider: "claude", Status: coreauth.StatusActive, ProxyURL: "http://proxy.invalid:8080",
			Metadata: map[string]any{
				"type":               "claude",
				"account_scheduling": map[string]any{"first_production_at": now.Add(-time.Hour).UTC().Format(time.RFC3339)},
				"quota_snapshot":     map[string]any{"profile": map[string]any{"organization": map[string]any{"rate_limit_tier": "default_claude_max_5x"}}},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	now = now.Add(24 * time.Hour)
	h := NewHandlerWithoutConfigFilePath(cfg, manager)
	probe := func(ctx context.Context, id string) *httptest.ResponseRecorder {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"name": id, "model": model, "message": "Reply OK", "max_tokens": 8})
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/v0/management/auth-files/test-message", strings.NewReader(string(body))).WithContext(ctx)
		c.Request.Header.Set("Content-Type", "application/json")
		h.TestAuthFileMessage(c)
		return rec
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if rec := probe(ctx, ids[0]); rec.Code == http.StatusOK {
		t.Fatal("cancelled probe succeeded")
	}
	cancelled := manager.WarmupTrafficPacingSnapshot(ids[0])
	if executor.sends != 0 || cancelled.PendingRequests == nil || *cancelled.PendingRequests != 0 || cancelled.Inflight == nil || *cancelled.Inflight != 0 || cancelled.RequestBalance == nil || *cancelled.RequestBalance != 2 {
		t.Fatalf("cancelled funded probe consumed a reservation: %+v sends=%d", cancelled, executor.sends)
	}
	for _, id := range ids {
		for range 2 {
			rec := probe(context.Background(), id)
			if rec.Code != http.StatusOK || executor.authID != id {
				t.Fatalf("probe %s: status=%d selected=%s body=%s", id, rec.Code, executor.authID, rec.Body.String())
			}
			now = now.Add(time.Second)
		}
		snapshot := manager.WarmupTrafficPacingSnapshot(id)
		if snapshot.Rolling24hRequests == nil || *snapshot.Rolling24hRequests != 2 || snapshot.ActiveBindingGroups == nil || *snapshot.ActiveBindingGroups != 1 {
			t.Fatalf("repeated probes did not share one metered binding: %+v", snapshot)
		}
		if rec := probe(context.Background(), id); rec.Code != http.StatusTooManyRequests {
			t.Fatalf("exhausted probe %s bypassed budget: %d %s", id, rec.Code, rec.Body.String())
		}
	}
	if executor.sends != 4 {
		t.Fatalf("unexpected actual sends: %d", executor.sends)
	}
}

func TestTestAuthFileMessagePinsSelectedAuth(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	targetID := "codex-target.json"
	otherID := "codex-other.json"
	modelID := "test-message-model"
	registry.GetGlobalRegistry().RegisterClient(targetID, "codex", []*registry.ModelInfo{{ID: modelID, Object: "model", Type: "codex"}})
	registry.GetGlobalRegistry().RegisterClient(otherID, "codex", []*registry.ModelInfo{{ID: modelID, Object: "model", Type: "codex"}})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(targetID)
		registry.GetGlobalRegistry().UnregisterClient(otherID)
	})

	executor := &testMessageCaptureExecutor{provider: "codex"}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	for _, id := range []string{targetID, otherID} {
		if _, err := manager.Register(context.Background(), &coreauth.Auth{ProxyURL: "http://test-proxy:8080",
			ID:       id,
			FileName: id,
			Provider: "codex",
			Status:   coreauth.StatusActive,
			Metadata: map[string]any{
				"type":  "codex",
				"email": id + "@example.test",
			},
		}); err != nil {
			t.Fatalf("register %s: %v", id, err)
		}
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v0/management/auth-files/test-message", strings.NewReader(`{"name":"codex-target.json","model":"test-message-model","message":"Reply OK","max_tokens":8}`))
	ctx.Request.Header.Set("Content-Type", "application/json")

	h.TestAuthFileMessage(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if executor.authID != targetID {
		t.Fatalf("executor authID = %q, want %q", executor.authID, targetID)
	}
	if executor.model != modelID {
		t.Fatalf("executor model = %q, want %q", executor.model, modelID)
	}
	if !strings.Contains(executor.payload, "Reply OK") {
		t.Fatalf("payload = %s, want test message", executor.payload)
	}

	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if payload["selected_auth_id"] != targetID || payload["auth_id"] != targetID {
		t.Fatalf("response did not report pinned auth: %#v", payload)
	}
	if payload["output_preview"] != "OK from pinned auth" {
		t.Fatalf("output_preview = %#v", payload["output_preview"])
	}
}

func TestTestAuthFileMessageUsesProviderDefaultModelWhenNoRegisteredModel(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	executor := &testMessageCaptureExecutor{provider: "codex"}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{ProxyURL: "http://test-proxy:8080",
		ID:       "codex-no-model.json",
		FileName: "codex-no-model.json",
		Provider: "codex",
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{"type": "codex", "plan_type": "plus"},
	}); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v0/management/auth-files/test-message", strings.NewReader(`{"name":"codex-no-model.json"}`))
	ctx.Request.Header.Set("Content-Type", "application/json")

	h.TestAuthFileMessage(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if executor.authID != "codex-no-model.json" {
		t.Fatalf("executor authID = %q, want codex-no-model.json", executor.authID)
	}
	// 有限兼容目录已恢复F旧ID；继续核对F原provider默认。
	if executor.model != "gpt-5.4-mini" {
		t.Fatalf("executor model = %q, want gpt-5.4-mini", executor.model)
	}
}

func TestDefaultAuthFileTestMessageModel_ClaudeProCreditsDoesNotPickOpus(t *testing.T) {
	auth := &coreauth.Auth{ProxyURL: "http://test-proxy:8080",
		Provider: "claude",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"plan_type":           "pro",
			"extra_usage_enabled": "true",
		},
	}

	model := defaultAuthFileTestMessageModel(auth)
	if model == "" {
		t.Fatal("expected Claude default test-message model")
	}
	if registry.IsClaudeOpusModelID(model) {
		t.Fatalf("Claude Pro default test-message model = %q, want non-Opus", model)
	}
	if got := authFileSubscriptionPlanType(auth); got != "pro" {
		t.Fatalf("authFileSubscriptionPlanType() = %q, want pro", got)
	}
}

func TestDefaultAuthFileTestMessageModel_ClaudeNestedMaxAllowsOpus(t *testing.T) {
	auth := &coreauth.Auth{ProxyURL: "http://test-proxy:8080",
		Provider: "claude",
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{
			"quota_snapshot": map[string]any{
				"profile": map[string]any{
					"subscription": map[string]any{"has_claude_max": true},
				},
			},
		},
	}

	models := registry.GetClaudeModelsForPlan(registry.NormalizeClaudeSubscriptionPlan(authFileSubscriptionPlanType(auth)), false)
	if !modelsContainID(models, "claude-opus-4-7") {
		t.Fatal("nested Claude Max profile should expose base Opus")
	}
	if got := authFileSubscriptionPlanType(auth); got != "max" {
		t.Fatalf("authFileSubscriptionPlanType() = %q, want max", got)
	}
}

func modelsContainID(models []*registry.ModelInfo, id string) bool {
	for _, model := range models {
		if model != nil && model.ID == id {
			return true
		}
	}
	return false
}

func TestTestAuthFileMessageRequiresModelForUnknownProvider(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	manager := coreauth.NewManager(nil, nil, nil)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{ProxyURL: "http://test-proxy:8080",
		ID:       "unknown-no-model.json",
		FileName: "unknown-no-model.json",
		Provider: "unknown",
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{"type": "unknown"},
	}); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v0/management/auth-files/test-message", strings.NewReader(`{"name":"unknown-no-model.json"}`))
	ctx.Request.Header.Set("Content-Type", "application/json")

	h.TestAuthFileMessage(ctx)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "model is required") {
		t.Fatalf("body = %s, want model error", rec.Body.String())
	}
}
