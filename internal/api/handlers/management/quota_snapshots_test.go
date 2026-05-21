package management

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type quotaSnapshotTestExecutor struct {
	provider        string
	body            string
	bodyBytes       []byte
	contentEncoding string
	responses       map[string]quotaSnapshotTestResponse

	mu          sync.Mutex
	calls       int
	callsByAuth map[string]int
}

type quotaSnapshotTestResponse struct {
	statusCode      int
	body            string
	bodyBytes       []byte
	contentEncoding string
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
	if e.callsByAuth == nil {
		e.callsByAuth = make(map[string]int)
	}
	authID := ""
	if auth != nil {
		authID = auth.ID
	}
	e.callsByAuth[authID]++
	e.mu.Unlock()

	response := quotaSnapshotTestResponse{
		statusCode:      http.StatusOK,
		body:            `{"rate_limit":{"used_percent":25},"plan_type":"plus"}`,
		contentEncoding: e.contentEncoding,
	}
	if e.body != "" {
		response.body = e.body
	}
	if e.bodyBytes != nil {
		response.bodyBytes = e.bodyBytes
	}
	if specific, ok := e.responses[req.URL.String()]; ok {
		response = specific
		if response.statusCode == 0 {
			response.statusCode = http.StatusOK
		}
	}
	bodyBytes := []byte(response.body)
	if response.bodyBytes != nil {
		bodyBytes = response.bodyBytes
	}
	header := http.Header{"Content-Type": []string{"application/json"}}
	if response.contentEncoding != "" {
		header.Set("Content-Encoding", response.contentEncoding)
	}
	return &http.Response{
		StatusCode: response.statusCode,
		Header:     header,
		Body:       io.NopCloser(bytes.NewReader(bodyBytes)),
		Request:    req.WithContext(ctx),
	}, nil
}

func (e *quotaSnapshotTestExecutor) Calls() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls
}

func (e *quotaSnapshotTestExecutor) CallsForAuth(authID string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.callsByAuth[authID]
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

func TestQuotaSnapshotsClaudeUnauthorizedRequiresReauth(t *testing.T) {
	t.Parallel()

	const (
		profileURL = "https://api.anthropic.com/api/oauth/profile"
		usageURL   = "https://api.anthropic.com/api/oauth/usage"
	)

	tests := []struct {
		name      string
		responses map[string]quotaSnapshotTestResponse
	}{
		{
			name: "profile 401",
			responses: map[string]quotaSnapshotTestResponse{
				profileURL: {statusCode: http.StatusUnauthorized, body: `{"error":"provider body 401 invalid token"}`},
			},
		},
		{
			name: "usage 403",
			responses: map[string]quotaSnapshotTestResponse{
				profileURL: {body: `{"plan_type":"pro"}`},
				usageURL:   {statusCode: http.StatusForbidden, body: `{"error":"provider body 403 forbidden"}`},
			},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			manager := coreauth.NewManager(nil, nil, nil)
			exec := &quotaSnapshotTestExecutor{provider: "claude", responses: tt.responses}
			manager.RegisterExecutor(exec)
			if _, err := manager.Register(context.Background(), &coreauth.Auth{
				ID:       "claude-oauth",
				Provider: "claude",
			}); err != nil {
				t.Fatalf("Register() error = %v", err)
			}
			handler := NewHandlerWithoutConfigFilePath(nil, manager)
			router := gin.New()
			router.POST("/v0/management/quota/refresh", handler.RefreshQuotaSnapshots)

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v0/management/quota/refresh", strings.NewReader(`{"auth_id":"claude-oauth"}`))
			req.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("refresh status = %d, want 200 body=%s", rec.Code, rec.Body.String())
			}

			entry := quotaSnapshotEntryForAuth(t, decodeQuotaSnapshotPayload(t, rec), "claude-oauth")
			if entry.Status != quotaRefreshStatusReauthRequired {
				t.Fatalf("entry status = %q, want %q", entry.Status, quotaRefreshStatusReauthRequired)
			}
			if entry.Error != claudeQuotaCredentialUnauthorizedMessage {
				t.Fatalf("entry error = %q, want sanitized reauth message", entry.Error)
			}
			for _, forbidden := range []string{"401", "403", "provider body", "invalid token", "forbidden"} {
				if strings.Contains(rec.Body.String(), forbidden) || strings.Contains(entry.Error, forbidden) {
					t.Fatalf("reauth response leaked %q: entry=%#v body=%s", forbidden, entry, rec.Body.String())
				}
			}

			updated, ok := manager.GetByID("claude-oauth")
			if !ok {
				t.Fatal("updated auth missing")
			}
			if updated.Disabled {
				t.Fatal("reauth-required quota refresh must not disable auth")
			}
			if got := metadataString(updated.Metadata, quotaRefreshStatusMetadataKey); got != quotaRefreshStatusReauthRequired {
				t.Fatalf("persisted quota status = %q, want %q", got, quotaRefreshStatusReauthRequired)
			}
			if got := metadataString(updated.Metadata, quotaRefreshErrorMetadataKey); got != claudeQuotaCredentialUnauthorizedMessage {
				t.Fatalf("persisted quota error = %q, want sanitized reauth message", got)
			}
		})
	}
}

