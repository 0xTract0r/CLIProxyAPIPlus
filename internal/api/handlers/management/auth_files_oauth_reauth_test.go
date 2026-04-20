package management

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestGetAuthStatusReturnsCancelledAfterCancel(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	state := "qwen-cancelled-state"
	RegisterOAuthSession(state, "qwen")
	t.Cleanup(func() {
		CompleteOAuthSession(state)
	})

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, coreauth.NewManager(nil, nil, nil))

	cancelRec := httptest.NewRecorder()
	cancelCtx, _ := gin.CreateTestContext(cancelRec)
	cancelCtx.Request = httptest.NewRequest(http.MethodDelete, "/v0/management/oauth-session?state="+state, nil)
	h.CancelOAuthSession(cancelCtx)

	if cancelRec.Code != http.StatusOK {
		t.Fatalf("cancel status = %d, want %d", cancelRec.Code, http.StatusOK)
	}

	var cancelResp map[string]any
	if err := json.Unmarshal(cancelRec.Body.Bytes(), &cancelResp); err != nil {
		t.Fatalf("decode cancel response: %v", err)
	}
	if got, ok := cancelResp["cancelled"].(bool); !ok || !got {
		t.Fatalf("cancelled = %#v, want true", cancelResp["cancelled"])
	}

	// Cancelled state should remain visible even if a leaked goroutine tries to write an error later.
	SetOAuthSessionError(state, "Authentication failed")

	statusRec := httptest.NewRecorder()
	statusCtx, _ := gin.CreateTestContext(statusRec)
	statusCtx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/oauth-status?state="+state, nil)
	h.GetAuthStatus(statusCtx)

	if statusRec.Code != http.StatusOK {
		t.Fatalf("status code = %d, want %d", statusRec.Code, http.StatusOK)
	}

	var statusResp map[string]any
	if err := json.Unmarshal(statusRec.Body.Bytes(), &statusResp); err != nil {
		t.Fatalf("decode status response: %v", err)
	}
	if got := statusResp["status"]; got != oauthSessionStatusCancelled {
		t.Fatalf("status = %#v, want %q", got, oauthSessionStatusCancelled)
	}
}

func TestCancelOAuthSessionStateCancelsAttachedContext(t *testing.T) {
	state := "kimi-cancel-hook"
	RegisterOAuthSession(state, "kimi")
	t.Cleanup(func() {
		CompleteOAuthSession(state)
	})

	sessionCtx, cancel := context.WithCancel(context.Background())
	if !SetOAuthSessionCancel(state, cancel) {
		t.Fatal("expected SetOAuthSessionCancel to succeed")
	}
	if !CancelOAuthSessionState(state) {
		t.Fatal("expected cancel to succeed")
	}

	select {
	case <-sessionCtx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("expected session context to be cancelled")
	}

	_, status, ok := GetOAuthSession(state)
	if !ok {
		t.Fatal("expected cancelled session to remain queryable")
	}
	if !isOAuthSessionCancelledStatus(status) {
		t.Fatalf("status = %q, want cancelled", status)
	}
}

