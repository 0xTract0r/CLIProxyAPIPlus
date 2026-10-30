package management

import (
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func applyAccountSchedulingPatch(a *coreauth.Auth, cfg config.AccountSchedulingConfig, req map[string]json.RawMessage, now time.Time) error {
	if providerKey(a) != "claude" || coreauth.IsPluginVirtualAuth(a) {
		return errors.New("account scheduling target is not editable")
	}
	if err := coreauth.ApplyAccountWarmupControlPatch(a, req); err != nil {
		return err
	}
	for _, key := range []string{"tier_override", "rate_scale", "first_production_at"} {
		raw, present := req[key]
		if !present {
			continue
		}
		value, err := decodeAuthFileFieldValue(raw)
		if err != nil {
			return errors.New("invalid scheduling field")
		}
		clear := isClearAccountSchedulingValue(value)
		switch key {
		case "tier_override":
			if clear {
				a.ClearAccountTierOverride()
				continue
			}
			v, ok := value.(string)
			if !ok {
				return errors.New("tier_override must be a string")
			}
			normalized, ok := coreauth.NormalizeTierOverride(a.Provider, v)
			if !ok {
				return errors.New("invalid tier_override for provider")
			}
			a.SetAccountTierOverride(normalized)
		case "rate_scale":
			if clear {
				a.ClearAccountRateScale()
				continue
			}
			v, ok := coreauth.ParseRateScaleValue(value)
			if !ok {
				return errors.New("rate_scale must be a finite number greater than 0")
			}
			a.SetAccountRateScale(v)
		case "first_production_at":
			if clear {
				a.ClearAccountFirstProductionAt()
				continue
			}
			v, ok := value.(string)
			if !ok {
				return errors.New("first_production_at must be an RFC3339 timestamp string")
			}
			at, err := time.Parse(time.RFC3339, strings.TrimSpace(v))
			if err != nil {
				return errors.New("first_production_at must be a valid RFC3339 timestamp")
			}
			if at.After(now) {
				return errors.New("first_production_at must not be in the future")
			}
			a.SetAccountFirstProductionAt(at)
		}
	}
	return coreauth.ApplyAccountLimitPatch(a, cfg, req["limit_overrides"], req["temporary_limits"], now)
}

// GetAuthFileWarmupPacing is a narrow read: no header synchronization, OAuth
// refresh, upstream probe, policy refresh, reservation or persistence.
func (h *Handler) GetAuthFileWarmupPacing(c *gin.Context) {
	if h == nil || h.authManager == nil {
		c.JSON(503, gin.H{"error": "core auth manager unavailable"})
		return
	}
	name, index := strings.TrimSpace(c.Query("name")), strings.TrimSpace(c.Query("auth_index"))
	if name == "" || index == "" {
		c.JSON(400, gin.H{"error": "name and auth_index are required"})
		return
	}
	a, _ := h.lookupAuthFile(name, index)
	if a == nil || providerKey(a) != "claude" {
		c.JSON(404, gin.H{"error": "auth file not found"})
		return
	}
	out := gin.H{"name": authDisplayName(a), "auth_index": a.Index, "disabled": a.Disabled, "status": a.Status, "unavailable": a.Unavailable, "account_scheduling": h.buildAccountSchedulingView(a)}
	if a.Disabled {
		out["availability_reason"] = "disabled"
	} else if a.AutoQuarantined {
		out["availability_reason"] = "reauth_required"
	} else if a.Unavailable {
		out["availability_reason"] = "unavailable"
	}
	c.JSON(200, out)
}