func TestQuotaSnapshotsCodexUnauthorizedRequiresReauth(t *testing.T) {
	t.Parallel()

	const (
		usageURL = "https://chatgpt.com/backend-api/wham/usage"
		wantErr  = "Codex credential unauthorized; reauthenticate this credential to refresh quota."
	)

	tests := []struct {
		name       string
		statusCode int
		body       string
	}{
		{
			name:       "usage 401",
			statusCode: http.StatusUnauthorized,
			body:       `{"error":{"type":"authentication_error","message":"Invalid authentication credentials"},"provider_body":"codex provider body marker 401"}`,
		},
		{
			name:       "usage 403",
			statusCode: http.StatusForbidden,
			body:       `{"error":{"type":"authentication_error","message":"Invalid authentication credentials"},"provider_body":"codex provider body marker 403"}`,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			manager := coreauth.NewManager(nil, nil, nil)
			exec := &quotaSnapshotTestExecutor{
				provider: "codex",
				responses: map[string]quotaSnapshotTestResponse{
					usageURL: {statusCode: tt.statusCode, body: tt.body},
				},
			}
			manager.RegisterExecutor(exec)
			if _, err := manager.Register(context.Background(), &coreauth.Auth{
				ID:       "codex-oauth",
				Provider: "codex",
			}); err != nil {
				t.Fatalf("Register() error = %v", err)
			}
			handler := NewHandlerWithoutConfigFilePath(nil, manager)
			router := gin.New()
			router.POST("/v0/management/quota/refresh", handler.RefreshQuotaSnapshots)

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v0/management/quota/refresh", strings.NewReader(`{"auth_id":"codex-oauth"}`))
			req.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("refresh status = %d, want 200 body=%s", rec.Code, rec.Body.String())
			}
			if got := exec.CallsForAuth("codex-oauth"); got != 1 {
				t.Fatalf("codex quota calls = %d, want 1", got)
			}

			entry := quotaSnapshotEntryForAuth(t, decodeQuotaSnapshotPayload(t, rec), "codex-oauth")
			if entry.Status != quotaRefreshStatusReauthRequired {
				t.Fatalf("entry status = %q, want %q", entry.Status, quotaRefreshStatusReauthRequired)
			}
			if entry.Error != wantErr {
				t.Fatalf("entry error = %q, want sanitized reauth message", entry.Error)
			}
			for _, forbidden := range []string{"401", "403", "authentication_error", "Invalid authentication credentials", "provider body", "provider_body"} {
				if strings.Contains(entry.Error, forbidden) {
					t.Fatalf("entry error leaked %q: %q", forbidden, entry.Error)
				}
			}

			updated, ok := manager.GetByID("codex-oauth")
			if !ok {
				t.Fatal("updated auth missing")
			}
			if updated.Disabled {
				t.Fatal("reauth-required quota refresh must not disable auth")
			}
			if got := metadataString(updated.Metadata, quotaRefreshStatusMetadataKey); got != quotaRefreshStatusReauthRequired {
				t.Fatalf("persisted quota status = %q, want %q", got, quotaRefreshStatusReauthRequired)
			}
			gotErr := metadataString(updated.Metadata, quotaRefreshErrorMetadataKey)
			if gotErr != wantErr {
				t.Fatalf("persisted quota error = %q, want sanitized reauth message", gotErr)
			}
			for _, forbidden := range []string{"401", "403", "authentication_error", "Invalid authentication credentials", "provider body", "provider_body"} {
				if strings.Contains(gotErr, forbidden) {
					t.Fatalf("persisted quota error leaked %q: %q", forbidden, gotErr)
				}
			}
		})
	}
}

