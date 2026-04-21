package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type statusRefreshExecutor struct {
	provider string
	refresh  func(context.Context, *coreauth.Auth) (*coreauth.Auth, error)
}

func (e *statusRefreshExecutor) Identifier() string { return e.provider }

func (e *statusRefreshExecutor) Execute(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *statusRefreshExecutor) ExecuteStream(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}

func (e *statusRefreshExecutor) Refresh(ctx context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	if e.refresh == nil {
		return auth, nil
	}
	return e.refresh(ctx, auth)
}

func (e *statusRefreshExecutor) CountTokens(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *statusRefreshExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func TestCancelOAuthSessionMarksStatusCancelled(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	state := "cancel-test-state"
	RegisterOAuthSession(state, "codex")
	t.Cleanup(func() { CompleteOAuthSession(state) })

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, coreauth.NewManager(nil, nil, nil))

	cancelRec := httptest.NewRecorder()
	cancelCtx, _ := gin.CreateTestContext(cancelRec)
	cancelCtx.Request = httptest.NewRequest(http.MethodDelete, "/v0/management/oauth-session?state="+state, nil)
	h.CancelOAuthSession(cancelCtx)

	if cancelRec.Code != http.StatusOK {
		t.Fatalf("cancel status = %d, body = %s", cancelRec.Code, cancelRec.Body.String())
	}
	var cancelPayload map[string]any
	if err := json.Unmarshal(cancelRec.Body.Bytes(), &cancelPayload); err != nil {
		t.Fatalf("decode cancel payload: %v", err)
	}
	if cancelPayload["status"] != "ok" || cancelPayload["cancelled"] != true {
		t.Fatalf("unexpected cancel payload: %#v", cancelPayload)
	}

	statusRec := httptest.NewRecorder()
	statusCtx, _ := gin.CreateTestContext(statusRec)
	statusCtx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/get-auth-status?state="+state, nil)
	h.GetAuthStatus(statusCtx)

	if statusRec.Code != http.StatusOK {
		t.Fatalf("status code = %d, body = %s", statusRec.Code, statusRec.Body.String())
	}
	var statusPayload map[string]any
	if err := json.Unmarshal(statusRec.Body.Bytes(), &statusPayload); err != nil {
		t.Fatalf("decode status payload: %v", err)
	}
	if statusPayload["status"] != oauthSessionStatusCancelled {
		t.Fatalf("status payload = %#v, want cancelled", statusPayload)
	}
}

func TestRefreshAuthFileStatusRecordsHistory(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	manager.RegisterExecutor(&statusRefreshExecutor{
		provider: "codex",
		refresh: func(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
			if auth.Metadata == nil {
				auth.Metadata = make(map[string]any)
			}
			auth.Metadata["last_refresh"] = "2026-04-22T00:00:00Z"
			return auth, nil
		},
	})

	record := &coreauth.Auth{
		ID:            "codex-route-test.json",
		FileName:      "codex-route-test.json",
		Provider:      "codex",
		Status:        coreauth.StatusError,
		StatusMessage: "stale warning",
		Unavailable:   true,
		Metadata: map[string]any{
			"type":  "codex",
			"email": "route-test@example.com",
		},
	}
	if _, err := manager.Register(context.Background(), record); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(
		http.MethodPost,
		"/v0/management/auth-files/refresh-status",
		strings.NewReader(`{"name":"codex-route-test.json","trigger":"manual"}`),
	)
	ctx.Request.Header.Set("Content-Type", "application/json")
	h.RefreshAuthFileStatus(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("refresh status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode refresh payload: %v", err)
	}
	if payload["status"] != "ok" {
		t.Fatalf("unexpected refresh payload: %#v", payload)
	}

	updated, ok := manager.GetByID("codex-route-test.json")
	if !ok || updated == nil {
		t.Fatalf("expected updated auth")
	}
	if updated.Status != coreauth.StatusActive || updated.Unavailable {
		t.Fatalf("updated status = %q unavailable=%t, want active false", updated.Status, updated.Unavailable)
	}

	events, err := readAuthStatusHistoryEventsFromFile(authStatusHistoryPath(h.cfg.AuthDir), "codex-route-test.json", 5)
	if err != nil {
		t.Fatalf("read auth status history: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	if events[0].EventType != "cleared" {
		t.Fatalf("event_type = %q, want cleared", events[0].EventType)
	}
}
