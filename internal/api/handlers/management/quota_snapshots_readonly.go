package management

import (
	"encoding/json"
	"math"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type storedQuotaEntry struct {
	AuthID          string         `json:"auth_id"`
	AuthIndex       string         `json:"auth_index,omitempty"`
	Name            string         `json:"name,omitempty"`
	Provider        string         `json:"provider"`
	Disabled        bool           `json:"disabled,omitempty"`
	Status          string         `json:"status"`
	Error           string         `json:"error,omitempty"`
	PlanType        string         `json:"plan_type,omitempty"`
	LastRefreshedAt *time.Time     `json:"last_refreshed_at,omitempty"`
	NextRefreshAt   *time.Time     `json:"next_refresh_at,omitempty"`
	Snapshot        map[string]any `json:"snapshot,omitempty"`
}

// GetStoredQuotaSnapshots returns display fields from loaded local snapshots.
func (h *Handler) GetStoredQuotaSnapshots(c *gin.Context) {
	h.mu.Lock()
	manager := h.authManager
	h.mu.Unlock()

	entries := make([]storedQuotaEntry, 0)
	if manager != nil {
		for _, item := range manager.List() {
			if item == nil {
				continue
			}
			provider := strings.ToLower(strings.TrimSpace(item.Provider))
			if provider == "codex" || provider == "claude" {
				entries = append(entries, makeStoredQuotaEntry(item, provider))
			}
		}
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].AuthID == entries[j].AuthID {
			return entries[i].AuthIndex < entries[j].AuthIndex
		}
		return entries[i].AuthID < entries[j].AuthID
	})
	c.JSON(http.StatusOK, gin.H{
		"generated_at": time.Now().UTC(),
		"policy":       gin.H{"enabled": false},
		"entries":      entries,
	})
}

func makeStoredQuotaEntry(item *coreauth.Auth, provider string) storedQuotaEntry {
	id := item.ID
	if filepath.IsAbs(id) {
		id = filepath.Base(id)
	}
	name := item.FileName
	if filepath.IsAbs(name) {
		name = filepath.Base(name)
	}
	status, _ := item.Metadata["quota_refresh_status"].(string)
	message := ""
	switch status {
	case "ok", "stale", "unsupported", "refresh_disabled":
	case "error":
		message = "Quota snapshot refresh failed"
	case "reauth_required":
		message = "Credential requires reauthentication"
	default:
		if item.RefreshDisabled() {
			status = "refresh_disabled"
		} else {
			status = "stale"
		}
	}
	plan, _ := item.Metadata["plan_type"].(string)
	return storedQuotaEntry{
		AuthID: id, AuthIndex: item.Index, Name: name, Provider: provider, Disabled: item.Disabled,
		Status: status, Error: message, PlanType: normalizeStoredQuotaPlan(plan),
		LastRefreshedAt: storedQuotaTime(item.Metadata["quota_last_refreshed_at"]),
		NextRefreshAt:   storedQuotaTime(item.Metadata["quota_next_refresh_after"]),
		Snapshot:        storedQuotaDisplaySnapshot(provider, storedQuotaObject(item.Metadata["quota_snapshot"])),
	}
}

func normalizeStoredQuotaPlan(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	switch value {
	case "free", "go", "plus", "pro", "max", "team", "business", "enterprise", "edu", "prolite", "pro_lite", "proplus", "pro_plus":
		return value
	}
	return ""
}

func storedQuotaTime(value any) *time.Time {
	t, ok := value.(time.Time)
	if !ok {
		raw, _ := value.(string)
		var err error
		t, err = time.Parse(time.RFC3339Nano, strings.TrimSpace(raw))
		if err != nil {
			return nil
		}
	}
	if t.Year() <= 1 {
		return nil
	}
	t = t.UTC()
	return &t
}

func storedQuotaObject(value any) map[string]any {
	data, _ := value.(map[string]any)
	return data
}

func storedQuotaDisplaySnapshot(provider string, raw map[string]any) map[string]any {
	out := make(map[string]any)
	usage := storedQuotaObject(raw["usage"])
	if provider == "codex" {
		if display := storedCodexQuota(usage); len(display) > 0 {
			out["usage"] = display
		}
		return out
	}
	if display := storedClaudeQuota(usage); len(display) > 0 {
		out["usage"] = display
	}
	if profile := storedClaudeQuotaProfile(storedQuotaObject(raw["profile"])); len(profile) > 0 {
		out["profile"] = profile
	}
	return out
}