func TestQuotaSnapshotsUnauthorizedDoesNotDecodeErrorBodyAndPreservesLastKnownPlan(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)
	manager := coreauth.NewManager(nil, nil, nil)
	exec := &quotaSnapshotTestExecutor{
		provider: "codex",
		responses: map[string]quotaSnapshotTestResponse{
			"https://chatgpt.com/backend-api/wham/usage": {
				statusCode:      http.StatusUnauthorized,
				bodyBytes:       []byte("not a gzip body"),
				contentEncoding: "gzip",
			},
		},
	}
	manager.RegisterExecutor(exec)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "codex-stale-pro",
		Provider: "codex",
		Metadata: map[string]any{
			quotaSnapshotPlanTypeKey: "pro",
			quotaSnapshotMetadataKey: map[string]any{"usage": map[string]any{"plan_type": "pro"}},
		},
	}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	handler := NewHandlerWithoutConfigFilePath(nil, manager)
	router := gin.New()
	router.POST("/v0/management/quota/refresh", handler.RefreshQuotaSnapshots)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v0/management/quota/refresh", strings.NewReader(`{"auth_id":"codex-stale-pro"}`))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh status = %d, want 200 body=%s", rec.Code, rec.Body.String())
	}

	entry := quotaSnapshotEntryForAuth(t, decodeQuotaSnapshotPayload(t, rec), "codex-stale-pro")
	if entry.Status != quotaRefreshStatusReauthRequired {
		t.Fatalf("entry status = %q, want %q", entry.Status, quotaRefreshStatusReauthRequired)
	}
	if entry.PlanType != "pro" {
		t.Fatalf("entry plan_type = %q, want last known pro after quota reauth", entry.PlanType)
	}
	updated, ok := manager.GetByID("codex-stale-pro")
	if !ok {
		t.Fatal("updated auth missing")
	}
	if got := metadataString(updated.Metadata, quotaSnapshotPlanTypeKey); got != "pro" {
		t.Fatalf("persisted plan_type = %q, want last known pro after quota reauth", got)
	}
	// Keep the last successful quota snapshot as stale observability data; the
	// reauth status is recorded separately and must not erase routing capability.
	if _, ok := updated.Metadata[quotaSnapshotMetadataKey]; !ok {
		t.Fatal("quota_snapshot should keep last known data after quota reauth")
	}
	if strings.Contains(rec.Body.String(), "gzip") || strings.Contains(rec.Body.String(), "not a gzip body") {
		t.Fatalf("response leaked error body/decode details: %s", rec.Body.String())
	}
}

