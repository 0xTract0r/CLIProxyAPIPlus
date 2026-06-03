// Package executor: codex cyber_policy alert side channel.
//
// 处理 Codex `/v1/responses` 上游 SSE 行内 cyber_policy 错误事件：
// - 计数：通过 sdk/cliproxy/auth Manager 原子写入 Auth.CyberPolicyFlagCount / LastCyberPolicyAt
// - 日志：结构化 WARN 行（沿用 helps.LogWithRequestID 的 request_id 字段）
// - 告警：当 config.CyberPolicyAlertConfig.WebhookURL 非空时异步 POST
//
// 设计要点：
// - 异步 POST 失败仅记录 WARN 日志，不重试、不阻塞主请求。
// - 同一 ExecuteStream 调用内仅计数一次（error/response.failed 双事件去重在调用方）。
package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

// cyberPolicyWebhookTimeout caps the asynchronous outbound POST.
const cyberPolicyWebhookTimeout = 5 * time.Second

// cyberPolicyAlertPayload is the minimal JSON envelope dispatched to the webhook.
type cyberPolicyAlertPayload struct {
	Event      string `json:"event"`
	AuthID     string `json:"auth_id"`
	Provider   string `json:"provider"`
	Label      string `json:"label,omitempty"`
	Model      string `json:"model,omitempty"`
	RequestID  string `json:"request_id,omitempty"`
	Count      int    `json:"count"`
	DetectedAt string `json:"detected_at"`
}

// recordCyberPolicy increments the per-auth counter and dispatches the optional
// webhook asynchronously. It is safe to call when manager or auth is nil — in
// that case only logging happens. The returned bool reports whether the counter
// was successfully incremented (used in tests).
func (e *CodexExecutor) recordCyberPolicy(ctx context.Context, auth *cliproxyauth.Auth, model string) bool {
	var (
		authID   string
		provider string
		label    string
	)
	if auth != nil {
		authID = auth.ID
		provider = auth.Provider
		label = auth.Label
	}

	count := 0
	detectedAt := time.Now().UTC()
	var persistErr error
	if e.authManager != nil && authID != "" {
		newCount, ts, err := e.authManager.IncrementCyberPolicyCount(ctx, authID)
		if !ts.IsZero() {
			detectedAt = ts
		}
		count = newCount
		persistErr = err
	}

	logFields := log.Fields{
		"auth_id":  authID,
		"provider": provider,
		"label":    label,
		"code":     "cyber_policy",
		"model":    model,
		"count":    count,
	}
	if persistErr != nil {
		// 持久化失败：in-memory 计数已 bump 但重启后会丢失。升级到 ERROR 级，
		// 同时抑制 webhook 以避免下游 operator 看到一次只在内存生效的告警。
		helps.LogWithRequestID(ctx).WithFields(logFields).WithError(persistErr).
			Error("upstream cyber_policy flag: persist failed; webhook suppressed")
		return count > 0
	}
	helps.LogWithRequestID(ctx).WithFields(logFields).Warn("upstream cyber_policy flag")

	webhookURL := ""
	if e.cfg != nil {
		webhookURL = strings.TrimSpace(e.cfg.CyberPolicyAlert.WebhookURL)
	}
	if webhookURL != "" {
		payload := cyberPolicyAlertPayload{
			Event:      "cyber_policy",
			AuthID:     authID,
			Provider:   provider,
			Label:      label,
			Model:      model,
			RequestID:  cyberPolicyRequestID(ctx),
			Count:      count,
			DetectedAt: detectedAt.Format(time.RFC3339),
		}
		go dispatchCyberPolicyWebhook(webhookURL, payload)
	}
	return count > 0
}

// cyberPolicyRequestID best-effort extracts the request id from context using
// the same helper that LogWithRequestID uses internally.
func cyberPolicyRequestID(ctx context.Context) string {
	entry := helps.LogWithRequestID(ctx)
	if entry == nil {
		return ""
	}
	if v, ok := entry.Data["request_id"]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// dispatchCyberPolicyWebhook sends the payload using a short-timeout HTTP client.
// All failures are logged at WARN level and never returned to the caller.
func dispatchCyberPolicyWebhook(url string, payload cyberPolicyAlertPayload) {
	body, err := json.Marshal(payload)
	if err != nil {
		log.WithError(err).Warn("codex executor: cyber_policy webhook payload encode failed")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), cyberPolicyWebhookTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		log.WithError(err).Warn("codex executor: cyber_policy webhook request build failed")
		return
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: cyberPolicyWebhookTimeout}
	resp, err := client.Do(req)
	if err != nil {
		log.WithError(err).WithField("webhook_url", url).Warn("codex executor: cyber_policy webhook dispatch failed")
		return
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	if resp.StatusCode >= 400 {
		log.WithFields(log.Fields{
			"webhook_url": url,
			"status":      resp.StatusCode,
		}).Warn("codex executor: cyber_policy webhook returned non-2xx status")
	}
}

// cyberPolicyHitFromData reports whether the given SSE data payload represents
// a cyber_policy error event. It checks both the top-level error.code/code
// (for `type=error` events) and response.error.code (for `type=response.failed`).
func cyberPolicyHitFromData(data []byte, eventType string) bool {
	switch eventType {
	case "error":
		return cyberPolicyCodeMatches(data, "error.code") || cyberPolicyCodeMatches(data, "code")
	case "response.failed":
		return cyberPolicyCodeMatches(data, "response.error.code")
	}
	return false
}

// cyberPolicyCodeMatches reports whether the JSON field at path equals "cyber_policy".
func cyberPolicyCodeMatches(data []byte, path string) bool {
	return gjson.GetBytes(data, path).String() == "cyber_policy"
}
