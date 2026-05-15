package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

func TestListAuthStatusHistoryFiltersNewestEntries(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	authDir := t.TempDir()
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, nil)
	h.appendAuthStatusHistoryEvent(authStatusHistoryEvent{
		EventType:     "warning",
		AuthName:      "alpha.json",
		Provider:      "codex",
		Trigger:       authStatusHistoryTriggerManual,
		Status:        "error",
		StatusMessage: "unexpected EOF",
		OccurredAt:    time.Date(2026, 4, 19, 0, 1, 0, 0, time.UTC),
	})
	h.appendAuthStatusHistoryEvent(authStatusHistoryEvent{
		EventType:  "cleared",
		AuthName:   "alpha.json",
		Provider:   "codex",
		Trigger:    authStatusHistoryTriggerAuto,
		Status:     "active",
		OccurredAt: time.Date(2026, 4, 19, 0, 2, 0, 0, time.UTC),
	})
	h.appendAuthStatusHistoryEvent(authStatusHistoryEvent{
		EventType:  "warning",
		AuthName:   "beta.json",
		Provider:   "claude",
		Trigger:    authStatusHistoryTriggerManual,
		Status:     "error",
		OccurredAt: time.Date(2026, 4, 19, 0, 3, 0, 0, time.UTC),
	})

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/auth-status-history?auth_name=alpha.json&limit=1", nil)
	h.ListAuthStatusHistory(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}

	var payload authStatusHistoryListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if payload.Limit != 1 {
		t.Fatalf("limit = %d, want 1", payload.Limit)
	}
	if payload.AuthName != "alpha.json" {
		t.Fatalf("auth_name = %q, want %q", payload.AuthName, "alpha.json")
	}
	if len(payload.Events) != 1 {
		t.Fatalf("events = %d, want 1", len(payload.Events))
	}
	if payload.Events[0].EventType != "cleared" {
		t.Fatalf("event_type = %q, want %q", payload.Events[0].EventType, "cleared")
	}
	if payload.Events[0].Trigger != authStatusHistoryTriggerAuto {
		t.Fatalf("trigger = %q, want %q", payload.Events[0].Trigger, authStatusHistoryTriggerAuto)
	}
}
