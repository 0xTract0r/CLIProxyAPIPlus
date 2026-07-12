package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
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

func TestExportUsageStatisticsWithoutParamsReturnsFullSnapshot(t *testing.T) {
	gin.SetMode(gin.TestMode)
	stats := mgmtusage.NewRequestStatistics()
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		stats.Record(context.Background(), coreusage.Record{
			APIKey:      "test-key",
			Model:       "gpt-5.4",
			RequestedAt: base.Add(time.Duration(i) * time.Minute),
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
	ginCtx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/usage/export", nil)
	handler.ExportUsageStatistics(ginCtx)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	payload := decodeUsageExportPayload(t, rec)
	if payload.HasMore {
		t.Fatalf("HasMore = true, want false for unwindowed export")
	}
	if payload.NextSince != "" {
		t.Fatalf("NextSince = %q, want empty for unwindowed export", payload.NextSince)
	}
	details := payload.Usage.APIs["test-key"].Models["gpt-5.4"].Details
	if len(details) != 3 {
		t.Fatalf("details len = %d, want 3", len(details))
	}
}

func TestExportUsageStatisticsWindowedPaginatesInOrder(t *testing.T) {
	gin.SetMode(gin.TestMode)
	stats := mgmtusage.NewRequestStatistics()
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	// Record out of chronological order.
	offsets := []int{2, 0, 3, 1}
	for _, offset := range offsets {
		stats.Record(context.Background(), coreusage.Record{
			APIKey:      "test-key",
			Model:       "gpt-5.4",
			RequestedAt: base.Add(time.Duration(offset) * time.Minute),
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
	sinceParam := base.Format(time.RFC3339)
	ginCtx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/usage/export?since="+sinceParam+"&limit=2", nil)
	handler.ExportUsageStatistics(ginCtx)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	payload := decodeUsageExportPayload(t, rec)
	if !payload.HasMore {
		t.Fatalf("HasMore = false, want true")
	}
	details := payload.Usage.APIs["test-key"].Models["gpt-5.4"].Details
	if len(details) != 2 {
		t.Fatalf("details len = %d, want 2", len(details))
	}
	if !details[0].Timestamp.Equal(base.Add(time.Minute)) || !details[1].Timestamp.Equal(base.Add(2*time.Minute)) {
		t.Fatalf("details out of order: %+v", details)
	}
	if payload.NextSince == "" {
		t.Fatalf("NextSince = empty, want non-empty cursor")
	}

	// Follow-up call using next_since must not repeat already-returned details.
	rec2 := httptest.NewRecorder()
	ginCtx2, _ := gin.CreateTestContext(rec2)
	ginCtx2.Request = httptest.NewRequest(http.MethodGet, "/v0/management/usage/export?since="+payload.NextSince+"&limit=2", nil)
	handler.ExportUsageStatistics(ginCtx2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec2.Code, http.StatusOK, rec2.Body.String())
	}
	payload2 := decodeUsageExportPayload(t, rec2)
	if payload2.HasMore {
		t.Fatalf("page2 HasMore = true, want false")
	}
	details2 := payload2.Usage.APIs["test-key"].Models["gpt-5.4"].Details
	if len(details2) != 1 {
		t.Fatalf("page2 details len = %d, want 1", len(details2))
	}
	if !details2[0].Timestamp.Equal(base.Add(3 * time.Minute)) {
		t.Fatalf("page2 detail timestamp = %s, want %s", details2[0].Timestamp, base.Add(3*time.Minute))
	}
}

// TestExportUsageStatisticsAppliesGlobalLimitAcrossBuckets is an HTTP-layer
// regression test for the production incident where /usage/export?limit=5000
// returned 124,083 details in a single response because DetailLimit was
// applied independently per (api, model) bucket. With multiple api keys and
// models, each individually under the limit, the combined export payload
// must still be capped at `limit` total details.
func TestExportUsageStatisticsAppliesGlobalLimitAcrossBuckets(t *testing.T) {
	gin.SetMode(gin.TestMode)
	stats := mgmtusage.NewRequestStatistics()
	base := time.Date(2026, 6, 4, 0, 0, 0, 0, time.UTC)

	apiKeys := []string{"key-a", "key-b"}
	models := []string{"model-1", "model-2", "model-3"}
	perBucket := 5 // 2 keys x 3 models x 5 = 30 total, no single bucket exceeds a limit of 8
	seq := 0
	for _, apiKey := range apiKeys {
		for _, model := range models {
			for i := 0; i < perBucket; i++ {
				stats.Record(context.Background(), coreusage.Record{
					APIKey:      apiKey,
					Model:       model,
					RequestedAt: base.Add(time.Duration(seq) * time.Second),
					Detail: coreusage.Detail{
						InputTokens:  1,
						OutputTokens: 1,
						TotalTokens:  2,
					},
				})
				seq++
			}
		}
	}

	handler := &Handler{}
	handler.SetUsageStatistics(stats)

	const limit = 8 // > any single bucket (5), well under combined total (30)
	rec := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(rec)
	ginCtx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/usage/export?limit="+strconv.Itoa(limit), nil)
	handler.ExportUsageStatistics(ginCtx)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	payload := decodeUsageExportPayload(t, rec)
	if !payload.HasMore {
		t.Fatalf("HasMore = false, want true (only %d of %d records should be returned)", limit, seq)
	}
	total := 0
	for _, api := range payload.Usage.APIs {
		for _, model := range api.Models {
			total += len(model.Details)
		}
	}
	if total != limit {
		t.Fatalf("combined details across all buckets = %d, want exactly %d (global limit, not per-bucket)", total, limit)
	}
}

func TestExportUsageStatisticsAcceptsUnixMillisSince(t *testing.T) {
	gin.SetMode(gin.TestMode)
	stats := mgmtusage.NewRequestStatistics()
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	stats.Record(context.Background(), coreusage.Record{
		APIKey:      "test-key",
		Model:       "gpt-5.4",
		RequestedAt: base.Add(time.Minute),
		Detail: coreusage.Detail{
			InputTokens: 10, OutputTokens: 20, TotalTokens: 30,
		},
	})

	handler := &Handler{}
	handler.SetUsageStatistics(stats)

	rec := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(rec)
	sinceMillis := base.UnixMilli()
	ginCtx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/usage/export?since="+strconv.FormatInt(sinceMillis, 10), nil)
	handler.ExportUsageStatistics(ginCtx)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	payload := decodeUsageExportPayload(t, rec)
	details := payload.Usage.APIs["test-key"].Models["gpt-5.4"].Details
	if len(details) != 1 {
		t.Fatalf("details len = %d, want 1", len(details))
	}
}

func TestExportUsageStatisticsInvalidSinceReturnsBadRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := &Handler{}
	handler.SetUsageStatistics(mgmtusage.NewRequestStatistics())

	rec := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(rec)
	ginCtx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/usage/export?since=not-a-time", nil)
	handler.ExportUsageStatistics(ginCtx)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
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

func decodeUsageExportPayload(t *testing.T, rec *httptest.ResponseRecorder) struct {
	Usage     mgmtusage.StatisticsSnapshot `json:"usage"`
	HasMore   bool                         `json:"has_more"`
	NextSince string                       `json:"next_since"`
} {
	t.Helper()
	var payload struct {
		Usage     mgmtusage.StatisticsSnapshot `json:"usage"`
		HasMore   bool                         `json:"has_more"`
		NextSince string                       `json:"next_since"`
	}
	if errUnmarshal := json.Unmarshal(rec.Body.Bytes(), &payload); errUnmarshal != nil {
		t.Fatalf("unmarshal usage export response: %v body=%s", errUnmarshal, rec.Body.String())
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