func TestQuotaSnapshotsImplicitRefreshSkipsReauthAndRefreshDisabled(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)
	manager := coreauth.NewManager(nil, nil, nil)
	exec := &quotaSnapshotTestExecutor{provider: "claude"}
	manager.RegisterExecutor(exec)
	past := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	for _, auth := range []*coreauth.Auth{
		{
			ID:       "claude-reauth",
			Provider: "claude",
			Metadata: map[string]any{
				quotaRefreshStatusMetadataKey: quotaRefreshStatusReauthRequired,
				quotaRefreshErrorMetadataKey:  claudeQuotaCredentialUnauthorizedMessage,
				quotaNextRefreshMetadataKey:   past,
			},
		},
		{
			ID:       "claude-refresh-disabled",
			Provider: "claude",
			Metadata: map[string]any{
				"refresh_disabled":          true,
				quotaNextRefreshMetadataKey: past,
			},
		},
		{
			ID:       "claude-active",
			Provider: "claude",
			Metadata: map[string]any{
				quotaNextRefreshMetadataKey: past,
			},
		},
	} {
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("Register(%s) error = %v", auth.ID, err)
		}
	}
	handler := NewHandlerWithoutConfigFilePath(nil, manager)
	router := gin.New()
	router.POST("/v0/management/quota/refresh", handler.RefreshQuotaSnapshots)

	handler.refreshDueQuotaSnapshots(context.Background(), defaultQuotaSnapshotRefreshInterval)

	fullRec := httptest.NewRecorder()
	fullReq := httptest.NewRequest(http.MethodPost, "/v0/management/quota/refresh", strings.NewReader(`{}`))
	fullReq.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(fullRec, fullReq)
	if fullRec.Code != http.StatusOK {
		t.Fatalf("full refresh status = %d, want 200 body=%s", fullRec.Code, fullRec.Body.String())
	}

	providerRec := httptest.NewRecorder()
	providerReq := httptest.NewRequest(http.MethodPost, "/v0/management/quota/refresh", strings.NewReader(`{"provider":"claude"}`))
	providerReq.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(providerRec, providerReq)
	if providerRec.Code != http.StatusOK {
		t.Fatalf("provider refresh status = %d, want 200 body=%s", providerRec.Code, providerRec.Body.String())
	}

	if got := exec.CallsForAuth("claude-reauth"); got != 0 {
		t.Fatalf("reauth auth calls = %d, want 0", got)
	}
	if got := exec.CallsForAuth("claude-refresh-disabled"); got != 0 {
		t.Fatalf("refresh-disabled auth calls = %d, want 0", got)
	}
	if got := exec.CallsForAuth("claude-active"); got != 6 {
		t.Fatalf("active auth calls = %d, want 6", got)
	}
}

func TestQuotaSnapshotEntryMarksRefreshDisabled(t *testing.T) {
	t.Parallel()

	entry := quotaSnapshotEntryFromAuth(&coreauth.Auth{
		ID:       "claude-access-token-only",
		Provider: "claude",
		Metadata: map[string]any{
			"refresh_disabled": true,
		},
	})
	if entry.Status != quotaRefreshStatusRefreshDisabled {
		t.Fatalf("entry status = %q, want %q", entry.Status, quotaRefreshStatusRefreshDisabled)
	}
	if entry.Error != "" {
		t.Fatalf("entry error = %q, want empty", entry.Error)
	}
}

func TestQuotaSnapshotsBulkRefreshReturnsEntriesWhenAllImplicitTargetsSkipped(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)
	manager := coreauth.NewManager(nil, nil, nil)
	exec := &quotaSnapshotTestExecutor{provider: "claude"}
	manager.RegisterExecutor(exec)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "claude-reauth",
		Provider: "claude",
		Metadata: map[string]any{
			quotaRefreshStatusMetadataKey: quotaRefreshStatusReauthRequired,
			quotaRefreshErrorMetadataKey:  claudeQuotaCredentialUnauthorizedMessage,
		},
	}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	handler := NewHandlerWithoutConfigFilePath(nil, manager)
	router := gin.New()
	router.POST("/v0/management/quota/refresh", handler.RefreshQuotaSnapshots)

	for _, body := range []string{`{}`, `{"provider":"claude"}`} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v0/management/quota/refresh", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("bulk refresh %s status = %d, want 200 body=%s", body, rec.Code, rec.Body.String())
		}
		entry := quotaSnapshotEntryForAuth(t, decodeQuotaSnapshotPayload(t, rec), "claude-reauth")
		if entry.Status != quotaRefreshStatusReauthRequired {
			t.Fatalf("entry status = %q, want %q", entry.Status, quotaRefreshStatusReauthRequired)
		}
	}
	if got := exec.CallsForAuth("claude-reauth"); got != 0 {
		t.Fatalf("reauth auth calls = %d, want 0", got)
	}
}

