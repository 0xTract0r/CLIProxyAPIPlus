package management

import (
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	log "github.com/sirupsen/logrus"
)

var (
	errRequestLogMissing  = errors.New("request log not found")
	errArchiveUnavailable = errors.New("request log archive unavailable")
	// A stalled archive read must not create unbounded management I/O workers.
	requestLogArchiveSlots = make(chan struct{}, 2)
)

func openRequestLogIn(dir, requestID, exactName string) (*os.File, string, error) {
	directory, err := os.Lstat(filepath.Clean(dir))
	if err != nil {
		return nil, "", err
	}
	if directory.Mode()&os.ModeSymlink != 0 || !directory.IsDir() {
		return nil, "", errors.New("symlink request log directory is not allowed")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, "", err
	}
	shortID := logging.ShortRequestID(requestID)
	byName := make(map[string]*requestLogCandidate)
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		logicalName := strings.TrimSuffix(name, ".gz")
		matchID := requestID
		switch {
		case exactName != "":
			if name != exactName && !(strings.HasSuffix(exactName, ".log") && name == exactName+".gz") {
				continue
			}
		case strings.HasSuffix(logicalName, "-"+requestID+".log"):
		case strings.HasSuffix(logicalName, "-"+shortID+".log"):
			matchID = shortID
		default:
			continue
		}
		info, err := entry.Info()
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, "", err
		}
		if !info.Mode().IsRegular() {
			continue
		}
		candidate := byName[logicalName]
		if candidate == nil {
			candidate = &requestLogCandidate{name: logicalName, requestID: matchID, metadata: parseLogMetadata(logicalName, matchID)}
			byName[logicalName] = candidate
		}
		if name == logicalName {
			candidate.plain = info
		} else {
			candidate.compressed = info
		}
	}
	candidates := make([]*requestLogCandidate, 0, len(byName))
	for _, candidate := range byName {
		candidates = append(candidates, candidate)
	}
	sort.Slice(candidates, func(i, j int) bool {
		left, right := candidates[i], candidates[j]
		if left.requestID != right.requestID {
			// Legacy logs may contain the complete ID, which identifies the request
			// more precisely than a matching shortened suffix.
			return left.requestID == requestID
		}
		return logFileIsNewer(left.name, left.preferred().ModTime(), left.metadata, right.name, right.preferred().ModTime(), right.metadata)
	})
	for _, candidate := range candidates {
		// Plain and compressed copies of the same log can briefly coexist.
		for _, info := range []os.FileInfo{candidate.plain, candidate.compressed} {
			if info == nil {
				continue
			}
			file, err := os.Open(filepath.Join(dir, info.Name()))
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return nil, "", err
			}
			opened, err := file.Stat()
			if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
				_ = file.Close()
				if err != nil {
					return nil, "", err
				}
				continue
			}
			return file, info.Name(), nil
		}
	}
	return nil, "", errRequestLogMissing
}

type requestLogCandidate struct {
	name       string
	requestID  string
	metadata   logFileMeta
	plain      os.FileInfo
	compressed os.FileInfo
}

func (c *requestLogCandidate) preferred() os.FileInfo {
	if c.plain != nil {
		return c.plain
	}
	return c.compressed
}

// openRequestLog holds the selected FD across migration. Local ENOENT falls
// through to the archive only after the archiver has published its final file.
func (h *Handler) openRequestLog(requestID, exactName string) (*os.File, string, func(), error) {
	file, name, err := openRequestLogIn(h.logDirectory(), requestID, exactName)
	noop := func() {}
	if err == nil {
		return file, name, noop, nil
	}
	if !errors.Is(err, errRequestLogMissing) && !os.IsNotExist(err) {
		return nil, "", noop, err
	}
	if h.requestLogArchiveDir == "" {
		return nil, "", noop, errRequestLogMissing
	}
	select {
	case requestLogArchiveSlots <- struct{}{}:
	default:
		return nil, "", noop, errArchiveUnavailable
	}
	release := func() { <-requestLogArchiveSlots }
	file, name, err = openRequestLogIn(h.requestLogArchiveDir, requestID, exactName)
	if err != nil {
		release()
		if errors.Is(err, errRequestLogMissing) {
			return nil, "", noop, errRequestLogMissing
		}
		return nil, "", noop, fmt.Errorf("%w: %v", errArchiveUnavailable, err)
	}
	return file, name, release, nil
}

