package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
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
		if _, err := manager.Register(context.Background(), &coreauth.Auth{
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
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
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
	if executor.model != "gpt-5.4-mini" {
		t.Fatalf("executor model = %q, want gpt-5.4-mini", executor.model)
	}
}

func TestDefaultAuthFileTestMessageModel_ClaudeProCreditsDoesNotPickOpus(t *testing.T) {
	auth := &coreauth.Auth{
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
	auth := &coreauth.Auth{
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
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
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
