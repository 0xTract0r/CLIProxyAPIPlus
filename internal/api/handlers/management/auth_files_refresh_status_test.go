package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type refreshStatusExecutor struct {
	provider string
	refresh  func(ctx context.Context, auth *coreauth.Auth) (*coreauth.Auth, error)
}

func (e *refreshStatusExecutor) Identifier() string { return e.provider }

func (e *refreshStatusExecutor) Execute(ctx context.Context, auth *coreauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *refreshStatusExecutor) ExecuteStream(ctx context.Context, auth *coreauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}

func (e *refreshStatusExecutor) Refresh(ctx context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	if e.refresh == nil {
		return auth, nil
	}
	return e.refresh(ctx, auth)
}

func (e *refreshStatusExecutor) CountTokens(ctx context.Context, auth *coreauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *refreshStatusExecutor) HttpRequest(ctx context.Context, auth *coreauth.Auth, req *http.Request) (*http.Response, error) {
	return nil, nil
}

func TestRefreshAuthFileStatusClearsStaleWarningOnSuccess(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	manager.RegisterExecutor(&refreshStatusExecutor{
		provider: "codex",
		refresh: func(ctx context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
			if auth.Metadata == nil {
				auth.Metadata = make(map[string]any)
			}
			auth.Metadata["last_refresh"] = "2026-04-17T16:00:00Z"
			auth.Metadata["access_token"] = "new-token"
			return auth, nil
		},
	})

	record := &coreauth.Auth{
		ID:            "codex-cory2btc@gmail.com-pro.json",
		FileName:      "codex-cory2btc@gmail.com-pro.json",
		Provider:      "codex",
		Label:         "cory2btc@gmail.com",
		Status:        coreauth.StatusError,
		StatusMessage: "unexpected EOF",
		Unavailable:   true,
		Attributes: map[string]string{
			"path": "/tmp/codex-cory2btc@gmail.com-pro.json",
		},
		Metadata: map[string]any{
			"type":          "codex",
			"email":         "cory2btc@gmail.com",
			"refresh_token": "rt-123",
		},
	}
	if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
		t.Fatalf("register auth record: %v", errRegister)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v0/management/auth-files/refresh-status", strings.NewReader(`{"name":"codex-cory2btc@gmail.com-pro.json"}`))
	req.Header.Set("Content-Type", "application/json")
	ctx.Request = req
	h.RefreshAuthFileStatus(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}

	updated, ok := manager.GetByID("codex-cory2btc@gmail.com-pro.json")
	if !ok || updated == nil {
		t.Fatalf("expected refreshed auth record")
	}
	if updated.Status != coreauth.StatusActive {
		t.Fatalf("status = %q, want %q", updated.Status, coreauth.StatusActive)
	}
	if updated.StatusMessage != "" {
		t.Fatalf("status message = %q, want empty", updated.StatusMessage)
	}
	if updated.Unavailable {
		t.Fatalf("expected unavailable to be false after successful refresh")
	}

	events, errRead := readAuthStatusHistoryEventsFromFile(
		authStatusHistoryPath(h.cfg.AuthDir),
		"codex-cory2btc@gmail.com-pro.json",
		5,
	)
	if errRead != nil {
		t.Fatalf("read auth status history: %v", errRead)
	}
	if len(events) != 1 {
		t.Fatalf("history events = %d, want 1", len(events))
	}
	if events[0].EventType != "cleared" {
		t.Fatalf("event_type = %q, want %q", events[0].EventType, "cleared")
	}
	if events[0].PreviousMessage != "unexpected EOF" {
		t.Fatalf("previous_message = %q, want %q", events[0].PreviousMessage, "unexpected EOF")
	}
	if events[0].Status != string(coreauth.StatusActive) {
		t.Fatalf("status = %q, want %q", events[0].Status, coreauth.StatusActive)
	}
}

