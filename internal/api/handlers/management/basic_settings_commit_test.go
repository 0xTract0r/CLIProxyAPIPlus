package management

import (
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBasicSettingsCommitBeforePublish(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("port: 8317\nlogs-compress-after-days: 7\nlogs-delete-after-days: 30\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(cfg, path, nil)
	r := gin.New()
	r.GET("/days", h.GetLogsCompressAfterDays)
	r.PUT("/days", h.PutLogsCompressAfterDays)
	r.PUT("/proxy", h.PutProxyURL)
	r.PUT("/strategy", h.PutRoutingStrategy)
	call := func(method, where, body string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		req := httptest.NewRequest(method, where, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		return w
	}
	if got := call(http.MethodPut, "/proxy", `{"value":"http://private:secret@bad.invalid:90808"}`); got.Code != 400 || strings.Contains(got.Body.String(), "secret") {
		t.Fatal("invalid URL was accepted or echoed")
	}
	if got := call(http.MethodPut, "/strategy", `{"value":"adaptive"}`); got.Code != 400 {
		t.Fatal("unsupported mode was accepted")
	}
	h.configFilePath = dir
	if got := call(http.MethodPut, "/days", `{"value":0}`); got.Code != 500 {
		t.Fatalf("failed save status %d", got.Code)
	}
	if got := call(http.MethodGet, "/days", ""); got.Code != 200 || !strings.Contains(got.Body.String(), ":7") {
		t.Fatal("failed edit changed the value")
	}
	h.configFilePath = path
	if got := call(http.MethodPut, "/days", `{"value":0}`); got.Code != 200 {
		t.Fatalf("save status %d", got.Code)
	}
	disk, err := config.LoadConfig(path)
	if err != nil || disk.LogsCompressAfterDays != 0 || disk.LogsDeleteAfterDays != 30 {
		t.Fatal("persisted configuration lost zero or sibling")
	}
}