func TestQuotaSnapshotsLegacyUnauthorizedErrorMapsToReauthAndRetriesExplicitly(t *testing.T) {
	t.Parallel()

	const legacyError = `quota endpoint returned 401: {"type":"error","error":{"type":"authentication_error","message":"Invalid authentication credentials"},"provider_body":"legacy raw auth failure"}`

	gin.SetMode(gin.TestMode)
	manager := coreauth.NewManager(nil, nil, nil)
	exec := &quotaSnapshotTestExecutor{provider: "claude"}
	manager.RegisterExecutor(exec)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "claude-legacy",
		Provider: "claude",
		FileName: "claude-legacy.json",
		Metadata: map[string]any{
			quotaRefreshStatusMetadataKey: quotaRefreshStatusError,
			quotaRefreshErrorMetadataKey:  legacyError,
			quotaNextRefreshMetadataKey:   time.Now().UTC().Add(-time.Minute).Format(time.RFC3339),
		},
	}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	handler := NewHandlerWithoutConfigFilePath(nil, manager)
	router := gin.New()
	router.GET("/v0/management/quota/snapshots", handler.GetQuotaSnapshots)
	router.POST("/v0/management/quota/refresh", handler.RefreshQuotaSnapshots)

	snapshotRec := httptest.NewRecorder()
	router.ServeHTTP(snapshotRec, httptest.NewRequest(http.MethodGet, "/v0/management/quota/snapshots", nil))
	if snapshotRec.Code != http.StatusOK {
		t.Fatalf("snapshots status = %d, want 200 body=%s", snapshotRec.Code, snapshotRec.Body.String())
	}
	legacyEntry := quotaSnapshotEntryForAuth(t, decodeQuotaSnapshotPayload(t, snapshotRec), "claude-legacy")
	if legacyEntry.Status != quotaRefreshStatusReauthRequired {
		t.Fatalf("legacy entry status = %q, want %q", legacyEntry.Status, quotaRefreshStatusReauthRequired)
	}
	if legacyEntry.Error != claudeQuotaCredentialUnauthorizedMessage {
		t.Fatalf("legacy entry error = %q, want sanitized reauth message", legacyEntry.Error)
	}
	for _, forbidden := range []string{"401", "authentication_error", "Invalid authentication credentials", "provider_body", "legacy raw auth failure"} {
		if strings.Contains(snapshotRec.Body.String(), forbidden) || strings.Contains(legacyEntry.Error, forbidden) {
			t.Fatalf("legacy snapshot leaked %q: entry=%#v body=%s", forbidden, legacyEntry, snapshotRec.Body.String())
		}
	}

	handler.refreshDueQuotaSnapshots(context.Background(), defaultQuotaSnapshotRefreshInterval)
	fullRec := httptest.NewRecorder()
	fullReq := httptest.NewRequest(http.MethodPost, "/v0/management/quota/refresh", strings.NewReader(`{}`))
	fullReq.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(fullRec, fullReq)
	if fullRec.Code != http.StatusOK {
		t.Fatalf("full refresh status = %d, want 200 body=%s", fullRec.Code, fullRec.Body.String())
	}
	if got := exec.CallsForAuth("claude-legacy"); got != 0 {
		t.Fatalf("legacy reauth auth implicit calls = %d, want 0", got)
	}
	fullEntry := quotaSnapshotEntryForAuth(t, decodeQuotaSnapshotPayload(t, fullRec), "claude-legacy")
	if fullEntry.Status != quotaRefreshStatusReauthRequired {
		t.Fatalf("full refresh legacy entry status = %q, want %q", fullEntry.Status, quotaRefreshStatusReauthRequired)
	}

	explicitRec := httptest.NewRecorder()
	explicitReq := httptest.NewRequest(http.MethodPost, "/v0/management/quota/refresh", strings.NewReader(`{"auth_id":"claude-legacy"}`))
	explicitReq.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(explicitRec, explicitReq)
	if explicitRec.Code != http.StatusOK {
		t.Fatalf("explicit refresh status = %d, want 200 body=%s", explicitRec.Code, explicitRec.Body.String())
	}
	if got := exec.CallsForAuth("claude-legacy"); got != 2 {
		t.Fatalf("explicit legacy refresh calls = %d, want 2", got)
	}
	entry := quotaSnapshotEntryForAuth(t, decodeQuotaSnapshotPayload(t, explicitRec), "claude-legacy")
	if entry.Status != quotaRefreshStatusOK {
		t.Fatalf("explicit legacy entry status = %q, want %q", entry.Status, quotaRefreshStatusOK)
	}
	if entry.Error != "" {
		t.Fatalf("explicit legacy entry error = %q, want empty", entry.Error)
	}
	updated, ok := manager.GetByID("claude-legacy")
	if !ok {
		t.Fatal("updated auth missing")
	}
	if updated.Disabled {
		t.Fatal("successful explicit legacy retry must not disable auth")
	}
	if got := metadataString(updated.Metadata, quotaRefreshStatusMetadataKey); got != quotaRefreshStatusOK {
		t.Fatalf("persisted quota status = %q, want %q", got, quotaRefreshStatusOK)
	}
	if got := metadataString(updated.Metadata, quotaRefreshErrorMetadataKey); got != "" {
		t.Fatalf("persisted quota error = %q, want empty", got)
	}
}

