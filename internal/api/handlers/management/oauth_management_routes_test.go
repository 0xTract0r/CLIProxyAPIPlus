package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	claudeauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/claude"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
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

func TestClaudeOAuthReauthUsesTargetAccountContext(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	authDir := t.TempDir()
	targetPath := filepath.Join(authDir, "claude-existing.json")
	target := &coreauth.Auth{
		ID:       "claude-existing.json",
		FileName: "claude-existing.json",
		Provider: "claude",
		ProxyURL: "socks5://proxy.example:1080",
		Attributes: map[string]string{
			"path": targetPath,
		},
		Metadata: map[string]any{
			"type":          "claude",
			"email":         "old@example.com",
			"access_token":  "old-access",
			"refresh_token": "old-refresh",
			"proxy_url":     "socks5://proxy.example:1080",
			"note":          "keep me",
			"account_settings": map[string]any{
				"schema_version": 1,
				"tls_profile": map[string]any{
					"profile_id": "claude_reqwest_rustls_compatible_v1",
				},
			},
		},
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{
		AuthDir: authDir,
		SDKConfig: config.SDKConfig{
			ProxyURL: "socks5://global-proxy.example:1080",
		},
	}, nil)

	summary := h.claudeOAuthTransportSummary(target)
	if summary["proxy_source"] != "account" {
		t.Fatalf("proxy_source = %q, want account", summary["proxy_source"])
	}
	if summary["tls_profile"] != "claude_reqwest_rustls_compatible_v1" {
		t.Fatalf("tls_profile = %q, want claude_reqwest_rustls_compatible_v1", summary["tls_profile"])
	}

	record := buildClaudeOAuthTokenRecord(target, &claudeauth.ClaudeTokenStorage{
		Email:        "new@example.com",
		AccessToken:  "new-access",
		RefreshToken: "new-refresh",
	})
	if record.ID != target.ID || record.FileName != target.FileName {
		t.Fatalf("record identity = (%q,%q), want (%q,%q)", record.ID, record.FileName, target.ID, target.FileName)
	}
	if got := record.Attributes["path"]; got != targetPath {
		t.Fatalf("record path = %q, want %q", got, targetPath)
	}
	if got := record.ProxyURL; got != target.ProxyURL {
		t.Fatalf("record proxy = %q, want %q", got, target.ProxyURL)
	}
	if got := record.Metadata["note"]; got != "keep me" {
		t.Fatalf("metadata note = %#v, want keep me", got)
	}
	if _, ok := record.Metadata["account_settings"]; !ok {
		t.Fatalf("account_settings was not preserved")
	}
	for _, key := range []string{"access_token", "refresh_token"} {
		if _, ok := record.Metadata[key]; ok {
			t.Fatalf("old %s leaked into metadata", key)
		}
	}
	if got := record.Metadata["email"]; got != "new@example.com" {
		t.Fatalf("metadata email = %#v, want new@example.com", got)
	}
}

func TestClaudeOAuthExchangeRetryClassifier(t *testing.T) {
	retriable := []error{
		contextNSError("token exchange request failed: Post \"https://api.anthropic.com/v1/oauth/token\": socks connect tcp 80.174.217.1:12324->api.anthropic.com:443: unknown error connection not allowed by ruleset"),
		contextNSError("token exchange request failed: Post \"https://api.anthropic.com/v1/oauth/token\": proxyconnect tcp: connection reset by peer"),
		contextNSError("token exchange request failed: Post \"https://api.anthropic.com/v1/oauth/token\": dial tcp: i/o timeout"),
	}
	for _, err := range retriable {
		if !isRetriableClaudeOAuthExchangeError(err) {
			t.Fatalf("expected retriable error for %q", err)
		}
	}

	notRetriable := []error{
		contextNSError("token exchange failed with status 400: {\"error\":\"invalid_grant\"}"),
		contextNSError("failed to parse token response: invalid character"),
		context.Canceled,
		context.DeadlineExceeded,
	}
	for _, err := range notRetriable {
		if isRetriableClaudeOAuthExchangeError(err) {
			t.Fatalf("expected non-retriable error for %q", err)
		}
	}
}

type contextNSError string

func (e contextNSError) Error() string { return string(e) }

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

	record := &coreauth.Auth{ProxyURL: "http://test-proxy:8080",
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
