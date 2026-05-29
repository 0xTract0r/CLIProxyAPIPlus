package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	log "github.com/sirupsen/logrus"
)

const (
	feishuErrorAlertQueueSize    = 128
	feishuErrorAlertTimeout      = 3 * time.Second
	feishuErrorAlertMaxTextLen   = 3800
	feishuErrorAlertMaxValueLen  = 500
	feishuErrorAlertMaxFieldsLen = 24
)

var (
	globalFeishuErrorHookOnce sync.Once
	globalFeishuErrorHook     *feishuErrorHook

	alertBearerPattern = regexp.MustCompile(`(?i)Bearer\s+[A-Za-z0-9._~+/=-]{8,}`)
	alertSecretPattern = regexp.MustCompile(`(?i)"?(access_token|refresh_token|id_token|api_key|authorization|password|secret|webhook)"?\s*[=:]\s*["']?[^"'\s,}]+`)
	alertAPIKeyPattern = regexp.MustCompile(`sk-[A-Za-z0-9]{12,}`)
)

type feishuErrorHook struct {
	mu         sync.RWMutex
	webhookURL string
	client     *http.Client
	queue      chan feishuErrorAlertRequest
	done       chan struct{}
}

type feishuErrorAlertRequest struct {
	webhookURL string
	payload    feishuTextPayload
}

type feishuTextPayload struct {
	MsgType string            `json:"msg_type"`
	Content feishuTextContent `json:"content"`
}

type feishuTextContent struct {
	Text string `json:"text"`
}

// ConfigureErrorLogAlert installs and updates the global error-log alert hook.
// The hook is inert when feishu-webhook-url is empty.
func ConfigureErrorLogAlert(cfg config.ErrorLogAlertConfig) {
	SetupBaseLogger()
	globalFeishuErrorHookOnce.Do(func() {
		globalFeishuErrorHook = newFeishuErrorHook(nil)
		log.AddHook(globalFeishuErrorHook)
	})
	if globalFeishuErrorHook != nil {
		globalFeishuErrorHook.Configure(cfg)
	}
}

func newFeishuErrorHook(client *http.Client) *feishuErrorHook {
	if client == nil {
		client = &http.Client{}
	}
	h := &feishuErrorHook{
		client: client,
		queue:  make(chan feishuErrorAlertRequest, feishuErrorAlertQueueSize),
		done:   make(chan struct{}),
	}
	go h.run()
	return h
}

func (h *feishuErrorHook) Configure(cfg config.ErrorLogAlertConfig) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.webhookURL = strings.TrimSpace(cfg.FeishuWebhookURL)
	h.mu.Unlock()
}

func (h *feishuErrorHook) Levels() []log.Level {
	return []log.Level{log.PanicLevel, log.FatalLevel, log.ErrorLevel}
}

func (h *feishuErrorHook) Fire(entry *log.Entry) error {
	if h == nil || entry == nil {
		return nil
	}
	webhookURL := h.currentWebhookURL()
	if webhookURL == "" {
		return nil
	}
	payload := feishuTextPayload{
		MsgType: "text",
		Content: feishuTextContent{
			Text: buildFeishuErrorAlertText(entry),
		},
	}
	select {
	case h.queue <- feishuErrorAlertRequest{webhookURL: webhookURL, payload: payload}:
	default:
		// Drop instead of blocking the request path during error storms.
	}
	return nil
}

func (h *feishuErrorHook) currentWebhookURL() string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.webhookURL
}

func (h *feishuErrorHook) run() {
	defer close(h.done)
	for req := range h.queue {
		h.post(req)
	}
}

func (h *feishuErrorHook) post(req feishuErrorAlertRequest) {
	body, err := json.Marshal(req.payload)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), feishuErrorAlertTimeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, req.webhookURL, bytes.NewReader(body))
	if err != nil {
		log.WithError(err).Warn("feishu error log alert request build failed")
		return
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := h.client.Do(httpReq)
	if err != nil {
		log.WithError(err).Warn("feishu error log alert delivery failed")
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Warnf("feishu error log alert delivery failed: status=%d", resp.StatusCode)
	}
}

func (h *feishuErrorHook) close() {
	if h == nil {
		return
	}
	close(h.queue)
	<-h.done
}

func buildFeishuErrorAlertText(entry *log.Entry) string {
	timestamp := entry.Time
	if timestamp.IsZero() {
		timestamp = time.Now()
	}

	lines := []string{
		"[CLIProxyAPI] error log",
		fmt.Sprintf("level=%s", entry.Level.String()),
		fmt.Sprintf("time=%s", timestamp.Format(time.RFC3339)),
	}
	if hostname, err := os.Hostname(); err == nil && strings.TrimSpace(hostname) != "" {
		lines = append(lines, "host="+sanitizeAlertText(hostname, feishuErrorAlertMaxValueLen))
	}
	if entry.Caller != nil {
		lines = append(lines, fmt.Sprintf("caller=%s:%d", filepath.Base(entry.Caller.File), entry.Caller.Line))
	}
	if message := sanitizeAlertText(entry.Message, feishuErrorAlertMaxValueLen); message != "" {
		lines = append(lines, "message="+message)
	}
	if len(entry.Data) > 0 {
		keys := make([]string, 0, len(entry.Data))
		for key := range entry.Data {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		lines = append(lines, "fields:")
		for i, key := range keys {
			if i >= feishuErrorAlertMaxFieldsLen {
				lines = append(lines, fmt.Sprintf("- ... %d more field(s)", len(keys)-i))
				break
			}
			lines = append(lines, fmt.Sprintf("- %s=%s", key, sanitizeAlertField(key, entry.Data[key])))
		}
	}

	return truncateAlertText(strings.Join(lines, "\n"), feishuErrorAlertMaxTextLen)
}

func sanitizeAlertField(key string, value any) string {
	if isSensitiveAlertField(key) {
		return "<redacted>"
	}
	return sanitizeAlertText(fmt.Sprint(value), feishuErrorAlertMaxValueLen)
}

func isSensitiveAlertField(key string) bool {
	normalized := strings.ToLower(strings.NewReplacer("-", "_", ".", "_").Replace(strings.TrimSpace(key)))
	if normalized == "" {
		return false
	}
	sensitiveFragments := []string{
		"authorization",
		"access_token",
		"refresh_token",
		"id_token",
		"auth_token",
		"auth_id",
		"auth_file",
		"email",
		"api_key",
		"apikey",
		"secret",
		"password",
		"passwd",
		"cookie",
		"webhook",
	}
	for _, fragment := range sensitiveFragments {
		if strings.Contains(normalized, fragment) {
			return true
		}
	}
	return normalized == "token" || strings.HasSuffix(normalized, "_token") || strings.HasPrefix(normalized, "token_") || strings.Contains(normalized, "_token_")
}

func sanitizeAlertText(value string, maxLen int) string {
	value = strings.Join(strings.Fields(strings.TrimSpace(value)), " ")
	if value == "" {
		return ""
	}
	value = alertBearerPattern.ReplaceAllString(value, "Bearer <redacted>")
	value = alertSecretPattern.ReplaceAllString(value, "$1=<redacted>")
	value = alertAPIKeyPattern.ReplaceAllString(value, "sk-<redacted>")
	return truncateAlertText(value, maxLen)
}

func truncateAlertText(value string, maxLen int) string {
	if maxLen <= 0 {
		return value
	}
	runes := []rune(value)
	if len(runes) <= maxLen {
		return value
	}
	return string(runes[:maxLen]) + "...(truncated)"
}
