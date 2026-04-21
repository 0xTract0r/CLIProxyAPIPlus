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
	"time"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

const (
	oauthReauthHistoryDirName      = ".oauth-history"
	oauthReauthHistoryFileName     = "reauth.jsonl"
	defaultOAuthReauthHistoryLimit = 20
	maxOAuthReauthHistoryLimit     = 100
)

type oauthReauthFileSummary struct {
	FileSHA256    string    `json:"file_sha256,omitempty"`
	Size          int64     `json:"size,omitempty"`
	ModTime       time.Time `json:"modtime,omitempty"`
	Provider      string    `json:"provider,omitempty"`
	Email         string    `json:"email,omitempty"`
	Plan          string    `json:"plan,omitempty"`
	ProjectID     string    `json:"project_id,omitempty"`
	Label         string    `json:"label,omitempty"`
	AccountIDHash string    `json:"account_id_hash,omitempty"`
}

type oauthReauthHistoryEvent struct {
	EventType         string                  `json:"event_type"`
	OccurredAt        time.Time               `json:"occurred_at"`
	Provider          string                  `json:"provider,omitempty"`
	TargetAuthFile    string                  `json:"target_auth_file,omitempty"`
	OverwroteExisting bool                    `json:"overwrote_existing"`
	Before            *oauthReauthFileSummary `json:"before,omitempty"`
	After             *oauthReauthFileSummary `json:"after,omitempty"`
	Error             string                  `json:"error,omitempty"`
}

type oauthReauthHistoryListResponse struct {
	Events   []oauthReauthHistoryEvent `json:"events"`
	Limit    int                       `json:"limit"`
	AuthName string                    `json:"auth_name,omitempty"`
}

func oauthReauthHistoryPath(authDir string) string {
	authDir = strings.TrimSpace(authDir)
	if authDir == "" {
		return ""
	}
	return filepath.Join(authDir, oauthReauthHistoryDirName, oauthReauthHistoryFileName)
}

func normalizeOAuthReauthHistoryLimit(raw string) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return defaultOAuthReauthHistoryLimit, nil
	}
	limit, err := strconv.Atoi(raw)
	if err != nil {
		return 0, err
	}
	if limit <= 0 {
		return 0, errors.New("limit must be greater than 0")
	}
	if limit > maxOAuthReauthHistoryLimit {
		return maxOAuthReauthHistoryLimit, nil
	}
	return limit, nil
}

func readOAuthReauthHistoryEventsFromFile(path, authName string, limit int) ([]oauthReauthHistoryEvent, error) {
	path = strings.TrimSpace(path)
	authName = strings.TrimSpace(authName)
	if path == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = defaultOAuthReauthHistoryLimit
	}

	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return []oauthReauthHistoryEvent{}, nil
		}
		return nil, err
	}
	defer f.Close()

	events := make([]oauthReauthHistoryEvent, 0, limit)
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		var event oauthReauthHistoryEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			return nil, err
		}
		if authName != "" && !strings.EqualFold(strings.TrimSpace(event.TargetAuthFile), authName) {
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

func (h *Handler) ListOAuthReauthHistory(c *gin.Context) {
	if h == nil || h.cfg == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler not initialized"})
		return
	}

	authName := strings.TrimSpace(c.Query("auth_name"))
	if authName != "" && isUnsafeAuthFileName(authName) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid auth_name"})
		return
	}

	limit, err := normalizeOAuthReauthHistoryLimit(c.Query("limit"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid limit"})
		return
	}

	events, err := readOAuthReauthHistoryEventsFromFile(oauthReauthHistoryPath(h.cfg.AuthDir), authName, limit)
	if err != nil {
		log.WithError(err).Warn("management: read oauth reauth history failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read oauth reauth history"})
		return
	}

	c.JSON(http.StatusOK, oauthReauthHistoryListResponse{
		Events:   events,
		Limit:    limit,
		AuthName: authName,
	})
}