func TestQuotaSnapshotsExplicitReauthRefreshRetriesAndClearsState(t *testing.T) {
	t.Parallel()

	const (
		profileURL = "https://api.anthropic.com/api/oauth/profile"
		usageURL   = "https://api.anthropic.com/api/oauth/usage"
	)

	tests := []struct {
		name string
		body string
	}{
		{name: "auth_id", body: `{"auth_id":"claude-reauth"}`},
		{name: "name", body: `{"name":"claude-main.json"}`},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			manager := coreauth.NewManager(nil, nil, nil)
			exec := &quotaSnapshotTestExecutor{
				provider: "claude",
				responses: map[string]quotaSnapshotTestResponse{
					profileURL: {body: `{"plan_type":"pro"}`},
					usageURL:   {body: `{"extra_usage":{"is_enabled":true}}`},
				},
			}
			manager.RegisterExecutor(exec)
			if _, err := manager.Register(context.Background(), &coreauth.Auth{
				ID:       "claude-reauth",
				Provider: "claude",
				FileName: "claude-main.json",
				Metadata: map[string]any{
					quotaRefreshStatusMetadataKey: quotaRefreshStatusReauthRequired,
					quotaRefreshErrorMetadataKey:  claudeQuotaCredentialUnauthorizedMessage,
				},
			}); err != nil {
				t.Fatalf("Register() error = %v", err)
			}
			handler := NewHandlerWithoutConfigFilePath(nil, manager)
			router := gin.New()
			router.POST("/v0/management/quota/refresh", handler.RefreshQuotaSnapshots)

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v0/management/quota/refresh", strings.NewReader(tt.body))
			req.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("refresh status = %d, want 200 body=%s", rec.Code, rec.Body.String())
			}
			if got := exec.CallsForAuth("claude-reauth"); got != 2 {
				t.Fatalf("explicit refresh calls = %d, want 2", got)
			}

			entry := quotaSnapshotEntryForAuth(t, decodeQuotaSnapshotPayload(t, rec), "claude-reauth")
			if entry.Status != quotaRefreshStatusOK {
				t.Fatalf("entry status = %q, want %q", entry.Status, quotaRefreshStatusOK)
			}
			if entry.Error != "" {
				t.Fatalf("entry error = %q, want empty", entry.Error)
			}
			if entry.PlanType != "pro" {
				t.Fatalf("entry plan_type = %q, want pro", entry.PlanType)
			}

			updated, ok := manager.GetByID("claude-reauth")
			if !ok {
				t.Fatal("updated auth missing")
			}
			if updated.Disabled {
				t.Fatal("successful explicit retry must not disable auth")
			}
			if got := metadataString(updated.Metadata, quotaRefreshStatusMetadataKey); got != quotaRefreshStatusOK {
				t.Fatalf("persisted quota status = %q, want %q", got, quotaRefreshStatusOK)
			}
			if got := metadataString(updated.Metadata, quotaRefreshErrorMetadataKey); got != "" {
				t.Fatalf("persisted quota error = %q, want empty", got)
			}
			if _, ok := updated.Metadata[quotaSnapshotMetadataKey].(map[string]any); !ok {
				t.Fatalf("quota snapshot missing or wrong type: %#v", updated.Metadata[quotaSnapshotMetadataKey])
			}
			if got := metadataString(updated.Metadata, quotaSnapshotPlanTypeKey); got != "pro" {
				t.Fatalf("persisted plan_type = %q, want pro", got)
			}
		})
	}
}

