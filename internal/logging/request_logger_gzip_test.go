package logging

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func readCompressedRequestLog(t *testing.T, path string) []byte {
	t.Helper()
	if !strings.HasSuffix(path, ".log.gz") {
		t.Fatalf("expected compressed request log, got %s", path)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = gz.Close() }()
	body, err := io.ReadAll(gz)
	if err != nil {
		t.Fatalf("read complete gzip including checksum: %v", err)
	}
	return body
}

func TestCompressedRequestLogPublishesOnlyCompleteFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "request.log")
	payload := bytes.Repeat([]byte("complete request body\n"), 65536)
	err := writeCompressedRequestLog(path, func(w io.Writer) error {
		for _, name := range []string{path, path + ".gz"} {
			if _, err := os.Stat(name); !os.IsNotExist(err) {
				t.Fatalf("published before completion: %s", name)
			}
		}
		_, err := w.Write(payload)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(readCompressedRequestLog(t, path+".gz"), payload) {
		t.Fatal("gzip roundtrip changed content")
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != "request.log.gz" {
		t.Fatalf("unexpected final files: %v, %v", entries, err)
	}
	s, err := os.Stat(path + ".gz")
	if err != nil {
		t.Fatal(err)
	}
	if s.Size() >= int64(len(payload))/2 {
		t.Fatalf("compressible fixture not reduced: %d", s.Size())
	}
}

func TestCompressedRequestLogCleansFailedWriteAndPublish(t *testing.T) {
	for _, fail := range []string{"write", "publish"} {
		t.Run(fail, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "request.log")
			if fail == "publish" {
				if err := os.Mkdir(path+".gz", 0o755); err != nil {
					t.Fatal(err)
				}
			}
			want := errors.New("source read failed")
			err := writeCompressedRequestLog(path, func(w io.Writer) error {
				if _, err := io.WriteString(w, "partial body"); err != nil {
					return err
				}
				if fail == "write" {
					return want
				}
				return nil
			})
			if err == nil {
				t.Fatal("expected failure")
			}
			if fail == "write" && !errors.Is(err, want) {
				t.Fatalf("lost source error: %v", err)
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range entries {
				if !e.IsDir() {
					t.Fatalf("failed write left a file: %s", e.Name())
				}
			}
		})
	}
}

func TestStreamingRequestLogWritesGzipAndCleansSpools(t *testing.T) {
	dir := t.TempDir()
	logger := NewFileRequestLogger(true, dir, "", 0)
	w, err := logger.LogStreamingRequest("/v1/responses", "POST", nil, []byte(`{"input":"hello"}`), "stream-gzip")
	if err != nil {
		t.Fatal(err)
	}
	w.WriteStatus(200, nil)
	if err := w.WriteAPIRequest([]byte("upstream request")); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteAPIResponse([]byte("upstream response")); err != nil {
		t.Fatal(err)
	}
	w.WriteChunkAsync([]byte("data: first\n\n"))
	w.WriteChunkAsync([]byte("data: last\n\n"))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("spools remain: %v %v", entries, err)
	}
	body := readCompressedRequestLog(t, filepath.Join(dir, entries[0].Name()))
	for _, want := range []string{`{"input":"hello"}`, "upstream request", "upstream response", "data: first", "data: last"} {
		if !bytes.Contains(body, []byte(want)) {
			t.Fatalf("missing section %q", want)
		}
	}
}

func TestStreamingRequestLogCleansSpoolsOnPublishFailure(t *testing.T) {
	dir := t.TempDir()
	logger := NewFileRequestLogger(true, dir, "", 0)
	writer, err := logger.LogStreamingRequest("/v1/responses", "POST", nil, []byte("request"), "fail")
	if err != nil {
		t.Fatal(err)
	}
	w := writer.(*FileStreamingLogWriter)
	if err := os.Mkdir(w.logFilePath+".gz", 0o755); err != nil {
		t.Fatal(err)
	}
	w.WriteChunkAsync([]byte("response"))
	if err := w.Close(); err == nil {
		t.Fatal("expected publication failure")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || !entries[0].IsDir() {
		t.Fatalf("temporary files remain: %v", entries)
	}
}

func TestErrorLogCleanupCountsPlainAndGzipTogether(t *testing.T) {
	dir := t.TempDir()
	for i, name := range []string{"error-old.log", "error-mid.log.gz", "error-new.log.gz", "normal.log.gz"} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("fixture"), 0o644); err != nil {
			t.Fatal(err)
		}
		when := time.Unix(int64(i), 0)
		if err := os.Chtimes(path, when, when); err != nil {
			t.Fatal(err)
		}
	}
	logger := NewFileRequestLogger(false, dir, "", 1)
	if err := logger.cleanupOldErrorLogs(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("wrong retained count: %v", entries)
	}
	for _, name := range []string{"error-new.log.gz", "normal.log.gz"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
}
