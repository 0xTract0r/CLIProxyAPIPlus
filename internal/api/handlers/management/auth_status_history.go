package management

import (
	"bufio"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const (
	authStatusHistoryDirName      = ".auth-status-history"
	authStatusHistoryFileName     = "status.jsonl"
	defaultAuthStatusHistoryLimit = 20
	maxAuthStatusHistoryLimit     = 100

	authStatusHistoryTriggerManual = "manual"
	authStatusHistoryTriggerAuto   = "auto"
)

var (
	authStatusHistoryWriteMu sync.Mutex
	healthyStatusMessages    = map[string]struct{}{
		"ok":        {},
		"healthy":   {},
		"ready":     {},
		"success":   {},
		"available": {},
	}
)

type authStatusHistoryEvent struct {
	source          *os.Root
	EventType       string    `json:"event_type"`
	OccurredAt      time.Time `json:"occurred_at"`
	AuthName        string    `json:"auth_name,omitempty"`
	Provider        string    `json:"provider,omitempty"`
	Trigger         string    `json:"trigger,omitempty"`
	PreviousStatus  string    `json:"previous_status,omitempty"`
	PreviousMessage string    `json:"previous_message,omitempty"`
	Status          string    `json:"status,omitempty"`
	StatusMessage   string    `json:"status_message,omitempty"`
	Error           string    `json:"error,omitempty"`
}

type authStatusHistoryListResponse struct {
	Events   []authStatusHistoryEvent `json:"events"`
	Limit    int                      `json:"limit"`
	AuthName string                   `json:"auth_name,omitempty"`
}

type authStatusHistorySnapshot struct {
	AuthName      string
	Provider      string
	Status        string
	StatusMessage string
	Unavailable   bool
}

func authStatusHistoryPath(authDir string) string {
	authDir = strings.TrimSpace(authDir)
	if authDir == "" {
		return ""
	}
	return filepath.Join(authDir, authStatusHistoryDirName, authStatusHistoryFileName)
}

func normalizeAuthStatusHistoryLimit(raw string) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return defaultAuthStatusHistoryLimit, nil
	}
	limit, err := strconv.Atoi(raw)
	if err != nil {
		return 0, err
	}
	if limit <= 0 {
		return 0, errors.New("limit must be greater than 0")
	}
	if limit > maxAuthStatusHistoryLimit {
		return maxAuthStatusHistoryLimit, nil
	}
	return limit, nil
}

func normalizeAuthStatusHistoryTrigger(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case authStatusHistoryTriggerAuto:
		return authStatusHistoryTriggerAuto
	default:
		return authStatusHistoryTriggerManual
	}
}

func authStatusHistoryAuthName(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if name := strings.TrimSpace(auth.FileName); name != "" {
		return name
	}
	return strings.TrimSpace(auth.ID)
}

func authStatusHistoryProvider(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if provider := strings.TrimSpace(auth.Provider); provider != "" {
		return provider
	}
	return strings.TrimSpace(authAttribute(auth, "provider"))
}

func authStatusHistorySnapshotFromAuth(auth *coreauth.Auth) authStatusHistorySnapshot {
	if auth == nil {
		return authStatusHistorySnapshot{}
	}
	return authStatusHistorySnapshot{
		AuthName:      authStatusHistoryAuthName(auth),
		Provider:      authStatusHistoryProvider(auth),
		Status:        strings.TrimSpace(string(auth.Status)),
		StatusMessage: strings.TrimSpace(auth.StatusMessage),
		Unavailable:   auth.Unavailable,
	}
}

func authStatusHistoryHasWarning(snapshot authStatusHistorySnapshot) bool {
	if snapshot.Unavailable || strings.EqualFold(snapshot.Status, string(coreauth.StatusError)) {
		return true
	}
	message := strings.ToLower(strings.TrimSpace(snapshot.StatusMessage))
	if message == "" {
		return false
	}
	_, ok := healthyStatusMessages[message]
	return !ok
}

func deriveAuthStatusHistoryEventType(before, after authStatusHistorySnapshot, err error) string {
	beforeWarning := authStatusHistoryHasWarning(before)
	afterWarning := authStatusHistoryHasWarning(after)

	if err != nil {
		if afterWarning {
			return "warning"
		}
		return "check_failed"
	}
	if beforeWarning && !afterWarning {
		return "cleared"
	}
	if afterWarning {
		return "warning"
	}
	return "checked"
}

// openAuthStatusHistorySource captures the physical directory before the
// producer starts work. The producer closes the handle when it finishes.
func (h *Handler) openAuthStatusHistorySource() (*os.Root, error) {
	cfg := h.configSnapshot()
	if cfg == nil || strings.TrimSpace(cfg.AuthDir) == "" {
		return nil, errors.New("auth status history source directory is missing")
	}
	return os.OpenRoot(strings.TrimSpace(cfg.AuthDir))
}

