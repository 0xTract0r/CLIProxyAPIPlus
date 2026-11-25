package executor

import (
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

const claudeFastModeBeta = "fast-mode-2026-02-01"

func claudeRequestUsesFastMode(body []byte, requested map[string]bool) bool {
	return requested[claudeFastModeBeta] || strings.EqualFold(strings.TrimSpace(ojson.GetBytesNoCopy(body, "speed").String()), "fast")
}

func claudeRequestedBetas(incoming string, body []string) map[string]bool {
	result := make(map[string]bool)
	for _, value := range strings.Split(incoming, ",") {
		if value = strings.TrimSpace(value); value != "" {
			result[value] = true
		}
	}
	for _, value := range body {
		if value = strings.TrimSpace(value); value != "" {
			result[value] = true
		}
	}
	return result
}

func isAnthropicUpstreamBase(raw string) bool {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	return err == nil && helps.IsAnthropicUpstreamURL(parsed)
}

func claudeCreds(auth *cliproxyauth.Auth) (string, string) {
	key, _, _ := ordinaryClaudeCredential(auth)
	if auth == nil {
		return "", ""
	}
	return key, strings.TrimSpace(auth.Attributes["base_url"])
}

func isClaudeOAuthToken(key string) bool {
	return strings.HasPrefix(strings.TrimSpace(key), "sk-ant-oat")
}

func validateOrdinaryClaudeModel(doc *ojson.Doc, base string) error {
	if !isAnthropicUpstreamBase(base) || !util.IsClaudeLegacySystemReminderModel(doc.Get("model").String()) {
		return nil
	}
	for _, message := range doc.Get("messages").Array() {
		if message.Get("role").String() == "system" {
			return statusErr{code: http.StatusBadRequest, msg: "mid-conversation system messages are not supported on this Claude model", requestScoped: true}
		}
	}
	return nil
}

func ordinaryClaudeRetryAfter(headers http.Header, now time.Time) *time.Duration {
	var values []string
	for name, items := range headers {
		if strings.EqualFold(name, "Retry-After") {
			values = append(values, items...)
		}
	}
	if len(values) != 1 {
		return nil
	}
	text := strings.TrimSpace(values[0])
	if text == "" || strings.HasPrefix(text, "+") {
		return nil
	}
	return parseRetryAfterHeader(http.Header{"Retry-After": {text}}, now)
}
