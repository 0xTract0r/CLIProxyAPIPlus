package management

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// PreviewAuthFileAccountScheduling validates and projects a detached policy.
// It deliberately never uses UpdateAccountScheduling or a live runtime handler:
// no save, auth publication, refresh, admission, ledger or session access occurs.
func (h *Handler) PreviewAuthFileAccountScheduling(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}
	var req map[string]json.RawMessage
	decoder := json.NewDecoder(c.Request.Body)
	decoder.UseNumber()
	if err := decoder.Decode(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	name, nameOK := decodeAccountSchedulingStringField(req, "name")
	index, indexOK := decodeAccountSchedulingStringField(req, "auth_index")
	if !nameOK || name == "" || !indexOK || index == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name and auth_index are required"})
		return
	}

	// Match a single current record and clone config without assigning a reload
	// generation. lookupAuthFile never falls back to display name with an index.
	h.mu.Lock()
	if h.authManager == nil {
		h.mu.Unlock()
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}
	current, _ := h.lookupAuthFile(name, index)
	var cfg *config.Config
	if h.cfg != nil {
		cfg = h.cfg.CloneForRuntime()
	} else {
		cfg = &config.Config{}
	}
	h.mu.Unlock()
	if current == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "auth file not found"})
		return
	}
	if coreauth.IsPluginVirtualAuth(current) {
		c.JSON(http.StatusConflict, gin.H{"error": errPluginVirtualAuth.Error()})
		return
	}
	if coreauth.IsConfigAPIKeyAuth(current) {
		c.JSON(http.StatusConflict, gin.H{"error": "static configuration accounts do not support persistent limit overrides", "reason": "static_config_account"})
		return
	}
	if providerKey(current) != "claude" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "account-scheduling controls apply to Claude accounts only"})
		return
	}

	now := time.Now()
	current = current.CloneForSchedulingPreview()
	candidate := current.CloneForSchedulingPreview()
	if err := applyAccountSchedulingPatch(candidate, cfg.AccountScheduling, req, now); err != nil {
		body := gin.H{"error": err.Error()}
		if err.Error() == "invalid tier_override for provider" {
			body["legal_values"] = coreauth.LegalTierOverrideValues(current.Provider)
		}
		c.JSON(http.StatusBadRequest, body)
		return
	}
	// The isolated handler reuses saved projection rules but cannot consult a
	// live ledger, sessions, hooks or schedulers. Only policy fields escape;
	// an uninitialized pacing placeholder is not a candidate balance promise.
	projector := &Handler{cfg: cfg}
	c.JSON(http.StatusOK, gin.H{
		"name":                       authDisplayName(current),
		"auth_index":                 current.Index,
		"preview":                    true,
		"account_scheduling":         schedulingPreviewPolicy(projector.buildAccountSchedulingViewAt(candidate, now)),
		"current_account_scheduling": schedulingPreviewPolicy(projector.buildAccountSchedulingViewAt(current, now)),
	})
}

func schedulingPreviewPolicy(view gin.H) gin.H {
	result := gin.H{}
	for _, key := range []string{
		"subscription_tier", "tier_source", "first_production_at", "rate_scale",
		"limit_policy", "warmup_control", "warmup", "effective_limits",
		"in_distress", "warmup_health_stage_cap", "warmup_last_distress_at",
	} {
		result[key] = view[key]
	}
	return result
}
