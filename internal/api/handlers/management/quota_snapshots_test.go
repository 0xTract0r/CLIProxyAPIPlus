package management

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type quotaSnapshotTestExecutor struct {
	provider string

	mu    sync.Mutex
	calls int
}

func (e *quotaSnapshotTestExecutor) Identifier() string { return e.provider }

func (e *quotaSnapshotTestExecutor) Execute(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *quotaSnapshotTestExecutor) ExecuteStream(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}

func (e *quotaSnapshotTestExecutor) Refresh(context.Context, *coreauth.Auth) (*coreauth.Auth, error) {
	return nil, nil
}

func (e *quotaSnapshotTestExecutor) CountTokens(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *quotaSnapshotTestExecutor) HttpRequest(ctx context.Context, auth *coreauth.Auth, req *http.Request) (*http.Response, error) {
	e.mu.Lock()
	e.calls++
	e.mu.Unlock()
	body := `{"rate_limit":{"used_percent":25},"plan_type":"plus"}`
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req.WithContext(ctx),
	}, nil
}

func (e *quotaSnapshotTestExecutor) Calls() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls
}

func TestQuotaSnapshotsRefreshPersistsCoreSnapshot(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)
	manager := coreauth.NewManager(nil, nil, nil)
	exec := &quotaSnapshotTestExecutor{provider: "codex"}
	manager.RegisterExecutor(exec)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "codex-plus",
		Provider: "codex",
		Metadata: map[string]any{"plan_type": "plus"},
	}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	handler := NewHandlerWithoutConfigFilePath(nil, manager)
	router := gin.New()
	router.POST("/v0/management/quota/refresh", handler.RefreshQuotaSnapshots)
	router.GET("/v0/management/quota/snapshots", handler.GetQuotaSnapshots)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v0/management/quota/refresh", strings.NewReader(`{"auth_id":"codex-plus"}`))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh status = %d, want 200 body=%s", rec.Code, rec.Body.String())
	}
	if exec.Calls() != 1 {
		t.Fatalf("HttpRequest calls = %d, want 1", exec.Calls())
	}

	updated, ok := manager.GetByID("codex-plus")
	if !ok {
		t.Fatal("updated auth missing")
	}
	if got := metadataString(updated.Metadata, quotaRefreshStatusMetadataKey); got != "ok" {
		t.Fatalf("quota status = %q, want ok", got)
	}
	if _, ok := updated.Metadata[quotaSnapshotMetadataKey].(map[string]any); !ok {
		t.Fatalf("quota snapshot missing or wrong type: %#v", updated.Metadata[quotaSnapshotMetadataKey])
	}
	if _, ok := metadataTime(updated.Metadata, quotaLastRefreshedMetadataKey); !ok {
		t.Fatal("last refreshed timestamp missing")
	}
	if _, ok := metadataTime(updated.Metadata, quotaNextRefreshMetadataKey); !ok {
		t.Fatal("next refresh timestamp missing")
	}

	getRec := httptest.NewRecorder()
	router.ServeHTTP(getRec, httptest.NewRequest(http.MethodGet, "/v0/management/quota/snapshots", nil))
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200 body=%s", getRec.Code, getRec.Body.String())
	}
	if exec.Calls() != 1 {
		t.Fatalf("GET should not call provider; calls = %d, want 1", exec.Calls())
	}
	if !strings.Contains(getRec.Body.String(), `"status":"ok"`) {
		t.Fatalf("GET body missing ok snapshot: %s", getRec.Body.String())
	}
}

func TestQuotaSnapshotAutoRefreshSchedulesMissingNextBeforeProviderCall(t *testing.T) {
	t.Parallel()

	manager := coreauth.NewManager(nil, nil, nil)
	exec := &quotaSnapshotTestExecutor{provider: "codex"}
	manager.RegisterExecutor(exec)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "codex-plus",
		Provider: "codex",
		Metadata: map[string]any{"plan_type": "plus"},
	}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	handler := NewHandlerWithoutConfigFilePath(nil, manager)

	handler.refreshDueQuotaSnapshots(context.Background(), defaultQuotaSnapshotRefreshInterval)
	if exec.Calls() != 0 {
		t.Fatalf("first auto tick should only schedule jitter; HttpRequest calls = %d, want 0", exec.Calls())
	}

	updated, ok := manager.GetByID("codex-plus")
	if !ok {
		t.Fatal("updated auth missing")
	}
	if _, ok := metadataTime(updated.Metadata, quotaNextRefreshMetadataKey); !ok {
		t.Fatal("next refresh timestamp missing")
	}
}
