package logging

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	log "github.com/sirupsen/logrus"
)

var (
	requestLogID                        atomic.Uint64
	sanitizeFilenameInvalidCharsPattern = regexp.MustCompile(`[<>:"|?*\s]`)
	sanitizeFilenameMultiHyphenPattern  = regexp.MustCompile(`-+`)
)

// LogRequest logs a complete non-streaming request/response cycle to a file.
//
// Parameters:
//   - url: The request URL
//   - method: The HTTP method
//   - requestHeaders: The request headers
//   - body: The request body
//   - statusCode: The response status code
//   - responseHeaders: The response headers
//   - response: The raw response data
//   - apiRequest: The API request data
//   - apiResponse: The API response data
//   - requestID: Optional request ID for log file naming
//   - requestTimestamp: When the request was received
//   - apiResponseTimestamp: When the API response was received
//
// Returns:
//   - error: An error if logging fails, nil otherwise
func (l *FileRequestLogger) LogRequest(url, method string, requestHeaders map[string][]string, body []byte, statusCode int, responseHeaders map[string][]string, response, websocketTimeline, apiRequest, apiResponse, apiWebsocketTimeline []byte, apiResponseErrors []*interfaces.ErrorMessage, requestID string, requestTimestamp, apiResponseTimestamp time.Time) error {
	return l.logRequest(url, method, requestHeaders, body, statusCode, responseHeaders, response, websocketTimeline, apiRequest, apiResponse, apiWebsocketTimeline, apiResponseErrors, false, requestID, requestTimestamp, apiResponseTimestamp)
}

// LogRequestWithOptions logs a request with optional forced logging behavior.
// The force flag allows writing error logs even when regular request logging is disabled.
func (l *FileRequestLogger) LogRequestWithOptions(url, method string, requestHeaders map[string][]string, body []byte, statusCode int, responseHeaders map[string][]string, response, websocketTimeline, apiRequest, apiResponse, apiWebsocketTimeline []byte, apiResponseErrors []*interfaces.ErrorMessage, force bool, requestID string, requestTimestamp, apiResponseTimestamp time.Time) error {
	return l.logRequestWithSources(url, method, requestHeaders, body, statusCode, responseHeaders, response, websocketTimeline, nil, apiRequest, nil, apiResponse, nil, apiWebsocketTimeline, nil, apiResponseErrors, force, requestID, requestTimestamp, apiResponseTimestamp)
}

func (l *FileRequestLogger) logRequest(url, method string, requestHeaders map[string][]string, body []byte, statusCode int, responseHeaders map[string][]string, response, websocketTimeline, apiRequest, apiResponse, apiWebsocketTimeline []byte, apiResponseErrors []*interfaces.ErrorMessage, force bool, requestID string, requestTimestamp, apiResponseTimestamp time.Time) error {
	return l.logRequestWithSources(url, method, requestHeaders, body, statusCode, responseHeaders, response, websocketTimeline, nil, apiRequest, nil, apiResponse, nil, apiWebsocketTimeline, nil, apiResponseErrors, force, requestID, requestTimestamp, apiResponseTimestamp)
}

// LogRequestWithOptionsAndSources logs a request with optional file-backed large sections.
func (l *FileRequestLogger) LogRequestWithOptionsAndSources(url, method string, requestHeaders map[string][]string, body []byte, statusCode int, responseHeaders map[string][]string, response, websocketTimeline []byte, websocketTimelineSource *FileBodySource, apiRequest, apiResponse, apiWebsocketTimeline []byte, apiWebsocketTimelineSource *FileBodySource, apiResponseErrors []*interfaces.ErrorMessage, force bool, requestID string, requestTimestamp, apiResponseTimestamp time.Time) error {
	return l.logRequestWithSources(url, method, requestHeaders, body, statusCode, responseHeaders, response, websocketTimeline, websocketTimelineSource, apiRequest, nil, apiResponse, nil, apiWebsocketTimeline, apiWebsocketTimelineSource, apiResponseErrors, force, requestID, requestTimestamp, apiResponseTimestamp)
}