func storedCodexQuota(usage map[string]any) map[string]any {
	out := make(map[string]any)
	for _, key := range []string{"plan_type", "planType"} {
		if raw, ok := usage[key].(string); ok {
			if plan := normalizeStoredQuotaPlan(raw); plan != "" {
				out[key] = plan
			}
		}
	}
	for _, key := range []string{"rate_limit", "rateLimit", "code_review_rate_limit", "codeReviewRateLimit"} {
		if limit := storedCodexQuotaLimit(storedQuotaObject(usage[key])); len(limit) > 0 {
			out[key] = limit
		}
	}
	for _, key := range []string{"additional_rate_limits", "additionalRateLimits"} {
		items, _ := usage[key].([]any)
		var limits []map[string]any
		for _, node := range items {
			item := storedQuotaObject(node)
			display := make(map[string]any)
			for _, field := range []string{"rate_limit", "rateLimit"} {
				if limit := storedCodexQuotaLimit(storedQuotaObject(item[field])); len(limit) > 0 {
					display[field] = limit
				}
			}
			if len(display) > 0 {
				copyStoredQuotaFields(display, item, storedQuotaMeterName, "limit_name", "limitName", "metered_feature", "meteredFeature")
				limits = append(limits, display)
			}
		}
		if len(limits) > 0 {
			out[key] = limits
		}
	}
	credits := storedQuotaObject(usage["credits"])
	display := make(map[string]any)
	copyStoredQuotaFields(display, credits, storedQuotaBool, "has_credits", "hasCredits", "unlimited")
	copyStoredQuotaFields(display, credits, storedQuotaNumber, "balance")
	if len(display) > 0 {
		out["credits"] = display
	}
	for _, key := range []string{"rate_limit_reset_credits", "rateLimitResetCredits"} {
		display := make(map[string]any)
		copyStoredQuotaFields(display, storedQuotaObject(usage[key]), storedQuotaNumber, "available_count", "availableCount")
		if len(display) > 0 {
			out[key] = display
		}
	}
	for _, key := range []string{"subscription_active_until", "subscriptionActiveUntil"} {
		if ts := storedQuotaTime(usage[key]); ts != nil {
			out[key] = ts
		} else if number := storedQuotaNumber(usage[key]); number != nil {
			out[key] = number
		}
	}
	return out
}

func storedCodexQuotaLimit(limit map[string]any) map[string]any {
	out := make(map[string]any)
	copyStoredQuotaFields(out, limit, storedQuotaBool, "allowed", "limit_reached", "limitReached")
	for _, key := range []string{"primary_window", "primaryWindow", "secondary_window", "secondaryWindow"} {
		window := make(map[string]any)
		copyStoredQuotaFields(window, storedQuotaObject(limit[key]), storedQuotaNumber,
			"used_percent", "usedPercent", "limit_window_seconds", "limitWindowSeconds", "reset_after_seconds", "resetAfterSeconds", "reset_at", "resetAt")
		if len(window) > 0 {
			out[key] = window
		}
	}
	return out
}

func storedClaudeQuota(usage map[string]any) map[string]any {
	out := make(map[string]any)
	for _, key := range []string{"five_hour", "seven_day", "seven_day_oauth_apps", "seven_day_opus", "seven_day_sonnet", "seven_day_cowork", "iguana_necktie"} {
		raw := storedQuotaObject(usage[key])
		if utilization := storedQuotaNumber(raw["utilization"]); utilization != nil {
			display := map[string]any{"utilization": utilization}
			if ts := storedQuotaTime(raw["resets_at"]); ts != nil {
				display["resets_at"] = ts
			}
			out[key] = display
		}
	}
	extra := storedQuotaObject(usage["extra_usage"])
	display := make(map[string]any)
	copyStoredQuotaFields(display, extra, storedQuotaBool, "is_enabled")
	copyStoredQuotaFields(display, extra, storedQuotaNumber, "monthly_limit", "used_credits", "utilization")
	if len(display) > 0 {
		out["extra_usage"] = display
	}
	return out
}

func storedClaudeQuotaProfile(profile map[string]any) map[string]any {
	out := make(map[string]any)
	account := make(map[string]any)
	copyStoredQuotaFields(account, storedQuotaObject(profile["account"]), storedQuotaBool, "has_claude_max", "has_claude_pro")
	if len(account) > 0 {
		out["account"] = account
	}
	org := storedQuotaObject(profile["organization"])
	organization, _ := org["organization_type"].(string)
	status, _ := org["subscription_status"].(string)
	if strings.EqualFold(organization, "claude_team") && strings.EqualFold(status, "active") {
		out["organization"] = map[string]string{"organization_type": "claude_team", "subscription_status": "active"}
	}
	return out
}

func copyStoredQuotaFields(out, source map[string]any, normalize func(any) any, keys ...string) {
	for _, key := range keys {
		if value := normalize(source[key]); value != nil {
			out[key] = value
		}
	}
}

func storedQuotaNumber(value any) any {
	var raw string
	switch n := value.(type) {
	case int, int32, int64, uint, uint32, uint64:
		return n
	case float64:
		if !math.IsNaN(n) && !math.IsInf(n, 0) {
			return n
		}
	case json.Number:
		raw = n.String()
	case string:
		raw = strings.TrimSpace(n)
	}
	if number, err := strconv.ParseFloat(raw, 64); err == nil && !math.IsNaN(number) && !math.IsInf(number, 0) {
		return raw
	}
	return nil
}

func storedQuotaBool(value any) any {
	if flag, ok := value.(bool); ok {
		return flag
	}
	switch strings.ToLower(strings.TrimSpace(fmtStoredQuotaScalar(value))) {
	case "true", "1", "yes", "y", "on":
		return true
	case "false", "0", "no", "n", "off":
		return false
	}
	return nil
}

func fmtStoredQuotaScalar(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'g', -1, 64)
	case json.Number:
		return v.String()
	}
	return ""
}

func storedQuotaMeterName(value any) any {
	name, _ := value.(string)
	name = strings.TrimSpace(name)
	switch strings.ToLower(name) {
	case "code review", "code review premium", "codex spark", "codex_spark", "gpt-5.3-codex-spark", "spark":
		return name
	}
	// Older panels use a generic label for an unnamed additional window.
	return nil
}
