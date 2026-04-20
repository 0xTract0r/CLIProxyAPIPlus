package management

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
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
	oauthReauthHistoryDirName      = ".oauth-history"
	oauthReauthHistoryFileName     = "reauth.jsonl"
	defaultOAuthReauthHistoryLimit = 20
	maxOAuthReauthHistoryLimit     = 100
)

var oauthReauthHistoryWriteMu sync.Mutex

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

func oauthReauthHistorySummaryFromAuth(auth *coreauth.Auth, authDir string) *oauthReauthFileSummary {
	if auth == nil {
		return nil
	}
	return oauthReauthHistorySummaryFromFile(
		resolveAuthFilePath(auth, authDir),
		auth.Metadata,
		auth.Provider,
		auth.Label,
	)
}

func oauthReauthHistorySummaryFromFile(path string, metadataHint map[string]any, providerHint, labelHint string) *oauthReauthFileSummary {
	summary := &oauthReauthFileSummary{
		Provider: strings.TrimSpace(providerHint),
		Label:    strings.TrimSpace(labelHint),
	}

	metadata := metadataHint
	if strings.TrimSpace(path) != "" {
		if info, err := os.Stat(path); err == nil {
			summary.Size = info.Size()
			summary.ModTime = info.ModTime().UTC()
		}
		if data, err := os.ReadFile(path); err == nil {
			sum := sha256.Sum256(data)
			summary.FileSHA256 = hex.EncodeToString(sum[:])[:16]

			var fileMetadata map[string]any
			if err := json.Unmarshal(data, &fileMetadata); err == nil {
				metadata = fileMetadata
			}
		}
	}

	if metadata != nil {
		if provider := strings.TrimSpace(valueAsString(metadata["type"])); provider != "" {
			summary.Provider = provider
		}
		if email := oauthReauthMetadataString(metadata, "email"); email != "" {
			summary.Email = email
		}
		if plan := oauthReauthMetadataString(metadata, "plan_type", "chatgpt_plan_type", "plan", "subscription_plan"); plan != "" {
			summary.Plan = plan
		}
		if projectID := oauthReauthMetadataString(metadata, "project_id"); projectID != "" {
			summary.ProjectID = projectID
		}
		if label := oauthReauthMetadataString(metadata, "label"); label != "" {
			summary.Label = label
		}
		if accountID := oauthReauthMetadataString(metadata, "account_id", "chatgpt_account_id", "organization_id"); accountID != "" {
			summary.AccountIDHash = oauthReauthDigest(accountID)
		}
	}

	if oauthReauthHistorySummaryEmpty(summary) {
		return nil
	}
	return summary
}

func oauthReauthMetadataString(metadata map[string]any, keys ...string) string {
	if len(metadata) == 0 {
		return ""
	}
	for _, key := range keys {
		if value := strings.TrimSpace(valueAsString(metadata[key])); value != "" {
			return value
		}
	}
	return ""
}

func oauthReauthDigest(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:16]
}

func oauthReauthHistorySummaryEmpty(summary *oauthReauthFileSummary) bool {
	if summary == nil {
		return true
	}
	return summary.FileSHA256 == "" &&
		summary.Size == 0 &&
		summary.ModTime.IsZero() &&
		summary.Provider == "" &&
		summary.Email == "" &&
		summary.Plan == "" &&
		summary.ProjectID == "" &&
		summary.Label == "" &&
		summary.AccountIDHash == ""
}

func resolveAuthFilePath(auth *coreauth.Auth, authDir string) string {
	if auth == nil {
		return ""
	}
	if path := strings.TrimSpace(authAttribute(auth, "path")); path != "" {
		return path
	}
	if fileName := strings.TrimSpace(auth.FileName); fileName != "" {
		if filepath.IsAbs(fileName) || strings.TrimSpace(authDir) == "" {
			return fileName
		}
		return filepath.Join(authDir, fileName)
	}
	if id := strings.TrimSpace(auth.ID); id != "" {
		if filepath.IsAbs(id) || strings.TrimSpace(authDir) == "" {
			return id
		}
		return filepath.Join(authDir, id)
	}
	return ""
}

func (h *Handler) appendOAuthReauthHistoryEvent(event oauthReauthHistoryEvent) {
	if h == nil || h.cfg == nil {
		return
	}
	path := oauthReauthHistoryPath(h.cfg.AuthDir)
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
	event.Provider = strings.TrimSpace(event.Provider)
	event.TargetAuthFile = strings.TrimSpace(event.TargetAuthFile)
	event.Error = strings.TrimSpace(event.Error)

	data, err := json.Marshal(event)
	if err != nil {
		log.WithError(err).Warn("management: marshal oauth reauth history failed")
		return
	}

	oauthReauthHistoryWriteMu.Lock()
	defer oauthReauthHistoryWriteMu.Unlock()

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		log.WithError(err).Warn("management: create oauth reauth history dir failed")
		return
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		log.WithError(err).Warn("management: open oauth reauth history failed")
		return
	}
	defer f.Close()

	if _, err := f.Write(append(data, '\n')); err != nil {
		log.WithError(err).Warn("management: append oauth reauth history failed")
	}
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
