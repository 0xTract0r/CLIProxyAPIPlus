package api

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func readGzipRequestLogFixture(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	reader, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	return io.ReadAll(reader)
}

func TestRegisteredRequestLogArchiveRoutes(t *testing.T) {
	const key = "synthetic-management-key"
	const id = "abcd1234"
	const content = "synthetic archived request"
	archive := t.TempDir()
	t.Setenv("MANAGEMENT_PASSWORD", key)
	t.Setenv("REQUEST_LOG_ARCHIVE_DIR", archive)
	server := newTestServer(t)
	server.mgmt.SetLogDirectory(t.TempDir())

	f, err := os.Create(filepath.Join(archive, "v1-responses-2026-11-21T120000-"+id+".log.gz"))
	if err != nil {
		t.Fatal(err)
	}
	writer := gzip.NewWriter(f)
	if _, err := io.WriteString(writer, content); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	for _, endpoint := range []string{
		"/v0/management/request-log-by-id/" + id,
		"/v8/management/observability/logs/requests/" + id,
	} {
		req := httptest.NewRequest(http.MethodGet, endpoint, nil)
		req.RemoteAddr = "127.0.0.1:40000"
		req.Header.Set("Authorization", "Bearer "+key)
		rr := httptest.NewRecorder()
		server.engine.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK || rr.Body.String() != content {
			t.Errorf("%s: status %d, payload %q", endpoint, rr.Code, rr.Body.String())
		}
	}
}
