package logging

import (
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestEnforceLogDirSizeLimitDeletesOldest(t *testing.T) {
	dir := t.TempDir()

	writeLogFile(t, filepath.Join(dir, "old.log"), 60, time.Unix(1, 0))
	writeLogFile(t, filepath.Join(dir, "mid.log"), 60, time.Unix(2, 0))
	protected := filepath.Join(dir, "main.log")
	writeLogFile(t, protected, 60, time.Unix(3, 0))

	deleted, err := enforceLogDirSizeLimit(dir, 120, protected)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("expected 1 deleted file, got %d", deleted)
	}

	if _, err := os.Stat(filepath.Join(dir, "old.log")); !os.IsNotExist(err) {
		t.Fatalf("expected old.log to be removed, stat error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "mid.log")); err != nil {
		t.Fatalf("expected mid.log to remain, stat error: %v", err)
	}
	if _, err := os.Stat(protected); err != nil {
		t.Fatalf("expected protected main.log to remain, stat error: %v", err)
	}
}

func TestEnforceLogDirSizeLimitSkipsProtected(t *testing.T) {
	dir := t.TempDir()

	protected := filepath.Join(dir, "main.log")
	writeLogFile(t, protected, 200, time.Unix(1, 0))
	writeLogFile(t, filepath.Join(dir, "other.log"), 50, time.Unix(2, 0))

	deleted, err := enforceLogDirSizeLimit(dir, 100, protected)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("expected 1 deleted file, got %d", deleted)
	}

	if _, err := os.Stat(protected); err != nil {
		t.Fatalf("expected protected main.log to remain, stat error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "other.log")); !os.IsNotExist(err) {
		t.Fatalf("expected other.log to be removed, stat error: %v", err)
	}
}

func TestEnforceLogDirPolicyCompressesOldLogs(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(40*24*3600, 0)
	oldTime := now.Add(-10 * 24 * time.Hour)

	writeLogFile(t, filepath.Join(dir, "old.log"), 128, oldTime)
	protected := filepath.Join(dir, "main.log")
	writeLogFile(t, protected, 64, oldTime)

	result, err := enforceLogDirPolicyAt(dir, logDirCleanerPolicy{
		compressAfter: 7 * 24 * time.Hour,
		deleteAfter:   30 * 24 * time.Hour,
		protectedPath: protected,
	}, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.compressed != 1 {
		t.Fatalf("expected 1 compressed file, got %d", result.compressed)
	}
	if result.deleted != 0 {
		t.Fatalf("expected 0 deleted files, got %d", result.deleted)
	}

	if _, err := os.Stat(filepath.Join(dir, "old.log")); !os.IsNotExist(err) {
		t.Fatalf("expected old.log to be removed, stat error: %v", err)
	}

	compressedPath := filepath.Join(dir, "old.log.gz")
	info, err := os.Stat(compressedPath)
	if err != nil {
		t.Fatalf("expected compressed log to exist, stat error: %v", err)
	}
	if !info.ModTime().Equal(oldTime) {
		t.Fatalf("compressed log modtime = %s, want %s", info.ModTime(), oldTime)
	}

	reader, err := os.Open(compressedPath)
	if err != nil {
		t.Fatalf("open compressed log: %v", err)
	}
	defer func() { _ = reader.Close() }()

	gzReader, err := gzip.NewReader(reader)
	if err != nil {
		t.Fatalf("open gzip reader: %v", err)
	}
	defer func() { _ = gzReader.Close() }()

	data, err := io.ReadAll(gzReader)
	if err != nil {
		t.Fatalf("read gzip payload: %v", err)
	}
	if len(data) != 128 {
		t.Fatalf("compressed payload length = %d, want 128", len(data))
	}

	if _, err := os.Stat(protected); err != nil {
		t.Fatalf("expected protected main.log to remain, stat error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "main.log.gz")); !os.IsNotExist(err) {
		t.Fatalf("expected protected main.log not to be compressed, stat error: %v", err)
	}
}

func TestEnforceLogDirPolicyDeletesExpiredLogs(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(50*24*3600, 0)
	expiredTime := now.Add(-40 * 24 * time.Hour)
	midTime := now.Add(-10 * 24 * time.Hour)

	writeLogFile(t, filepath.Join(dir, "expired.log"), 64, expiredTime)
	writeCompressedLogFile(t, filepath.Join(dir, "expired.log.gz"), []byte("expired"), expiredTime)
	writeLogFile(t, filepath.Join(dir, "mid.log"), 64, midTime)

	result, err := enforceLogDirPolicyAt(dir, logDirCleanerPolicy{
		compressAfter: 7 * 24 * time.Hour,
		deleteAfter:   30 * 24 * time.Hour,
	}, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.deleted != 2 {
		t.Fatalf("expected 2 deleted files, got %d", result.deleted)
	}
	if result.compressed != 1 {
		t.Fatalf("expected 1 compressed file, got %d", result.compressed)
	}

	if _, err := os.Stat(filepath.Join(dir, "expired.log")); !os.IsNotExist(err) {
		t.Fatalf("expected expired.log to be removed, stat error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "expired.log.gz")); !os.IsNotExist(err) {
		t.Fatalf("expected expired.log.gz to be removed, stat error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "mid.log")); !os.IsNotExist(err) {
		t.Fatalf("expected mid.log to be replaced by gzip, stat error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "mid.log.gz")); err != nil {
		t.Fatalf("expected mid.log.gz to exist, stat error: %v", err)
	}
}

func TestEnforceLogDirPolicyKeepsProtectedMainLogDuringAgeCleanup(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(50*24*3600, 0)
	expiredTime := now.Add(-40 * 24 * time.Hour)
	protected := filepath.Join(dir, "main.log")

	writeLogFile(t, protected, 64, expiredTime)

	result, err := enforceLogDirPolicyAt(dir, logDirCleanerPolicy{
		compressAfter: 7 * 24 * time.Hour,
		deleteAfter:   30 * 24 * time.Hour,
		protectedPath: protected,
	}, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.deleted != 0 {
		t.Fatalf("expected 0 deleted files, got %d", result.deleted)
	}
	if result.compressed != 0 {
		t.Fatalf("expected 0 compressed files, got %d", result.compressed)
	}

	if _, err := os.Stat(protected); err != nil {
		t.Fatalf("expected protected main.log to remain, stat error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "main.log.gz")); !os.IsNotExist(err) {
		t.Fatalf("expected protected main.log not to be compressed, stat error: %v", err)
	}
}

func writeLogFile(t *testing.T, path string, size int, modTime time.Time) {
	t.Helper()

	data := make([]byte, size)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := os.Chtimes(path, modTime, modTime); err != nil {
		t.Fatalf("set times: %v", err)
	}
}

func writeCompressedLogFile(t *testing.T, path string, payload []byte, modTime time.Time) {
	t.Helper()

	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatalf("create compressed log: %v", err)
	}

	writer := gzip.NewWriter(file)
	writer.Name = filepath.Base(path)
	writer.ModTime = modTime
	if _, err := writer.Write(payload); err != nil {
		t.Fatalf("write compressed payload: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close gzip writer: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close compressed log file: %v", err)
	}
	if err := os.Chtimes(path, modTime, modTime); err != nil {
		t.Fatalf("set compressed times: %v", err)
	}
}