func TestSaveOAuthTokenRecordReplacesTargetAuthAndPreservesEditableFields(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	authDir := t.TempDir()
	store := &fileBackedTestAuthStore{baseDir: authDir}
	manager := coreauth.NewManager(store, nil, nil)
	targetAuth := &coreauth.Auth{
		ID:       "claude-old.json",
		FileName: "claude-old.json",
		Provider: "claude",
		Prefix:   "team-a",
		ProxyURL: "http://proxy.local",
		Disabled: true,
		Attributes: map[string]string{
			"path":           filepath.Join(authDir, "claude-old.json"),
			"header:X-Test":  "old-value",
			"priority":       "7",
			"note":           "keep me",
			"runtime_only":   "false",
			"header:X-Trace": "trace-value",
		},
		Metadata: map[string]any{
			"type":    "claude",
			"email":   "old@example.com",
			"headers": map[string]any{"X-Test": "old-value"},
		},
	}
	if _, err := manager.Register(context.Background(), targetAuth); err != nil {
		t.Fatalf("register target auth: %v", err)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	h.tokenStore = store

	RegisterOAuthSessionWithTarget("state-reauth", "anthropic", "claude-old.json")
	t.Cleanup(func() {
		CompleteOAuthSession("state-reauth")
	})

	record := &coreauth.Auth{
		ID:       "claude-new.json",
		FileName: "claude-new.json",
		Provider: "claude",
		Metadata: map[string]any{
			"type":               "claude",
			"email":              "new@example.com",
			"access_token":       "new-token",
			"refresh_token":      "new-refresh-token",
			"plan_type":          "pro",
			"chatgpt_account_id": "acct-new-123",
		},
	}

	if _, err := h.saveOAuthTokenRecord(context.Background(), "state-reauth", record); err != nil {
		t.Fatalf("saveOAuthTokenRecord returned error: %v", err)
	}

	store.mu.Lock()
	saved := store.items["claude-old.json"]
	store.mu.Unlock()

	if saved == nil {
		t.Fatalf("expected replacement auth to be saved under original id")
	}
	if saved.ID != "claude-old.json" {
		t.Fatalf("saved id = %q, want %q", saved.ID, "claude-old.json")
	}
	if saved.FileName != "claude-old.json" {
		t.Fatalf("saved file name = %q, want %q", saved.FileName, "claude-old.json")
	}
	if saved.Prefix != "team-a" {
		t.Fatalf("saved prefix = %q, want %q", saved.Prefix, "team-a")
	}
	if saved.ProxyURL != "http://proxy.local" {
		t.Fatalf("saved proxy url = %q, want %q", saved.ProxyURL, "http://proxy.local")
	}
	if !saved.Disabled {
		t.Fatalf("expected saved auth to preserve disabled flag")
	}
	if got := saved.Attributes["path"]; got != filepath.Join(authDir, "claude-old.json") {
		t.Fatalf("saved path = %q, want %q", got, filepath.Join(authDir, "claude-old.json"))
	}
	if got, _ := saved.Metadata["email"].(string); got != "new@example.com" {
		t.Fatalf("saved metadata email = %q, want %q", got, "new@example.com")
	}
	if got, _ := saved.Metadata["access_token"].(string); got != "new-token" {
		t.Fatalf("saved metadata access_token = %q, want %q", got, "new-token")
	}
	headers, ok := saved.Metadata["headers"].(map[string]any)
	if !ok {
		t.Fatalf("saved metadata headers = %T, want map[string]any", saved.Metadata["headers"])
	}
	if got := headers["X-Test"]; got != "old-value" {
		t.Fatalf("saved metadata headers[X-Test] = %#v, want %q", got, "old-value")
	}
	if got := headers["X-Trace"]; got != "trace-value" {
		t.Fatalf("saved metadata headers[X-Trace] = %#v, want %q", got, "trace-value")
	}
	if got, ok := saved.Metadata["priority"].(int); !ok || got != 7 {
		t.Fatalf("saved metadata priority = %#v, want %d", saved.Metadata["priority"], 7)
	}
	if got, _ := saved.Metadata["note"].(string); got != "keep me" {
		t.Fatalf("saved metadata note = %q, want %q", got, "keep me")
	}
	if got, ok := saved.Metadata["disabled"].(bool); !ok || !got {
		t.Fatalf("saved metadata disabled = %#v, want true", saved.Metadata["disabled"])
	}

	events := readOAuthReauthHistoryEvents(t, authDir)
	if len(events) != 1 {
		t.Fatalf("history events len = %d, want 1", len(events))
	}
	event := events[0]
	if event.EventType != "success" {
		t.Fatalf("history event type = %q, want %q", event.EventType, "success")
	}
	if event.Provider != "anthropic" {
		t.Fatalf("history provider = %q, want %q", event.Provider, "anthropic")
	}
	if event.TargetAuthFile != "claude-old.json" {
		t.Fatalf("history target_auth_file = %q, want %q", event.TargetAuthFile, "claude-old.json")
	}
	if !event.OverwroteExisting {
		t.Fatalf("expected overwrote_existing to be true")
	}
	if event.Before == nil || event.Before.Email != "old@example.com" {
		t.Fatalf("history before email = %#v, want old@example.com", event.Before)
	}
	if event.After == nil || event.After.Email != "new@example.com" {
		t.Fatalf("history after email = %#v, want new@example.com", event.After)
	}
	if event.After.Plan != "pro" {
		t.Fatalf("history after plan = %q, want %q", event.After.Plan, "pro")
	}
	if event.After.AccountIDHash == "" {
		t.Fatalf("expected history after account_id_hash to be populated")
	}
	historyBytes, err := os.ReadFile(oauthReauthHistoryPath(authDir))
	if err != nil {
		t.Fatalf("read oauth reauth history: %v", err)
	}
	historyText := string(historyBytes)
	if strings.Contains(historyText, "new-token") || strings.Contains(historyText, "new-refresh-token") {
		t.Fatalf("history file leaked raw token data: %s", historyText)
	}
}

func TestSaveOAuthTokenRecordWritesFailureHistoryWhenPersistFails(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	authDir := t.TempDir()
	store := &fileBackedTestAuthStore{baseDir: authDir}
	manager := coreauth.NewManager(store, nil, nil)
	targetAuth := &coreauth.Auth{
		ID:       "claude-old.json",
		FileName: "claude-old.json",
		Provider: "claude",
		Attributes: map[string]string{
			"path": filepath.Join(authDir, "claude-old.json"),
		},
		Metadata: map[string]any{
			"type":  "claude",
			"email": "old@example.com",
		},
	}
	if _, err := manager.Register(context.Background(), targetAuth); err != nil {
		t.Fatalf("register target auth: %v", err)
	}

	store.failSave = errors.New("disk full")

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	h.tokenStore = store

	RegisterOAuthSessionWithTarget("state-reauth-fail", "anthropic", "claude-old.json")
	t.Cleanup(func() {
		CompleteOAuthSession("state-reauth-fail")
	})

	record := &coreauth.Auth{
		ID:       "claude-new.json",
		FileName: "claude-new.json",
		Provider: "claude",
		Metadata: map[string]any{
			"type":         "claude",
			"email":        "new@example.com",
			"access_token": "new-token",
		},
	}

	if _, err := h.saveOAuthTokenRecord(context.Background(), "state-reauth-fail", record); err == nil {
		t.Fatal("expected saveOAuthTokenRecord to return error")
	}

	events := readOAuthReauthHistoryEvents(t, authDir)
	if len(events) != 1 {
		t.Fatalf("history events len = %d, want 1", len(events))
	}
	event := events[0]
	if event.EventType != "failure" {
		t.Fatalf("history event type = %q, want %q", event.EventType, "failure")
	}
	if event.Error == "" || !strings.Contains(event.Error, "disk full") {
		t.Fatalf("history error = %q, want disk full", event.Error)
	}
	if event.Before == nil || event.Before.Email != "old@example.com" {
		t.Fatalf("history before email = %#v, want old@example.com", event.Before)
	}
	if event.After != nil {
		t.Fatalf("history after = %#v, want nil", event.After)
	}
}

func TestSaveOAuthTokenRecordRejectsCancelledDeviceCodeSession(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	authDir := t.TempDir()
	store := &fileBackedTestAuthStore{baseDir: authDir}
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, coreauth.NewManager(nil, nil, nil))
	h.tokenStore = store

	state := "qwen-cancelled-save"
	RegisterOAuthSession(state, "qwen")
	t.Cleanup(func() {
		CompleteOAuthSession(state)
	})
	if !CancelOAuthSessionState(state) {
		t.Fatal("expected cancel to succeed")
	}

	record := &coreauth.Auth{
		ID:       "qwen-new.json",
		FileName: "qwen-new.json",
		Provider: "qwen",
		Metadata: map[string]any{
			"email": "cancelled@example.com",
		},
	}

	if _, err := h.saveOAuthTokenRecord(context.Background(), state, record); !errors.Is(err, errOAuthSessionCancelled) {
		t.Fatalf("saveOAuthTokenRecord error = %v, want %v", err, errOAuthSessionCancelled)
	}

	store.mu.Lock()
	itemCount := len(store.items)
	store.mu.Unlock()
	if itemCount != 0 {
		t.Fatalf("saved auth count = %d, want 0", itemCount)
	}

	entries, err := os.ReadDir(authDir)
	if err != nil {
		t.Fatalf("read auth dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("auth dir entries = %d, want 0", len(entries))
	}
}

func TestResolvedGeminiProjectIDUsesExistingProjectForReauth(t *testing.T) {
	targetAuth := &coreauth.Auth{
		Provider: "gemini-cli",
		Metadata: map[string]any{
			"project_id": "demo-project",
		},
	}
	if got := resolvedGeminiProjectID("", targetAuth); got != "demo-project" {
		t.Fatalf("resolvedGeminiProjectID() = %q, want %q", got, "demo-project")
	}
}

func TestResolvedGeminiProjectIDMapsMultiProjectReauthToAll(t *testing.T) {
	targetAuth := &coreauth.Auth{
		Provider: "gemini-cli",
		Metadata: map[string]any{
			"project_id": "proj-a,proj-b",
		},
	}
	if got := resolvedGeminiProjectID("", targetAuth); got != "ALL" {
		t.Fatalf("resolvedGeminiProjectID() = %q, want %q", got, "ALL")
	}
}

func TestListOAuthReauthHistoryFiltersAndLimitsResults(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	authDir := t.TempDir()
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, coreauth.NewManager(nil, nil, nil))

	h.appendOAuthReauthHistoryEvent(oauthReauthHistoryEvent{
		EventType:      "success",
		OccurredAt:     time.Date(2026, 4, 17, 1, 0, 0, 0, time.UTC),
		Provider:       "codex",
		TargetAuthFile: "alpha.json",
		After: &oauthReauthFileSummary{
			Email: "alpha@example.com",
		},
	})
	h.appendOAuthReauthHistoryEvent(oauthReauthHistoryEvent{
		EventType:      "failure",
		OccurredAt:     time.Date(2026, 4, 17, 2, 0, 0, 0, time.UTC),
		Provider:       "codex",
		TargetAuthFile: "beta.json",
		Error:          "disk full",
	})
	h.appendOAuthReauthHistoryEvent(oauthReauthHistoryEvent{
		EventType:      "success",
		OccurredAt:     time.Date(2026, 4, 17, 3, 0, 0, 0, time.UTC),
		Provider:       "codex",
		TargetAuthFile: "alpha.json",
		After: &oauthReauthFileSummary{
			Email: "alpha-new@example.com",
		},
	})

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/oauth-reauth-history?auth_name=alpha.json&limit=1", nil)

	h.ListOAuthReauthHistory(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, want %d", rec.Code, http.StatusOK)
	}

	var response oauthReauthHistoryListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.Limit != 1 {
		t.Fatalf("limit = %d, want 1", response.Limit)
	}
	if response.AuthName != "alpha.json" {
		t.Fatalf("auth_name = %q, want %q", response.AuthName, "alpha.json")
	}
	if len(response.Events) != 1 {
		t.Fatalf("events len = %d, want 1", len(response.Events))
	}
	event := response.Events[0]
	if event.TargetAuthFile != "alpha.json" {
		t.Fatalf("target_auth_file = %q, want %q", event.TargetAuthFile, "alpha.json")
	}
	if event.OccurredAt.Format(time.RFC3339) != "2026-04-17T03:00:00Z" {
		t.Fatalf("occurred_at = %s, want 2026-04-17T03:00:00Z", event.OccurredAt.Format(time.RFC3339))
	}
	if event.After == nil || event.After.Email != "alpha-new@example.com" {
		t.Fatalf("after = %#v, want alpha-new@example.com", event.After)
	}
}