func TestQuotaSnapshotsRefreshNormalizesANSIWrappedJSON(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)
	manager := coreauth.NewManager(nil, nil, nil)
	exec := &quotaSnapshotTestExecutor{
		provider: "codex",
		body:     "\x1b[32mhttps://chatgpt.com/backend-api/wham/usage\n{\"rate_limit\":{\"used_percent\":25},\"plan_type\":\"plus\"}\x1b[0m\n",
	}
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

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v0/management/quota/refresh", strings.NewReader(`{"auth_id":"codex-plus"}`))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh status = %d, want 200 body=%s", rec.Code, rec.Body.String())
	}

	updated, ok := manager.GetByID("codex-plus")
	if !ok {
		t.Fatal("updated auth missing")
	}
	if got := metadataString(updated.Metadata, quotaRefreshStatusMetadataKey); got != "ok" {
		t.Fatalf("quota status = %q, want ok", got)
	}
	if got := metadataString(updated.Metadata, quotaRefreshErrorMetadataKey); got != "" {
		t.Fatalf("quota refresh error = %q, want empty", got)
	}
}

func TestQuotaSnapshotsRefreshDecodesBrotliJSON(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)
	manager := coreauth.NewManager(nil, nil, nil)
	exec := &quotaSnapshotTestExecutor{
		provider:        "codex",
		bodyBytes:       brotliEncodeQuotaTest(t, `{"rate_limit":{"used_percent":25},"plan_type":"plus"}`),
		contentEncoding: "br",
	}
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

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v0/management/quota/refresh", strings.NewReader(`{"auth_id":"codex-plus"}`))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh status = %d, want 200 body=%s", rec.Code, rec.Body.String())
	}

	updated, ok := manager.GetByID("codex-plus")
	if !ok {
		t.Fatal("updated auth missing")
	}
	if got := metadataString(updated.Metadata, quotaRefreshStatusMetadataKey); got != "ok" {
		t.Fatalf("quota status = %q, want ok", got)
	}
}

func TestNormalizeQuotaJSONPayloadStripsBOM(t *testing.T) {
	t.Parallel()

	var payload map[string]any
	if err := json.Unmarshal(normalizeQuotaJSONPayload([]byte("\xef\xbb\xbf{\"ok\":true}")), &payload); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if payload["ok"] != true {
		t.Fatalf("payload = %#v, want ok=true", payload)
	}
}

func TestInferCodexPlanTypeUsesCodexNormalizer(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		authPlan  string
		authAttrs map[string]string
		usage     map[string]any
		want      string
	}{
		{
			name:     "metadata chatgpt plus slug",
			authPlan: "chatgpt-plus",
			want:     "plus",
		},
		{
			name:  "usage chatgpt plus label",
			usage: map[string]any{"planType": "ChatGPT Plus"},
			want:  "plus",
		},
		{
			name:  "usage plan prefix",
			usage: map[string]any{"chatgpt_plan_type": "plan_plus"},
			want:  "plus",
		},
		{
			name:  "usage pro",
			usage: map[string]any{"plan_type": "pro"},
			want:  "pro",
		},
		{
			name:     "usage plus overrides stale metadata pro",
			authPlan: "pro",
			usage:    map[string]any{"plan_type": "plus"},
			want:     "plus",
		},
		{
			name:     "usage pro overrides stale metadata plus",
			authPlan: "plus",
			usage:    map[string]any{"plan_type": "pro"},
			want:     "pro",
		},
		{
			name:      "attributes fallback",
			authAttrs: map[string]string{"chatgptPlanType": "ChatGPT Plus"},
			want:      "plus",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			auth := &coreauth.Auth{}
			if tt.authPlan != "" {
				auth.Metadata = map[string]any{"plan_type": tt.authPlan}
			}
			if tt.authAttrs != nil {
				auth.Attributes = tt.authAttrs
			}
			if got := inferCodexPlanType(auth, tt.usage); got != tt.want {
				t.Fatalf("inferCodexPlanType() = %q, want %q", got, tt.want)
			}
		})
	}
}