// LogRequestWithOptionsAndAllSources logs a request with optional file-backed request and response sections.
func (l *FileRequestLogger) LogRequestWithOptionsAndAllSources(url, method string, requestHeaders map[string][]string, body []byte, statusCode int, responseHeaders map[string][]string, response, websocketTimeline []byte, websocketTimelineSource *FileBodySource, apiRequest []byte, apiRequestSource *FileBodySource, apiResponse []byte, apiResponseSource *FileBodySource, apiWebsocketTimeline []byte, apiWebsocketTimelineSource *FileBodySource, apiResponseErrors []*interfaces.ErrorMessage, force bool, requestID string, requestTimestamp, apiResponseTimestamp time.Time) error {
	return l.logRequestWithSources(url, method, requestHeaders, body, statusCode, responseHeaders, response, websocketTimeline, websocketTimelineSource, apiRequest, apiRequestSource, apiResponse, apiResponseSource, apiWebsocketTimeline, apiWebsocketTimelineSource, apiResponseErrors, force, requestID, requestTimestamp, apiResponseTimestamp)
}

func (l *FileRequestLogger) logRequestWithSources(url, method string, requestHeaders map[string][]string, body []byte, statusCode int, responseHeaders map[string][]string, response, websocketTimeline []byte, websocketTimelineSource *FileBodySource, apiRequest []byte, apiRequestSource *FileBodySource, apiResponse []byte, apiResponseSource *FileBodySource, apiWebsocketTimeline []byte, apiWebsocketTimelineSource *FileBodySource, apiResponseErrors []*interfaces.ErrorMessage, force bool, requestID string, requestTimestamp, apiResponseTimestamp time.Time) error {
	defer cleanupFileBodySources(websocketTimelineSource, apiRequestSource, apiResponseSource, apiWebsocketTimelineSource)

	if !l.enabled && !force {
		return nil
	}

	if l.homeEnabled && l.enabled {
		responseToWrite, decompressErr := l.decompressResponse(responseHeaders, response)
		if decompressErr != nil {
			responseToWrite = response
		}

		var buf bytes.Buffer
		writeErr := l.writeNonStreamingLog(
			&buf,
			url,
			method,
			requestHeaders,
			body,
			"",
			websocketTimeline,
			websocketTimelineSource,
			apiRequest,
			apiRequestSource,
			apiResponse,
			apiResponseSource,
			apiWebsocketTimeline,
			apiWebsocketTimelineSource,
			apiResponseErrors,
			statusCode,
			responseHeaders,
			responseToWrite,
			decompressErr,
			requestTimestamp,
			apiResponseTimestamp,
		)
		if writeErr != nil {
			return fmt.Errorf("failed to build request log content: %w", writeErr)
		}
		return l.forwardRequestLogToHome(context.Background(), requestHeaders, requestID, buf.String())
	}

	// Ensure logs directory exists
	if errEnsure := l.ensureLogsDir(); errEnsure != nil {
		return fmt.Errorf("failed to create logs directory: %w", errEnsure)
	}

	// Generate filename with request ID
	filename := l.generateFilename(url, requestID)
	if (force && !l.enabled) || (statusCode >= http.StatusBadRequest && statusCode != 499) {
		filename = l.generateErrorFilename(url, requestID)
	}

	requestBodyPath, errTemp := l.writeRequestBodyTempFile(body)
	if errTemp != nil {
		log.WithError(errTemp).Warn("failed to create request body temp file, falling back to direct write")
	}
	if requestBodyPath != "" {
		defer func() {
			if errRemove := os.Remove(requestBodyPath); errRemove != nil {
				log.WithError(errRemove).Warn("failed to remove request body temp file")
			}
		}()
	}

	responseToWrite, decompressErr := l.decompressResponse(responseHeaders, response)
	if decompressErr != nil {
		// If decompression fails, continue with original response and annotate the log output.
		responseToWrite = response
	}

	if requestTimestamp.IsZero() {
		requestTimestamp = time.Now()
	}
	location := DisplayLocation()
	writeErr := writeCompressedRequestLog(filepath.Join(l.logsDir, filename), func(logFile io.Writer) error {
		return l.writeNonStreamingLog(
			logFile,
			url,
			method,
			requestHeaders,
			body,
			requestBodyPath,
			websocketTimeline,
			websocketTimelineSource,
			apiRequest,
			apiRequestSource,
			apiResponse,
			apiResponseSource,
			apiWebsocketTimeline,
			apiWebsocketTimelineSource,
			apiResponseErrors,
			statusCode,
			responseHeaders,
			responseToWrite,
			decompressErr,
			requestTimestamp.In(location),
			apiResponseTimestamp.In(location),
		)
	})
	if writeErr != nil {
		return fmt.Errorf("failed to write log file: %w", writeErr)
	}

	if force && !l.enabled {
		if errCleanup := l.cleanupOldErrorLogs(); errCleanup != nil {
			log.WithError(errCleanup).Warn("failed to clean up old error logs")
		}
	}

	return nil
}