type fileBackedTestAuthStore struct {
	mu       sync.Mutex
	items    map[string]*coreauth.Auth
	baseDir  string
	failSave error
}

func (s *fileBackedTestAuthStore) List(_ context.Context) ([]*coreauth.Auth, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]*coreauth.Auth, 0, len(s.items))
	for _, item := range s.items {
		out = append(out, item.Clone())
	}
	return out, nil
}

func (s *fileBackedTestAuthStore) Save(_ context.Context, auth *coreauth.Auth) (string, error) {
	if auth == nil {
		return "", nil
	}
	if s.failSave != nil {
		return "", s.failSave
	}

	path := strings.TrimSpace(authAttribute(auth, "path"))
	if path == "" {
		name := strings.TrimSpace(auth.FileName)
		if name == "" {
			name = strings.TrimSpace(auth.ID)
		}
		if !filepath.IsAbs(name) && strings.TrimSpace(s.baseDir) != "" {
			path = filepath.Join(s.baseDir, name)
		} else {
			path = name
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	raw, err := json.Marshal(auth.Metadata)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return "", err
	}

	copyAuth := auth.Clone()
	if copyAuth.Attributes == nil {
		copyAuth.Attributes = make(map[string]string)
	}
	copyAuth.Attributes["path"] = path
	if strings.TrimSpace(copyAuth.FileName) == "" {
		copyAuth.FileName = copyAuth.ID
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.items == nil {
		s.items = make(map[string]*coreauth.Auth)
	}
	s.items[copyAuth.ID] = copyAuth
	return path, nil
}

func (s *fileBackedTestAuthStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.items, id)
	return nil
}

func (s *fileBackedTestAuthStore) SetBaseDir(dir string) {
	s.baseDir = dir
}

func readOAuthReauthHistoryEvents(t *testing.T, authDir string) []oauthReauthHistoryEvent {
	t.Helper()

	path := oauthReauthHistoryPath(authDir)
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open oauth reauth history: %v", err)
	}
	defer f.Close()

	var events []oauthReauthHistoryEvent
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var event oauthReauthHistoryEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("unmarshal oauth reauth history line: %v", err)
		}
		events = append(events, event)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan oauth reauth history: %v", err)
	}
	return events
}
