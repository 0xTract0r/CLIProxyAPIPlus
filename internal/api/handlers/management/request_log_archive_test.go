package management

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestRequestLogArchiveReadAndLocalPriority(t *testing.T) {
	for _, compressed := range []bool{false, true} {
		t.Run(map[bool]string{false: "plain", true: "gzip"}[compressed], func(t *testing.T) {
			local, archive := t.TempDir(), t.TempDir()
			const id = "archive-read"
			if compressed {
				writeGzipRequestLog(t, archive, id, "archived content")
			} else if err := os.WriteFile(filepath.Join(archive, "app-"+id+".log"), []byte("archived content"), 0o600); err != nil {
				t.Fatal(err)
			}
			h := newLogsTestHandler(local, true)
			h.requestLogArchiveDir = archive
			r := performGetRequestLogByID(t, h, id)
			if r.Code != 200 || r.Body.String() != "archived content" {
				t.Fatalf("archive: %d %q", r.Code, r.Body.String())
			}
			if err := os.WriteFile(filepath.Join(local, "app-"+id+".log"), []byte("local content"), 0o600); err != nil {
				t.Fatal(err)
			}
			h.requestLogArchiveDir = filepath.Join(archive, "unavailable")
			r = performGetRequestLogByID(t, h, id)
			if r.Code != 200 || r.Body.String() != "local content" {
				t.Fatalf("local priority: %d %q", r.Code, r.Body.String())
			}
		})
	}
}

func TestRequestLogArchiveUnavailableAndMissing(t *testing.T) {
	h := newLogsTestHandler(t.TempDir(), true)
	h.requestLogArchiveDir = filepath.Join(t.TempDir(), "offline")
	if r := performGetRequestLogByID(t, h, "missing"); r.Code != http.StatusServiceUnavailable {
		t.Fatalf("offline archive status %d", r.Code)
	}
	h.requestLogArchiveDir = t.TempDir()
	if r := performGetRequestLogByID(t, h, "missing"); r.Code != http.StatusNotFound {
		t.Fatalf("missing file status %d", r.Code)
	}
	for i := 0; i < cap(requestLogArchiveSlots); i++ {
		requestLogArchiveSlots <- struct{}{}
	}
	r := performGetRequestLogByID(t, h, "missing")
	for i := 0; i < cap(requestLogArchiveSlots); i++ {
		<-requestLogArchiveSlots
	}
	if r.Code != http.StatusServiceUnavailable {
		t.Fatalf("busy archive status %d", r.Code)
	}
}

func TestOpenedRequestLogSurvivesLocalMigration(t *testing.T) {
	local, archive := t.TempDir(), t.TempDir()
	name := "app-moving.log"
	if err := os.WriteFile(filepath.Join(local, name), []byte("complete body"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := newLogsTestHandler(local, true)
	h.requestLogArchiveDir = archive
	f, _, release, err := h.openRequestLog("moving", "")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	defer func() { _ = f.Close() }()
	if err := os.WriteFile(filepath.Join(archive, name), []byte("complete body"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(local, name)); err != nil {
		t.Fatal(err)
	}
	content, err := io.ReadAll(f)
	if err != nil || string(content) != "complete body" {
		t.Fatalf("open local FD lost: %q %v", content, err)
	}
	if r := performGetRequestLogByID(t, h, "moving"); r.Code != 200 || r.Body.String() != "complete body" {
		t.Fatalf("new lookup failed: %d", r.Code)
	}
}

func TestRequestLogArchiveRejectsSymlink(t *testing.T) {
	archive := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.log")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(archive, "app-link.log")); err != nil {
		t.Fatal(err)
	}
	h := newLogsTestHandler(t.TempDir(), true)
	h.requestLogArchiveDir = archive
	if r := performGetRequestLogByID(t, h, "link"); r.Code != 404 {
		t.Fatalf("followed symlink: %d", r.Code)
	}
}

func TestArchivedErrorLogsListAndDownload(t *testing.T) {
	local, archive := t.TempDir(), t.TempDir()
	for _, dir := range []string{local, archive} {
		if err := os.WriteFile(filepath.Join(dir, "error-duplicate.log"), []byte("local wins"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	path := writeGzipRequestLog(t, archive, "old", "archived error")
	name := "error-archived-old.log.gz"
	if err := os.Rename(path, filepath.Join(archive, name)); err != nil {
		t.Fatal(err)
	}
	h := newLogsTestHandler(local, false)
	h.requestLogArchiveDir = archive
	r := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(r)
	c.Request = httptest.NewRequest(http.MethodGet, "/request-error-logs", nil)
	h.GetRequestErrorLogs(c)
	var response struct {
		Files []requestErrorLogInfo `json:"files"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if r.Code != 200 || len(response.Files) != 2 {
		t.Fatalf("archive list: %d %s", r.Code, r.Body.String())
	}
	r = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(r)
	c.Request = httptest.NewRequest(http.MethodGet, "/request-error-logs/"+name, nil)
	c.Params = gin.Params{{Key: "name", Value: name}}
	h.DownloadRequestErrorLog(c)
	if r.Code != 200 || r.Body.String() != "archived error" {
		t.Fatalf("archive error download: %d %s", r.Code, r.Body.String())
	}
	r = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(r)
	c.Request = httptest.NewRequest(http.MethodGet, "/request-error-logs/error-archived-old.log", nil)
	c.Params = gin.Params{{Key: "name", Value: "error-archived-old.log"}}
	h.DownloadRequestErrorLog(c)
	if r.Code != 200 || r.Body.String() != "archived error" {
		t.Fatalf("pre-migration name lost: %d %s", r.Code, r.Body.String())
	}
}

func TestRequestLogArchiveRejectsLinkedRoot(t *testing.T) {
	outside := t.TempDir()
	writeGzipRequestLog(t, outside, "outside", "must not read")
	linked := filepath.Join(t.TempDir(), "linked")
	if err := os.Symlink(outside, linked); err != nil {
		t.Fatal(err)
	}
	h := newLogsTestHandler(t.TempDir(), true)
	h.requestLogArchiveDir = linked + string(os.PathSeparator)
	if r := performGetRequestLogByID(t, h, "outside"); r.Code != 503 {
		t.Fatalf("linked root read: %d", r.Code)
	}
}