func brotliEncodeQuotaTest(t *testing.T, text string) []byte {
	t.Helper()
	var buf bytes.Buffer
	writer := brotli.NewWriter(&buf)
	if _, err := writer.Write([]byte(text)); err != nil {
		t.Fatalf("brotli write: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("brotli close: %v", err)
	}
	return buf.Bytes()
}

func decodeQuotaSnapshotPayload(t *testing.T, rec *httptest.ResponseRecorder) quotaSnapshotPayload {
	t.Helper()
	var payload quotaSnapshotPayload
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode quota payload: %v body=%s", err, rec.Body.String())
	}
	return payload
}

func quotaSnapshotEntryForAuth(t *testing.T, payload quotaSnapshotPayload, authID string) quotaSnapshotEntry {
	t.Helper()
	for _, entry := range payload.Entries {
		if entry.AuthID == authID {
			return entry
		}
	}
	t.Fatalf("quota entry for auth %q not found in %#v", authID, payload.Entries)
	return quotaSnapshotEntry{}
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

func TestQuotaSnapshotMissingExecutorDoesNotPersistUnsupportedForSupportedProvider(t *testing.T) {
	t.Parallel()

	manager := coreauth.NewManager(nil, nil, nil)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "codex-plus",
		Provider: "codex",
		Metadata: map[string]any{
			quotaNextRefreshMetadataKey: time.Now().UTC().Add(-time.Minute).Format(time.RFC3339),
		},
	}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	handler := NewHandlerWithoutConfigFilePath(nil, manager)

	handler.refreshDueQuotaSnapshots(context.Background(), defaultQuotaSnapshotRefreshInterval)

	updated, ok := manager.GetByID("codex-plus")
	if !ok {
		t.Fatal("updated auth missing")
	}
	if got := metadataString(updated.Metadata, quotaRefreshStatusMetadataKey); got == quotaRefreshStatusUnsupported {
		t.Fatalf("missing executor must not persist unsupported status for supported provider")
	}
	if got := metadataString(updated.Metadata, quotaRefreshErrorMetadataKey); got == quotaUnsupportedProviderMessage {
		t.Fatalf("missing executor must not persist unsupported provider error")
	}
	next, ok := metadataTime(updated.Metadata, quotaNextRefreshMetadataKey)
	if !ok {
		t.Fatal("next refresh timestamp missing")
	}
	if !next.After(time.Now().UTC()) {
		t.Fatalf("next refresh = %s, want short future retry", next.Format(time.RFC3339))
	}
}

func TestQuotaSnapshotLegacyUnsupportedProviderErrorIsStaleAndRetried(t *testing.T) {
	t.Parallel()

	manager := coreauth.NewManager(nil, nil, nil)
	exec := &quotaSnapshotTestExecutor{provider: "codex"}
	manager.RegisterExecutor(exec)
	future := time.Now().UTC().Add(defaultQuotaSnapshotRefreshInterval).Format(time.RFC3339)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "codex-plus",
		Provider: "codex",
		Metadata: map[string]any{
			quotaRefreshStatusMetadataKey: quotaRefreshStatusUnsupported,
			quotaRefreshErrorMetadataKey:  quotaUnsupportedProviderMessage,
			quotaNextRefreshMetadataKey:   future,
		},
	}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	handler := NewHandlerWithoutConfigFilePath(nil, manager)

	entry := quotaSnapshotEntryFromAuth(&coreauth.Auth{
		ID:       "codex-plus",
		Provider: "codex",
		Metadata: map[string]any{
			quotaRefreshStatusMetadataKey: quotaRefreshStatusUnsupported,
			quotaRefreshErrorMetadataKey:  quotaUnsupportedProviderMessage,
		},
	})
	if entry.Status != quotaRefreshStatusStale || entry.Error != "" {
		t.Fatalf("legacy unsupported entry status/error = %q/%q, want stale/empty", entry.Status, entry.Error)
	}

	handler.refreshDueQuotaSnapshots(context.Background(), defaultQuotaSnapshotRefreshInterval)
	if exec.Calls() == 0 {
		t.Fatal("legacy unsupported status should be retried even when next refresh is in the future")
	}

	updated, ok := manager.GetByID("codex-plus")
	if !ok {
		t.Fatal("updated auth missing")
	}
	if got := metadataString(updated.Metadata, quotaRefreshStatusMetadataKey); got != quotaRefreshStatusOK {
		t.Fatalf("status after retry = %q, want ok", got)
	}
}
