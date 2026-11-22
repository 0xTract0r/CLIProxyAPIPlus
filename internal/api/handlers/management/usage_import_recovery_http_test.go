package management

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usage"
)

func TestUsageImportRejectsCorruptInitialHistoryAndRecovers(t *testing.T) {
	const prefix = "/v0/management/usage"
	const history = `{"version":2,"usage":{"apis":{"old-fixture":{"models":{"fixture-model":{"details":[{"timestamp":"2026-03-01T00:00:00Z","tokens":{"input_tokens":4}}]}}}}}}`
	const rejected = `{"version":2,"usage":{"apis":{"rejected-fixture":{"models":{"fixture-model":{"details":[{"timestamp":"2026-03-02T00:00:00Z","tokens":{"input_tokens":7}}]}}}}}}`
	const accepted = `{"version":2,"usage":{"apis":{"accepted-fixture":{"models":{"fixture-model":{"details":[{"timestamp":"2026-03-03T00:00:00Z","tokens":{"input_tokens":11}}]}}}}}}`
	dir := t.TempDir()
	broken := filepath.Join(dir, "broken.json")
	brokenBytes := []byte("{invalid history")
	if err := os.WriteFile(broken, brokenBytes, 0600); err != nil {
		t.Fatal(err)
	}
	stats := usage.NewRequestStatistics()
	if err := stats.SetPersistencePath(broken); err == nil {
		t.Fatal("initial bind accepted corrupt history")
	}

	newRouter := func(stats *usage.RequestStatistics) *gin.Engine {
		handler := &Handler{}
		handler.SetUsageStatistics(stats)
		router := gin.New()
		router.POST(prefix+"/import", handler.ImportUsageStatistics)
		router.GET(prefix, handler.GetUsageStatistics)
		router.GET(prefix+"/export", handler.ExportUsageStatistics)
		return router
	}
	request := func(router http.Handler, method, suffix, body string, status int) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, prefix+suffix, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(rec, req)
		if rec.Code != status {
			t.Fatalf("%s %s: status = %d, want %d, body=%s", method, suffix, rec.Code, status, rec.Body)
		}
		return rec
	}
	snapshot := func(router http.Handler, suffix string, count, tokens int64, labels ...string) {
		t.Helper()
		rec := request(router, http.MethodGet, suffix, "", http.StatusOK)
		var view struct {
			Usage usage.StatisticsSnapshot `json:"usage"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
			t.Fatal(err)
		}
		if view.Usage.TotalRequests != count || view.Usage.TotalTokens != tokens || len(view.Usage.APIs) != len(labels) {
			t.Fatalf("unexpected %q snapshot: requests=%d tokens=%d groups=%d", suffix, view.Usage.TotalRequests, view.Usage.TotalTokens, len(view.Usage.APIs))
		}
		for _, label := range labels {
			model := view.Usage.APIs[label].Models["fixture-model"]
			if model.TotalRequests != 1 || len(model.Details) != 1 {
				t.Fatalf("%q snapshot lost expected row for %q: %+v", suffix, label, model)
			}
		}
	}
	assertBrokenBytes := func() {
		t.Helper()
		got, err := os.ReadFile(broken)
		if err != nil || !bytes.Equal(got, brokenBytes) {
			t.Fatalf("corrupt source changed: bytes=%q, err=%v", got, err)
		}
	}

	router := newRouter(stats)
	request(router, http.MethodPost, "/import", rejected, http.StatusConflict)
	snapshot(router, "", 0, 0)
	snapshot(router, "/export", 0, 0)
	assertBrokenBytes()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(broken) {
		t.Fatalf("rejected import left a staging or replacement file: %v", entries)
	}

	recovered := filepath.Join(dir, "recovered.json")
	if err := os.WriteFile(recovered, []byte(history), 0600); err != nil {
		t.Fatal(err)
	}
	if err := stats.SetPersistencePath(recovered); err != nil {
		t.Fatalf("bind normal history: %v", err)
	}
	snapshot(router, "/export", 1, 4, "old-fixture")
	rec := request(router, http.MethodPost, "/import", accepted, http.StatusOK)
	var result struct {
		Added         int   `json:"added"`
		Skipped       int   `json:"skipped"`
		TotalRequests int64 `json:"total_requests"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Added != 1 || result.Skipped != 0 || result.TotalRequests != 2 {
		t.Fatalf("unexpected successful import result: %+v", result)
	}
	snapshot(router, "/export", 2, 15, "old-fixture", "accepted-fixture")

	reopened := usage.NewRequestStatistics()
	if err := reopened.SetPersistencePath(recovered); err != nil {
		t.Fatalf("reopen committed history: %v", err)
	}
	snapshot(newRouter(reopened), "/export", 2, 15, "old-fixture", "accepted-fixture")
	assertBrokenBytes()
}
