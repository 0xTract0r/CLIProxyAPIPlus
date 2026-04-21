package logging

import (
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

const logDirCleanerInterval = time.Minute

var logDirCleanerCancel context.CancelFunc

type logDirCleanerPolicy struct {
	maxBytes      int64
	compressAfter time.Duration
	deleteAfter   time.Duration
	protectedPath string
}

type logDirCleanerResult struct {
	deleted    int
	compressed int
}

type logFile struct {
	path         string
	name         string
	size         int64
	modTime      time.Time
	isCompressed bool
}

func configureLogDirCleanerLocked(logDir string, maxTotalSizeMB, compressAfterDays, deleteAfterDays int, protectedPath string) {
	stopLogDirCleanerLocked()

	policy := logDirCleanerPolicy{
		maxBytes:      int64(maxTotalSizeMB) * 1024 * 1024,
		compressAfter: normaliseRetentionDays(compressAfterDays),
		deleteAfter:   normaliseRetentionDays(deleteAfterDays),
		protectedPath: strings.TrimSpace(protectedPath),
	}
	if !policy.enabled() {
		return
	}

	dir := strings.TrimSpace(logDir)
	if dir == "" {
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	logDirCleanerCancel = cancel
	go runLogDirCleaner(ctx, filepath.Clean(dir), policy)
}

func normaliseRetentionDays(days int) time.Duration {
	if days <= 0 {
		return 0
	}
	return time.Duration(days) * 24 * time.Hour
}

func (p logDirCleanerPolicy) enabled() bool {
	return p.maxBytes > 0 || p.compressAfter > 0 || p.deleteAfter > 0
}

func stopLogDirCleanerLocked() {
	if logDirCleanerCancel == nil {
		return
	}
	logDirCleanerCancel()
	logDirCleanerCancel = nil
}

func runLogDirCleaner(ctx context.Context, logDir string, policy logDirCleanerPolicy) {
	ticker := time.NewTicker(logDirCleanerInterval)
	defer ticker.Stop()

	cleanOnce := func() {
		result, errClean := enforceLogDirPolicy(logDir, policy)
		if errClean != nil {
			log.WithError(errClean).Warn("logging: failed to enforce log directory policy")
			return
		}
		if result.compressed > 0 {
			log.Debugf("logging: compressed %d old log file(s)", result.compressed)
		}
		if result.deleted > 0 {
			log.Debugf("logging: removed %d old log file(s)", result.deleted)
		}
	}

	cleanOnce()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cleanOnce()
		}
	}
}

func enforceLogDirPolicy(logDir string, policy logDirCleanerPolicy) (logDirCleanerResult, error) {
	return enforceLogDirPolicyAt(logDir, policy, time.Now())
}

func enforceLogDirPolicyAt(logDir string, policy logDirCleanerPolicy, now time.Time) (logDirCleanerResult, error) {
	var result logDirCleanerResult

	files, total, errRead := scanLogDir(logDir)
	if errRead != nil {
		return result, errRead
	}

	if policy.deleteAfter > 0 {
		deleted := deleteExpiredLogFiles(files, policy, now)
		result.deleted += deleted
		if deleted > 0 {
			files, total, errRead = scanLogDir(logDir)
			if errRead != nil {
				return result, errRead
			}
		}
	}

	if policy.compressAfter > 0 {
		compressed := compressExpiredLogFiles(files, policy, now)
		result.compressed += compressed
		if compressed > 0 {
			files, total, errRead = scanLogDir(logDir)
			if errRead != nil {
				return result, errRead
			}
		}
	}

	if policy.maxBytes > 0 && total > policy.maxBytes {
		deleted := deleteLogsForSize(files, policy, total)
		result.deleted += deleted
	}

	return result, nil
}

func scanLogDir(logDir string) ([]logFile, int64, error) {
	dir := strings.TrimSpace(logDir)
	if dir == "" {
		return nil, 0, nil
	}
	dir = filepath.Clean(dir)

	entries, errRead := os.ReadDir(dir)
	if errRead != nil {
		if os.IsNotExist(errRead) {
			return nil, 0, nil
		}
		return nil, 0, errRead
	}

	files := make([]logFile, 0, len(entries))
	var total int64
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !isLogFileName(name) {
			continue
		}
		info, errInfo := entry.Info()
		if errInfo != nil || !info.Mode().IsRegular() {
			continue
		}
		path := filepath.Join(dir, name)
		files = append(files, logFile{
			path:         path,
			name:         name,
			size:         info.Size(),
			modTime:      info.ModTime(),
			isCompressed: strings.HasSuffix(strings.ToLower(name), ".log.gz"),
		})
		total += info.Size()
	}

	return files, total, nil
}