func TestRefreshAuthFileStatusPersistsCurrentFailure(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	manager.RegisterExecutor(&refreshStatusExecutor{
		provider: "codex",
		refresh: func(ctx context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
			return nil, &coreauth.Error{
				Code:       "refresh_failed",
				Message:    "token refresh failed with status 401: invalid_grant",
				Retryable:  false,
				HTTPStatus: http.StatusUnauthorized,
			}
		},
	})

	record := &coreauth.Auth{
		ID:       "codex-cory2btc@gmail.com-pro.json",
		FileName: "codex-cory2btc@gmail.com-pro.json",
		Provider: "codex",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"path": "/tmp/codex-cory2btc@gmail.com-pro.json",
		},
		Metadata: map[string]any{
			"type":          "codex",
			"email":         "cory2btc@gmail.com",
			"refresh_token": "rt-123",
		},
	}
	if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
		t.Fatalf("register auth record: %v", errRegister)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v0/management/auth-files/refresh-status", strings.NewReader(`{"name":"codex-cory2btc@gmail.com-pro.json"}`))
	req.Header.Set("Content-Type", "application/json")
	ctx.Request = req
	h.RefreshAuthFileStatus(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}

	updated, ok := manager.GetByID("codex-cory2btc@gmail.com-pro.json")
	if !ok || updated == nil {
		t.Fatalf("expected refreshed auth record")
	}
	if updated.Status != coreauth.StatusError {
		t.Fatalf("status = %q, want %q", updated.Status, coreauth.StatusError)
	}
	if updated.StatusMessage != "token refresh failed with status 401: invalid_grant" {
		t.Fatalf("status message = %q, want %q", updated.StatusMessage, "token refresh failed with status 401: invalid_grant")
	}
	if !updated.Unavailable {
		t.Fatalf("expected unavailable to be true after failed refresh")
	}

	events, errRead := readAuthStatusHistoryEventsFromFile(
		authStatusHistoryPath(h.cfg.AuthDir),
		"codex-cory2btc@gmail.com-pro.json",
		5,
	)
	if errRead != nil {
		t.Fatalf("read auth status history: %v", errRead)
	}
	if len(events) != 1 {
		t.Fatalf("history events = %d, want 1", len(events))
	}
	if events[0].EventType != "warning" {
		t.Fatalf("event_type = %q, want %q", events[0].EventType, "warning")
	}
	if events[0].Error == "" {
		t.Fatal("expected history error to be recorded")
	}
}

func TestRefreshAuthFileStatusLeavesTransientPreExpiryFailureNonRed(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	manager.RegisterExecutor(&refreshStatusExecutor{
		provider: "claude",
		refresh: func(ctx context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
			return nil, &coreauth.Error{
				Code:      "refresh_failed",
				Message:   "unexpected EOF",
				Retryable: true,
			}
		},
	})

	record := &coreauth.Auth{
		ID:       "claude-transient.json",
		FileName: "claude-transient.json",
		Provider: "claude",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"path": "/tmp/claude-transient.json",
		},
		Metadata: map[string]any{
			"type":          "claude",
			"email":         "claude@example.com",
			"refresh_token": "rt-123",
			"expired":       time.Now().Add(2 * time.Hour).Format(time.RFC3339),
		},
	}
	if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
		t.Fatalf("register auth record: %v", errRegister)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v0/management/auth-files/refresh-status", strings.NewReader(`{"name":"claude-transient.json","trigger":"auto"}`))
	req.Header.Set("Content-Type", "application/json")
	ctx.Request = req
	h.RefreshAuthFileStatus(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}

	var payload struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		File   struct {
			Name          string          `json:"name"`
			Status        coreauth.Status `json:"status"`
			StatusMessage string          `json:"status_message"`
			Unavailable   bool            `json:"unavailable"`
		} `json:"file"`
	}
	if errDecode := json.Unmarshal(rec.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if payload.Status != "error" {
		t.Fatalf("response status = %q, want %q", payload.Status, "error")
	}
	if payload.Error == "" || !strings.Contains(strings.ToLower(payload.Error), "eof") {
		t.Fatalf("response error = %q, want EOF context", payload.Error)
	}
	if payload.File.Name != "claude-transient.json" {
		t.Fatalf("file name = %q, want %q", payload.File.Name, "claude-transient.json")
	}
	if payload.File.Status != coreauth.StatusActive {
		t.Fatalf("file status = %q, want %q", payload.File.Status, coreauth.StatusActive)
	}
	if payload.File.StatusMessage != "" {
		t.Fatalf("file status_message = %q, want empty", payload.File.StatusMessage)
	}
	if payload.File.Unavailable {
		t.Fatal("expected response file to remain available for transient refresh failure")
	}

	updated, ok := manager.GetByID("claude-transient.json")
	if !ok || updated == nil {
		t.Fatalf("expected refreshed auth record")
	}
	if updated.Status != coreauth.StatusActive {
		t.Fatalf("persisted status = %q, want %q", updated.Status, coreauth.StatusActive)
	}
	if updated.StatusMessage != "" {
		t.Fatalf("persisted status message = %q, want empty", updated.StatusMessage)
	}
	if updated.Unavailable {
		t.Fatal("expected persisted auth to remain available after transient refresh failure")
	}

	events, errRead := readAuthStatusHistoryEventsFromFile(
		authStatusHistoryPath(h.cfg.AuthDir),
		"claude-transient.json",
		5,
	)
	if errRead != nil {
		t.Fatalf("read auth status history: %v", errRead)
	}
	if len(events) != 1 {
		t.Fatalf("history events = %d, want 1", len(events))
	}
	if events[0].EventType != "check_failed" {
		t.Fatalf("event_type = %q, want %q", events[0].EventType, "check_failed")
	}
	if events[0].Trigger != authStatusHistoryTriggerAuto {
		t.Fatalf("trigger = %q, want %q", events[0].Trigger, authStatusHistoryTriggerAuto)
	}
	if events[0].Status != string(coreauth.StatusActive) {
		t.Fatalf("status = %q, want %q", events[0].Status, coreauth.StatusActive)
	}
}

