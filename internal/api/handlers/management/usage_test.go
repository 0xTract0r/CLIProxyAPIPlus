package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/redisqueue"
	mgmtusage "github.com/router-for-me/CLIProxyAPI/v7/internal/usage"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

func TestGetUsageStatisticsSupportsLightweightOptions(t *testing.T) {
	gin.SetMode(gin.TestMode)
	stats := mgmtusage.NewRequestStatistics()
	oldTime := time.Date(2026, 3, 20, 10, 0, 0, 0, time.UTC)
	recentTime := time.Date(2026, 3, 20, 12, 0, 0, 0, time.UTC)
	for _, requestedAt := range []time.Time{oldTime, recentTime} {
		stats.Record(context.Background(), coreusage.Record{
			APIKey:      "test-key",
			Model:       "gpt-5.4",
			RequestedAt: requestedAt,
			Detail: coreusage.Detail{
				InputTokens:  10,
				OutputTokens: 20,
				TotalTokens:  30,
			},
		})
	}

	handler := &Handler{}
	handler.SetUsageStatistics(stats)

	rec := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(rec)
	ginCtx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/usage?include_details=false", nil)
	handler.GetUsageStatistics(ginCtx)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	payload := decodeUsageStatisticsPayload(t, rec)
	if payload.Usage.TotalRequests != 2 {
		t.Fatalf("total_requests = %d, want 2", payload.Usage.TotalRequests)
	}
	if details := payload.Usage.APIs["test-key"].Models["gpt-5.4"].Details; len(details) != 0 {
		t.Fatalf("details len with include_details=false = %d, want 0", len(details))
	}

	rec = httptest.NewRecorder()
	ginCtx, _ = gin.CreateTestContext(rec)
	ginCtx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/usage?since="+recentTime.Add(-time.Minute).Format(time.RFC3339), nil)
	handler.GetUsageStatistics(ginCtx)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	payload = decodeUsageStatisticsPayload(t, rec)
	details := payload.Usage.APIs["test-key"].Models["gpt-5.4"].Details
	if len(details) != 1 || !details[0].Timestamp.Equal(recentTime) {
		t.Fatalf("details with since = %#v, want only recent detail", details)
	}
}

func TestGetUsageQueuePopsRequestedRecords(t *testing.T) {
	gin.SetMode(gin.TestMode)
	withManagementUsageQueue(t, func() {
		redisqueue.Enqueue([]byte(`{"id":1}`))
		redisqueue.Enqueue([]byte(`{"id":2}`))
		redisqueue.Enqueue([]byte(`{"id":3}`))

		rec := httptest.NewRecorder()
		ginCtx, _ := gin.CreateTestContext(rec)
		ginCtx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/usage-queue?count=2", nil)

		h := &Handler{}
		h.GetUsageQueue(ginCtx)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
		}

		var payload []json.RawMessage
		if errUnmarshal := json.Unmarshal(rec.Body.Bytes(), &payload); errUnmarshal != nil {
			t.Fatalf("unmarshal response: %v", errUnmarshal)
		}
		if len(payload) != 2 {
			t.Fatalf("response records = %d, want 2", len(payload))
		}
		requireRecordID(t, payload[0], 1)
		requireRecordID(t, payload[1], 2)

		remaining := redisqueue.PopOldest(10)
		if len(remaining) != 1 || string(remaining[0]) != `{"id":3}` {
			t.Fatalf("remaining queue = %q, want third item only", remaining)
		}
	})
}

func TestGetUsageQueueInvalidCountDoesNotPop(t *testing.T) {
	gin.SetMode(gin.TestMode)
	withManagementUsageQueue(t, func() {
		redisqueue.Enqueue([]byte(`{"id":1}`))

		rec := httptest.NewRecorder()
		ginCtx, _ := gin.CreateTestContext(rec)
		ginCtx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/usage-queue?count=0", nil)

		h := &Handler{}
		h.GetUsageQueue(ginCtx)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
		}

		remaining := redisqueue.PopOldest(10)
		if len(remaining) != 1 || string(remaining[0]) != `{"id":1}` {
			t.Fatalf("remaining queue = %q, want original item", remaining)
		}
	})
}

func withManagementUsageQueue(t *testing.T, fn func()) {
	t.Helper()

	prevQueueEnabled := redisqueue.Enabled()
	redisqueue.SetEnabled(false)
	redisqueue.SetEnabled(true)

	defer func() {
		redisqueue.SetEnabled(false)
		redisqueue.SetEnabled(prevQueueEnabled)
	}()

	fn()
}

func decodeUsageStatisticsPayload(t *testing.T, rec *httptest.ResponseRecorder) struct {
	Usage          mgmtusage.StatisticsSnapshot `json:"usage"`
	FailedRequests int64                        `json:"failed_requests"`
} {
	t.Helper()
	var payload struct {
		Usage          mgmtusage.StatisticsSnapshot `json:"usage"`
		FailedRequests int64                        `json:"failed_requests"`
	}
	if errUnmarshal := json.Unmarshal(rec.Body.Bytes(), &payload); errUnmarshal != nil {
		t.Fatalf("unmarshal usage response: %v body=%s", errUnmarshal, rec.Body.String())
	}
	return payload
}

func requireRecordID(t *testing.T, raw json.RawMessage, want int) {
	t.Helper()

	var payload struct {
		ID int `json:"id"`
	}
	if errUnmarshal := json.Unmarshal(raw, &payload); errUnmarshal != nil {
		t.Fatalf("unmarshal record: %v", errUnmarshal)
	}
	if payload.ID != want {
		t.Fatalf("record id = %d, want %d", payload.ID, want)
	}
}