// The source handle is captured by the producer when its operation starts.
func (h *Handler) appendAuthStatusHistoryEvent(event authStatusHistoryEvent) error {
	if err := appendAuthStatusHistoryAtSource(event); err != nil {
		log.Warn("management: auth status history was not saved")
		return err
	}
	return nil
}

func appendAuthStatusHistoryAtSource(event authStatusHistoryEvent) error {
	if event.source == nil {
		return errors.New("auth status history source directory is missing")
	}
	event.EventType = strings.TrimSpace(event.EventType)
	if event.EventType == "" {
		return errors.New("auth status history event type is missing")
	}
	if event.OccurredAt.IsZero() {
		event.OccurredAt = time.Now().UTC()
	} else {
		event.OccurredAt = event.OccurredAt.UTC()
	}
	event.AuthName = strings.TrimSpace(event.AuthName)
	event.Provider = strings.TrimSpace(event.Provider)
	event.Trigger = normalizeAuthStatusHistoryTrigger(event.Trigger)
	event.PreviousStatus = strings.TrimSpace(event.PreviousStatus)
	event.Status = strings.TrimSpace(event.Status)
	redactAuthStatusHistory(&event)

	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	authStatusHistoryWriteMu.Lock()
	defer authStatusHistoryWriteMu.Unlock()
	root := event.source
	if err := root.MkdirAll(authStatusHistoryDirName, 0o700); err != nil {
		return err
	}
	f, err := root.OpenFile(filepath.Join(authStatusHistoryDirName, authStatusHistoryFileName), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(append(data, '\n'))
	return errors.Join(writeErr, f.Close())
}

var auditHistorySecret = regexp.MustCompile(`(?i)(\b(?:access[_ -]?token|refresh[_ -]?token|id[_ -]?token|client[_ -]?secret|api[_ -]?key|password|authorization[_ -]?code|code|state)["']?\s*[:=]|\b(?:bearer|basic)\s+\S+|://[^/\s]+@)`)

func redactAuditHistoryText(s string) string {
	s = strings.TrimSpace(s)
	if auditHistorySecret.MatchString(s) {
		return "[redacted]"
	}
	return s
}

func redactAuthStatusHistory(event *authStatusHistoryEvent) {
	event.PreviousMessage = redactAuditHistoryText(event.PreviousMessage)
	event.StatusMessage = redactAuditHistoryText(event.StatusMessage)
	event.Error = redactAuditHistoryText(event.Error)
}

func openAuditHistory(path string) (*os.File, error) {
	parent := filepath.Dir(path)
	root, err := os.OpenRoot(filepath.Dir(parent))
	if err != nil {
		return nil, err
	}
	defer root.Close()
	name := filepath.Join(filepath.Base(parent), filepath.Base(path))
	info, err := root.Stat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("history is not a regular file")
	}
	return root.Open(name)
}

func readAuthStatusHistoryEventsFromFile(path, authName string, limit int) ([]authStatusHistoryEvent, error) {
	path = strings.TrimSpace(path)
	authName = strings.TrimSpace(authName)
	if path == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = defaultAuthStatusHistoryLimit
	}

	authStatusHistoryWriteMu.Lock()
	defer authStatusHistoryWriteMu.Unlock()
	f, err := openAuditHistory(path)
	if err != nil {
		if os.IsNotExist(err) {
			return []authStatusHistoryEvent{}, nil
		}
		return nil, err
	}
	defer f.Close()

	events := make([]authStatusHistoryEvent, 0, limit)
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		var event authStatusHistoryEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			return nil, err
		}
		if authName != "" && !strings.EqualFold(strings.TrimSpace(event.AuthName), authName) {
			continue
		}
		if !event.OccurredAt.IsZero() {
			event.OccurredAt = event.OccurredAt.UTC()
		}
		redactAuthStatusHistory(&event)
		events = append(events, event)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	for left, right := 0, len(events)-1; left < right; left, right = left+1, right-1 {
		events[left], events[right] = events[right], events[left]
	}
	if len(events) > limit {
		events = events[:limit]
	}
	return events, nil
}

func (h *Handler) ListAuthStatusHistory(c *gin.Context) {
	cfg := h.configSnapshot()
	if cfg == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler not initialized"})
		return
	}

	authName := strings.TrimSpace(c.Query("auth_name"))
	if authName != "" && isUnsafeAuthFileName(authName) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid auth_name"})
		return
	}

	limit, err := normalizeAuthStatusHistoryLimit(c.Query("limit"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid limit"})
		return
	}

	events, err := readAuthStatusHistoryEventsFromFile(authStatusHistoryPath(cfg.AuthDir), authName, limit)
	if err != nil {
		log.Warn("management: read auth status history failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read auth status history"})
		return
	}

	c.JSON(http.StatusOK, authStatusHistoryListResponse{
		Events:   events,
		Limit:    limit,
		AuthName: authName,
	})
}
