package management

import (
	"bufio"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
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
	if provider := strings.TrimSpace(valueAsString(auth.Metadata["type"])); provider != "" {
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

func (h *Handler) appendAuthStatusHistoryEvent(event authStatusHistoryEvent) {
	if h == nil || h.cfg == nil {
		return
	}
	path := authStatusHistoryPath(h.cfg.AuthDir)
	if path == "" {
		return
	}

	event.EventType = strings.TrimSpace(event.EventType)
	if event.EventType == "" {
		return
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
	event.PreviousMessage = strings.TrimSpace(event.PreviousMessage)
	event.Status = strings.TrimSpace(event.Status)
	event.StatusMessage = strings.TrimSpace(event.StatusMessage)
	event.Error = strings.TrimSpace(event.Error)

	data, err := json.Marshal(event)
	if err != nil {
		log.WithError(err).Warn("management: marshal auth status history failed")
		return
	}

	authStatusHistoryWriteMu.Lock()
	defer authStatusHistoryWriteMu.Unlock()

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		log.WithError(err).Warn("management: create auth status history dir failed")
		return
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		log.WithError(err).Warn("management: open auth status history failed")
		return
	}
	defer f.Close()

	if _, err := f.Write(append(data, '\n')); err != nil {
		log.WithError(err).Warn("management: append auth status history failed")
	}
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

	f, err := os.Open(path)
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
	if h == nil || h.cfg == nil {
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

	events, err := readAuthStatusHistoryEventsFromFile(authStatusHistoryPath(h.cfg.AuthDir), authName, limit)
	if err != nil {
		log.WithError(err).Warn("management: read auth status history failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read auth status history"})
		return
	}

	c.JSON(http.StatusOK, authStatusHistoryListResponse{
		Events:   events,
		Limit:    limit,
		AuthName: authName,
	})
}
