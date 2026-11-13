package executor

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

const defaultUsageLimitRetryAfter = time.Hour

var retryAtPattern = regexp.MustCompile(`(?i)(?:try again at|again after|resets? at)\s+([0-9]{1,2}(?::[0-9]{2})?\s*(?:am|pm)?)`)

func parseUsageLimitRetryAfter(statusCode int, body []byte, headers http.Header, now time.Time) *time.Duration {
	if statusCode != http.StatusTooManyRequests || len(body) == 0 {
		return nil
	}
	if now.IsZero() {
		now = time.Now()
	}
	if !isUsageLimitBody(body) {
		return nil
	}
	if duration := retryAfterFromStructuredBody(body, now); duration != nil {
		return duration
	}
	if duration := retryAfterFromHeader(headers, now); duration != nil {
		return duration
	}
	if duration := retryAfterFromNaturalLanguage(body, now); duration != nil {
		return duration
	}
	retryAfter := defaultUsageLimitRetryAfter
	return &retryAfter
}

func isUsageLimitBody(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	candidates := []string{
		gjson.GetBytes(body, "error.type").String(),
		gjson.GetBytes(body, "error.code").String(),
		gjson.GetBytes(body, "error.message").String(),
		gjson.GetBytes(body, "type").String(),
		gjson.GetBytes(body, "code").String(),
		gjson.GetBytes(body, "message").String(),
		string(body),
	}
	for _, candidate := range candidates {
		lower := strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(candidate)), " "))
		if lower == "" {
			continue
		}
		switch {
		case strings.Contains(lower, "usage_limit_reached"):
			return true
		case strings.Contains(lower, "usage limit"):
			return true
		case strings.Contains(lower, "purchase more credits"):
			return true
		case strings.Contains(lower, "upgrade to pro") && strings.Contains(lower, "try again"):
			return true
		case strings.Contains(lower, "quota") && strings.Contains(lower, "exceeded"):
			return true
		}
	}
	return false
}

func retryAfterFromStructuredBody(body []byte, now time.Time) *time.Duration {
	for _, key := range []string{"error.resets_at", "resets_at", "reset_at"} {
		if resetsAt := gjson.GetBytes(body, key).Int(); resetsAt > 0 {
			resetAtTime := time.Unix(resetsAt, 0)
			if resetAtTime.After(now) {
				retryAfter := resetAtTime.Sub(now)
				return &retryAfter
			}
		}
	}
	for _, key := range []string{"error.resets_in_seconds", "resets_in_seconds", "retry_after_seconds"} {
		if resetsInSeconds := gjson.GetBytes(body, key).Int(); resetsInSeconds > 0 {
			retryAfter := time.Duration(resetsInSeconds) * time.Second
			return &retryAfter
		}
	}
	return nil
}

func retryAfterFromHeader(headers http.Header, now time.Time) *time.Duration {
	if headers == nil {
		return nil
	}
	raw := strings.TrimSpace(headers.Get("Retry-After"))
	if raw == "" {
		return nil
	}
	if seconds, err := strconv.ParseInt(raw, 10, 64); err == nil && seconds > 0 {
		retryAfter := time.Duration(seconds) * time.Second
		return &retryAfter
	}
	if resetAt, err := http.ParseTime(raw); err == nil && resetAt.After(now) {
		retryAfter := resetAt.Sub(now)
		return &retryAfter
	}
	return nil
}

func retryAfterFromNaturalLanguage(body []byte, now time.Time) *time.Duration {
	text := strings.Join(strings.Fields(string(body)), " ")
	match := retryAtPattern.FindStringSubmatch(text)
	if len(match) < 2 {
		return nil
	}
	rawTime := strings.ToUpper(strings.Join(strings.Fields(match[1]), ""))
	layouts := []string{"3:04PM", "3PM", "15:04", "15"}
	for _, layout := range layouts {
		parsed, err := time.ParseInLocation(layout, rawTime, now.Location())
		if err != nil {
			continue
		}
		resetAt := time.Date(now.Year(), now.Month(), now.Day(), parsed.Hour(), parsed.Minute(), 0, 0, now.Location())
		if !resetAt.After(now) {
			resetAt = resetAt.Add(24 * time.Hour)
		}
		retryAfter := resetAt.Sub(now)
		return &retryAfter
	}
	return nil
}