func (h *Handler) serveRequestLog(c *gin.Context, requestID, exactName string) {
	file, name, release, err := h.openRequestLog(requestID, exactName)
	if err != nil {
		switch {
		case errors.Is(err, errRequestLogMissing):
			c.JSON(http.StatusNotFound, gin.H{"error": "request log not found"})
		case errors.Is(err, errArchiveUnavailable):
			c.Header("Retry-After", "5")
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "request log archive temporarily unavailable"})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read request log"})
		}
		return
	}
	defer release()
	defer func() {
		if err := file.Close(); err != nil {
			log.WithError(err).Warn("failed to close request log")
		}
	}()
	h.serveOpenedRequestLog(c, file, name)
}

func (h *Handler) serveOpenedRequestLog(c *gin.Context, file *os.File, name string) {
	setDownloadHeaders := func() {
		c.Header("Content-Type", "text/plain; charset=utf-8")
		c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=%q", strings.TrimSuffix(name, ".gz")))
	}
	if !strings.HasSuffix(name, ".gz") {
		info, err := file.Stat()
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read request log"})
			return
		}
		setDownloadHeaders()
		http.ServeContent(c.Writer, c.Request, name, info.ModTime(), file)
		return
	}
	gz, err := gzip.NewReader(file)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to decompress request log"})
		return
	}
	defer func() { _ = gz.Close() }()
	setDownloadHeaders()
	c.Status(http.StatusOK)
	if _, err := io.Copy(c.Writer, gz); err != nil {
		log.WithError(err).Warn("failed to stream request log")
	}
}

type requestErrorLogInfo struct {
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	Modified int64  `json:"modified"`
}

func listErrorLogsIn(dir string) ([]requestErrorLogInfo, error) {
	directory, err := os.Lstat(filepath.Clean(dir))
	if err != nil {
		return nil, err
	}
	if directory.Mode()&os.ModeSymlink != 0 || !directory.IsDir() {
		return nil, errors.New("symlink request log directory is not allowed")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	files := make([]requestErrorLogInfo, 0)
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || e.Type()&os.ModeSymlink != 0 || !strings.HasPrefix(name, "error-") || !(strings.HasSuffix(name, ".log") || strings.HasSuffix(name, ".log.gz")) {
			continue
		}
		info, err := e.Info()
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			continue
		}
		files = append(files, requestErrorLogInfo{Name: name, Size: info.Size(), Modified: info.ModTime().Unix()})
	}
	return files, nil
}

func (h *Handler) requestErrorLogs() ([]requestErrorLogInfo, bool, error) {
	files, err := listErrorLogsIn(h.logDirectory())
	if os.IsNotExist(err) {
		files = []requestErrorLogInfo{}
		err = nil
	}
	if err != nil {
		return nil, false, err
	}
	if h.requestLogArchiveDir == "" {
		return files, false, nil
	}
	select {
	case requestLogArchiveSlots <- struct{}{}:
	default:
		return files, true, nil
	}
	archived, archiveErr := listErrorLogsIn(h.requestLogArchiveDir)
	<-requestLogArchiveSlots
	if archiveErr != nil {
		return files, true, nil
	}
	seen := make(map[string]bool, len(files))
	for _, f := range files {
		seen[f.Name] = true
	}
	for _, f := range archived {
		if !seen[f.Name] {
			files = append(files, f)
			seen[f.Name] = true
		}
	}
	return files, false, nil
}