func TestRefreshAuthFileStatusKeepsPreExpiryRefreshTokenReusedUsable(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	manager.RegisterExecutor(&refreshStatusExecutor{
		provider: "codex",
		refresh: func(ctx context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
			return nil, &coreauth.Error{
				Code:       "refresh_failed",
				Message:    `token refresh failed with status 401: {"error":"invalid_grant","error_description":"refresh_token_reused"}`,
				Retryable:  false,
				HTTPStatus: http.StatusUnauthorized,
			}
		},
	})

	record := &coreauth.Auth{
		ID:       "codex-reused.json",
		FileName: "codex-reused.json",
		Provider: "codex",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"path": "/tmp/codex-reused.json",
		},
		Metadata: map[string]any{
			"type":          "codex",
			"email":         "codex@example.com",
			"refresh_token": "rt-123",
			"expired":       time.Now().Add(90 * time.Minute).Format(time.RFC3339),
		},
	}
	if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
		t.Fatalf("register auth record: %v", errRegister)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v0/management/auth-files/refresh-status", strings.NewReader(`{"name":"codex-reused.json"}`))
	req.Header.Set("Content-Type", "application/json")
	ctx.Request = req
	h.RefreshAuthFileStatus(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}

	var payload struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		File   struct {
			Name          string          `json:"name"`
			Status        coreauth.Status `json:"status"`
			StatusMessage string          `json:"status_message"`
			Unavailable   bool            `json:"unavailable"`
		} `json:"file"`
	}
	if errDecode := json.Unmarshal(rec.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if payload.Status != "ok" {
		t.Fatalf("response status = %q, want %q", payload.Status, "ok")
	}
	if payload.Error != "" {
		t.Fatalf("response error = %q, want empty for grace-state refresh warning", payload.Error)
	}
	if payload.File.Status != coreauth.StatusActive {
		t.Fatalf("file status = %q, want %q", payload.File.Status, coreauth.StatusActive)
	}
	if payload.File.Unavailable {
		t.Fatal("expected response file to remain available while access token is still valid")
	}
	if !strings.Contains(payload.File.StatusMessage, "rotated by a newer session") {
		t.Fatalf("file status_message = %q, want rotated-session hint", payload.File.StatusMessage)
	}
	if !strings.Contains(payload.File.StatusMessage, "current access token remains usable until") {
		t.Fatalf("file status_message = %q, want usable-until hint", payload.File.StatusMessage)
	}
	if !strings.Contains(payload.File.StatusMessage, "reauthenticate as soon as possible") {
		t.Fatalf("file status_message = %q, want reauth hint", payload.File.StatusMessage)
	}

	updated, ok := manager.GetByID("codex-reused.json")
	if !ok || updated == nil {
		t.Fatalf("expected refreshed auth record")
	}
	if updated.Status != coreauth.StatusActive {
		t.Fatalf("persisted status = %q, want %q", updated.Status, coreauth.StatusActive)
	}
	if updated.Unavailable {
		t.Fatal("expected persisted auth to remain available")
	}
	if !strings.Contains(updated.StatusMessage, "rotated by a newer session") {
		t.Fatalf("persisted status message = %q, want rotated-session hint", updated.StatusMessage)
	}
	if strings.Contains(updated.StatusMessage, "refresh_token_reused") {
		t.Fatalf("persisted status message = %q, should hide raw refresh_token_reused detail", updated.StatusMessage)
	}
	if updated.LastError == nil || !strings.Contains(strings.ToLower(updated.LastError.Message), "refresh_token_reused") {
		t.Fatalf("persisted last error = %#v, want original refresh_token_reused detail", updated.LastError)
	}

	events, errRead := readAuthStatusHistoryEventsFromFile(
		authStatusHistoryPath(h.cfg.AuthDir),
		"codex-reused.json",
		5,
	)
	if errRead != nil {
		t.Fatalf("read auth status history: %v", errRead)
	}
	if len(events) != 1 {
		t.Fatalf("history events = %d, want 1", len(events))
	}
	if events[0].EventType != "warning" {
		t.Fatalf("event_type = %q, want %q", events[0].EventType, "warning")
	}
	if events[0].Status != string(coreauth.StatusActive) {
		t.Fatalf("status = %q, want %q", events[0].Status, coreauth.StatusActive)
	}
}