// codexRateLimitRetryAfterFromHeaders derives a retry-after window from Codex's
// rate-limit response headers when a primary/secondary window is fully consumed
// (used-percent >= 100). Codex surfaces sustained 5h/weekly plan limits via these
// headers (and a body type like "rate_limit_reached") that isUsageLimitBody does
// not match, so the 429 path would otherwise fall back to the 1-minute transient
// cooldown and keep re-selecting an exhausted credential. Returns nil when no
// window is exhausted (e.g. TPM bursts / model capacity), preserving fast
// transient recovery.
//
// Relation to the success path (quotaRetryAfterFromHeaders in
// sdk/cliproxy/auth/conductor_fork_ext.go): the header parsing is the same, but the
// credits handling is intentionally NOT symmetric. On a success response, usable
// credits (X-Codex-Credits-*) always exempt the main X-Codex buckets, because the
// upstream just proved it is serving the account from credits. On a 429 the
// caller (newCodexStatusErr) exempts them only when the response carries no
// explicit limit signal (codexExplicitLimitSignal: a usage-limit body, an
// error.type "rate_limit_reached" body, or an X-Codex-Rate-Limit-Reached-Type
// header). Such a signal means the upstream refused the credit path for this
// account, so it must still cool to the window reset (the backstop for optimistic
// credit headers), while a capacity / TPM 429 on a credit-backed account is
// transient and must not park it for days. The X-Codex-Bengalfox
// buckets are never exempted on either path.
func codexRateLimitRetryAfterFromHeaders(headers http.Header, now time.Time) *time.Duration {
	return codexRateLimitRetryAfterForPrefixes(headers, now, codexRateLimitHeaderPrefixes...)
}

// codexRateLimitHeaderPrefixes are the Codex quota header families: the main
// X-Codex buckets (which account credits cover) and the X-Codex-Bengalfox buckets
// (which credits are not known to cover).
var codexRateLimitHeaderPrefixes = []string{"X-Codex", "X-Codex-Bengalfox"}

// codexRateLimitRetryAfterForPrefixes is codexRateLimitRetryAfterFromHeaders
// restricted to the given header prefixes.
func codexRateLimitRetryAfterForPrefixes(headers http.Header, now time.Time, prefixes ...string) *time.Duration {
	if len(headers) == 0 {
		return nil
	}
	if now.IsZero() {
		now = time.Now()
	}
	var latest time.Time
	for _, prefix := range prefixes {
		for _, window := range []string{"Primary", "Secondary"} {
			if !codexRateLimitPercentExhausted(headers.Get(prefix + "-" + window + "-Used-Percent")) {
				continue
			}
			if resetAt := codexRateLimitResetTime(headers, prefix+"-"+window, now); resetAt.After(latest) {
				latest = resetAt
			}
		}
	}
	if latest.IsZero() || !latest.After(now) {
		return nil
	}
	retryAfter := latest.Sub(now)
	return &retryAfter
}

func codexRateLimitPercentExhausted(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false
	}
	value, err := strconv.ParseFloat(raw, 64)
	return err == nil && value >= 100
}

func codexRateLimitResetTime(headers http.Header, prefix string, now time.Time) time.Time {
	if headers == nil {
		return time.Time{}
	}
	if raw := strings.TrimSpace(headers.Get(prefix + "-Reset-At")); raw != "" {
		if unixSeconds, err := strconv.ParseInt(raw, 10, 64); err == nil && unixSeconds > 0 {
			if resetAt := time.Unix(unixSeconds, 0); resetAt.After(now) {
				return resetAt
			}
		}
	}
	if raw := strings.TrimSpace(headers.Get(prefix + "-Reset-After-Seconds")); raw != "" {
		if seconds, err := strconv.ParseInt(raw, 10, 64); err == nil && seconds > 0 {
			return now.Add(time.Duration(seconds) * time.Second)
		}
	}
	return time.Time{}
}