// writeCompressedRequestLog publishes a complete gzip log without first writing
// an uncompressed final file. Readers never see an unfinished gzip stream.
func writeCompressedRequestLog(plainPath string, write func(io.Writer) error) error {
	tempPath := filepath.Join(filepath.Dir(plainPath), ".request-log-"+rand.Text()+".tmp")
	file, errCreate := os.OpenFile(tempPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if errCreate != nil {
		return fmt.Errorf("create compressed request log: %w", errCreate)
	}
	defer func() { _ = os.Remove(tempPath) }()
	gz, errGzip := gzip.NewWriterLevel(file, gzip.BestSpeed)
	if errGzip != nil {
		_ = file.Close()
		return errGzip
	}
	errWrite := write(gz)
	errGzipClose := gz.Close()
	errFileClose := file.Close()
	if errWrite != nil {
		return errWrite
	}
	if errGzipClose != nil {
		return fmt.Errorf("finish compressed request log: %w", errGzipClose)
	}
	if errFileClose != nil {
		return fmt.Errorf("close compressed request log: %w", errFileClose)
	}
	ext := filepath.Ext(plainPath)
	base := strings.TrimSuffix(plainPath, ext)
	prefix, id := base, ""
	if i := strings.LastIndex(filepath.Base(base), "-"); i >= 0 {
		pos := len(base) - len(filepath.Base(base)) + i
		prefix, id = base[:pos], base[pos:]
	}
	for seq := 0; seq <= 1000; seq++ {
		logicalPath := plainPath
		if seq > 0 {
			logicalPath = fmt.Sprintf("%s_%d%s%s", prefix, seq, id, ext)
		}
		// Plain and gzip versions of the same logical name are read as one log.
		if _, err := os.Lstat(logicalPath); err == nil {
			continue
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("inspect request log name: %w", err)
		}
		target := logicalPath + ".gz"
		if err := os.Link(tempPath, target); err != nil {
			if os.IsExist(err) {
				if info, statErr := os.Lstat(target); statErr == nil && info.Mode().IsRegular() {
					continue
				}
			}
			return fmt.Errorf("publish compressed request log: %w", err)
		}
		return nil
	}
	return fmt.Errorf("too many conflicting log files for %s", plainPath)
}

// LogStreamingRequest initiates logging for a streaming request.
//
// Parameters:
//   - url: The request URL
//   - method: The HTTP method
//   - headers: The request headers
//   - body: The request body
//   - requestID: Optional request ID for log file naming
//
// Returns:
//   - StreamingLogWriter: A writer for streaming response chunks
//   - error: An error if logging initialization fails, nil otherwise
func (l *FileRequestLogger) LogStreamingRequest(url, method string, headers map[string][]string, body []byte, requestID string) (StreamingLogWriter, error) {
	if !l.enabled {
		return &NoOpStreamingLogWriter{}, nil
	}

	if l.homeEnabled {
		client := currentHomeRequestLogClient()
		if client == nil || !client.HeartbeatOK() {
			return &NoOpStreamingLogWriter{}, nil
		}
		return newHomeStreamingLogWriter(url, method, headers, body, requestID), nil
	}

	// Ensure logs directory exists
	if err := l.ensureLogsDir(); err != nil {
		return nil, fmt.Errorf("failed to create logs directory: %w", err)
	}

	// Generate filename with request ID
	filename := l.generateFilename(url, requestID)

	requestHeaders := make(map[string][]string, len(headers))
	for key, values := range headers {
		headerValues := make([]string, len(values))
		copy(headerValues, values)
		requestHeaders[key] = headerValues
	}

	requestBodyPath, errTemp := l.writeRequestBodyTempFile(body)
	if errTemp != nil {
		return nil, fmt.Errorf("failed to create request body temp file: %w", errTemp)
	}

	responseBodyFile, errCreate := os.CreateTemp(l.logsDir, "response-body-*.tmp")
	if errCreate != nil {
		_ = os.Remove(requestBodyPath)
		return nil, fmt.Errorf("failed to create response body temp file: %w", errCreate)
	}
	responseBodyPath := responseBodyFile.Name()

	// Create streaming writer
	writer := &FileStreamingLogWriter{
		logsDir:          l.logsDir,
		logFilename:      filename,
		url:              url,
		method:           method,
		timestamp:        time.Now(),
		requestHeaders:   requestHeaders,
		requestBodyPath:  requestBodyPath,
		responseBodyPath: responseBodyPath,
		responseBodyFile: responseBodyFile,
		chunkChan:        make(chan []byte, 100), // Buffered channel for async writes
		closeChan:        make(chan struct{}),
		errorChan:        make(chan error, 1),
	}

	// Start async writer goroutine
	go writer.asyncWriter()

	return writer, nil
}

// generateErrorFilename creates a filename with an error prefix to differentiate forced error logs.
func (l *FileRequestLogger) generateErrorFilename(url string, requestID ...string) string {
	return fmt.Sprintf("error-%s", l.generateFilename(url, requestID...))
}

// ensureLogsDir creates the logs directory if it doesn't exist.
//
// Returns:
//   - error: An error if directory creation fails, nil otherwise
func (l *FileRequestLogger) ensureLogsDir() error {
	if _, err := os.Stat(l.logsDir); os.IsNotExist(err) {
		return os.MkdirAll(l.logsDir, 0755)
	}
	return nil
}

// generateFilename creates a sanitized filename from the URL path and current timestamp.
// Format: v1-responses-2025-12-23T195811-a1b2c3d4.log
//
// Parameters:
//   - url: The request URL
//   - requestID: Optional request ID to include in filename
//
// Returns:
//   - string: A sanitized filename for the log file
func (l *FileRequestLogger) generateFilename(url string, requestID ...string) string {
	// Extract path from URL
	path := url
	if strings.Contains(url, "?") {
		path = strings.Split(url, "?")[0]
	}

	// Remove leading slash
	if strings.HasPrefix(path, "/") {
		path = path[1:]
	}

	// Sanitize path for filename
	sanitized := l.sanitizeForFilename(path)

	// Add timestamp
	// 人看的 per-request 抓包日志文件名时间戳，统一用显示时区（默认 UTC+8）。
	// 该格式（2006-01-02T150405，无分隔）仅作人看的可排序标签，全仓无任何代码反向
	// 解析它（lumberjack 轮转文件名走 2006-01-02T15-04-05 另一套，不受影响），
	// 因此切到 displayLoc 不存在读侧错位。出站时间字段不经过这里。
	timestamp := time.Now().In(DisplayLocation()).Format("2006-01-02T150405")

	// Use request ID if provided, otherwise use sequential ID
	var idPart string
	if len(requestID) > 0 && requestID[0] != "" {
		idPart = l.sanitizeForFilename(ShortRequestID(requestID[0]))
	} else {
		id := requestLogID.Add(1)
		idPart = fmt.Sprintf("%d", id)
	}

	return filepath.Base(fmt.Sprintf("%s-%s-%s.log", sanitized, timestamp, idPart))
}

// createUniqueLogFile atomically opens a unique log file within dir. If a file with filename already exists,
// it avoids overwriting by injecting an incrementing sequence number before the trailing request ID component.
func createUniqueLogFile(dir, filename string) (*os.File, string, error) {
	if filename == "" || strings.ContainsAny(filename, `/\`) || strings.Contains(filename, "..") || filename == "." || filepath.Base(filename) != filename {
		return nil, "", fmt.Errorf("invalid log filename")
	}
	ext := filepath.Ext(filename)
	base := strings.TrimSuffix(filename, ext)
	idx := strings.LastIndex(base, "-")
	prefix := base
	idPart := ""
	if idx > 0 {
		prefix = base[:idx]
		idPart = base[idx+1:]
	}

	target := filepath.Join(dir, filename)
	logFile, errOpen := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if errOpen == nil {
		return logFile, target, nil
	}
	if !os.IsExist(errOpen) {
		return nil, "", errOpen
	}

	for seq := 1; seq <= 1000; seq++ {
		candidateName := fmt.Sprintf("%s_%d-%s%s", prefix, seq, idPart, ext)
		candidatePath := filepath.Join(dir, candidateName)
		logCandidate, errCandidate := os.OpenFile(candidatePath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
		if errCandidate == nil {
			return logCandidate, candidatePath, nil
		}
		if !os.IsExist(errCandidate) {
			return nil, "", errCandidate
		}
	}
	return nil, "", fmt.Errorf("too many conflicting log files for %s", filename)
}

// sanitizeForFilename replaces characters that are not safe for filenames.
//
// Parameters:
//   - path: The path to sanitize
//
// Returns:
//   - string: A sanitized filename
func (l *FileRequestLogger) sanitizeForFilename(path string) string {
	// Replace slashes with hyphens
	sanitized := strings.NewReplacer("/", "-", "\\", "-").Replace(path)

	// Replace colons with hyphens
	sanitized = strings.ReplaceAll(sanitized, ":", "-")

	sanitized = sanitizeFilenameInvalidCharsPattern.ReplaceAllString(sanitized, "-")
	sanitized = strings.ReplaceAll(sanitized, "..", "-")
	sanitized = sanitizeFilenameMultiHyphenPattern.ReplaceAllString(sanitized, "-")
	sanitized = strings.Trim(sanitized, "-.")

	// Handle empty result
	if sanitized == "" {
		sanitized = "root"
	}

	return sanitized
}

// cleanupOldErrorLogs keeps only the newest errorLogsMaxFiles forced error log files.
func (l *FileRequestLogger) cleanupOldErrorLogs() error {
	if l.errorLogsMaxFiles <= 0 {
		return nil
	}

	entries, errRead := os.ReadDir(l.logsDir)
	if errRead != nil {
		return errRead
	}

	type logFile struct {
		name    string
		modTime time.Time
	}

	var files []logFile
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, "error-") || !(strings.HasSuffix(name, ".log") || strings.HasSuffix(name, ".log.gz")) {
			continue
		}
		info, errInfo := entry.Info()
		if errInfo != nil {
			log.WithError(errInfo).Warn("failed to read error log info")
			continue
		}
		files = append(files, logFile{name: name, modTime: info.ModTime()})
	}

	if len(files) <= l.errorLogsMaxFiles {
		return nil
	}

	sort.Slice(files, func(i, j int) bool {
		return files[i].modTime.After(files[j].modTime)
	})

	for _, file := range files[l.errorLogsMaxFiles:] {
		if errRemove := os.Remove(filepath.Join(l.logsDir, file.name)); errRemove != nil {
			log.WithError(errRemove).Warnf("failed to remove old error log: %s", file.name)
		}
	}

	return nil
}

func (l *FileRequestLogger) writeRequestBodyTempFile(body []byte) (string, error) {
	tmpFile, errCreate := os.CreateTemp(l.logsDir, "request-body-*.tmp")
	if errCreate != nil {
		return "", errCreate
	}
	tmpPath := tmpFile.Name()

	if _, errCopy := io.Copy(tmpFile, bytes.NewReader(body)); errCopy != nil {
		_ = tmpFile.Close()
		_ = os.Remove(tmpPath)
		return "", errCopy
	}
	if errClose := tmpFile.Close(); errClose != nil {
		_ = os.Remove(tmpPath)
		return "", errClose
	}
	return tmpPath, nil
}