func deleteExpiredLogFiles(files []logFile, policy logDirCleanerPolicy, now time.Time) int {
	deleted := 0
	for _, file := range files {
		if !shouldDeleteByAge(file, policy, now) {
			continue
		}
		if errRemove := os.Remove(file.path); errRemove != nil {
			log.WithError(errRemove).Warnf("logging: failed to remove expired log file: %s", file.name)
			continue
		}
		deleted++
	}
	return deleted
}

func compressExpiredLogFiles(files []logFile, policy logDirCleanerPolicy, now time.Time) int {
	sort.Slice(files, func(i, j int) bool {
		return files[i].modTime.Before(files[j].modTime)
	})

	compressed := 0
	for _, file := range files {
		if !shouldCompressByAge(file, policy, now) {
			continue
		}
		if errCompress := compressLogFile(file); errCompress != nil {
			log.WithError(errCompress).Warnf("logging: failed to compress old log file: %s", file.name)
			continue
		}
		compressed++
	}
	return compressed
}

func deleteLogsForSize(files []logFile, policy logDirCleanerPolicy, total int64) int {
	sort.Slice(files, func(i, j int) bool {
		return files[i].modTime.Before(files[j].modTime)
	})

	deleted := 0
	for _, file := range files {
		if total <= policy.maxBytes {
			break
		}
		if isProtectedLogFile(file.path, policy.protectedPath) {
			continue
		}
		if errRemove := os.Remove(file.path); errRemove != nil {
			log.WithError(errRemove).Warnf("logging: failed to remove old log file: %s", file.name)
			continue
		}
		total -= file.size
		deleted++
	}
	return deleted
}

func shouldDeleteByAge(file logFile, policy logDirCleanerPolicy, now time.Time) bool {
	if policy.deleteAfter <= 0 || isProtectedLogFile(file.path, policy.protectedPath) {
		return false
	}
	return fileAge(now, file.modTime) > policy.deleteAfter
}

func shouldCompressByAge(file logFile, policy logDirCleanerPolicy, now time.Time) bool {
	if policy.compressAfter <= 0 || file.isCompressed || isProtectedLogFile(file.path, policy.protectedPath) {
		return false
	}
	return fileAge(now, file.modTime) > policy.compressAfter
}

func fileAge(now, modTime time.Time) time.Duration {
	if now.Before(modTime) {
		return 0
	}
	return now.Sub(modTime)
}

func isProtectedLogFile(path string, protectedPath string) bool {
	protected := strings.TrimSpace(protectedPath)
	if protected == "" {
		return false
	}
	return filepath.Clean(path) == filepath.Clean(protected)
}

func compressLogFile(file logFile) error {
	source, errOpen := os.Open(file.path)
	if errOpen != nil {
		return errOpen
	}
	defer func() {
		_ = source.Close()
	}()

	destPath := file.path + ".gz"
	tmpPath := destPath + ".tmp"
	dest, errCreate := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if errCreate != nil {
		return errCreate
	}

	gzipWriter := gzip.NewWriter(dest)
	gzipWriter.Name = file.name
	gzipWriter.ModTime = file.modTime

	copyErr := error(nil)
	if _, copyErr = io.Copy(gzipWriter, source); copyErr != nil {
		_ = gzipWriter.Close()
		_ = dest.Close()
		_ = os.Remove(tmpPath)
		return copyErr
	}
	if errClose := gzipWriter.Close(); errClose != nil {
		_ = dest.Close()
		_ = os.Remove(tmpPath)
		return errClose
	}
	if errClose := dest.Close(); errClose != nil {
		_ = os.Remove(tmpPath)
		return errClose
	}
	if errTimes := os.Chtimes(tmpPath, file.modTime, file.modTime); errTimes != nil {
		_ = os.Remove(tmpPath)
		return errTimes
	}
	if errRename := os.Rename(tmpPath, destPath); errRename != nil {
		_ = os.Remove(tmpPath)
		return errRename
	}
	if errTimes := os.Chtimes(destPath, file.modTime, file.modTime); errTimes != nil {
		_ = os.Remove(destPath)
		return errTimes
	}
	if errRemove := os.Remove(file.path); errRemove != nil {
		_ = os.Remove(destPath)
		return errRemove
	}
	return nil
}

func enforceLogDirSizeLimit(logDir string, maxBytes int64, protectedPath string) (int, error) {
	if maxBytes <= 0 {
		return 0, nil
	}

	result, err := enforceLogDirPolicyAt(logDir, logDirCleanerPolicy{
		maxBytes:      maxBytes,
		protectedPath: protectedPath,
	}, time.Now())
	if err != nil {
		return 0, err
	}
	return result.deleted, nil
}

func isLogFileName(name string) bool {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return false
	}
	lower := strings.ToLower(trimmed)
	return strings.HasSuffix(lower, ".log") || strings.HasSuffix(lower, ".log.gz")
}
