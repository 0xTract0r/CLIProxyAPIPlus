package management

// Fork-only management endpoint. It gives operators an API-driven way to set or
// clear an account's adaptive-scheduling overrides
// (account_scheduling.tier_override and account_scheduling.rate_scale) so tier
// differentiation / a per-account safety-test rate multiplier can be tuned at
// runtime instead of hand-editing the auth JSON on disk. Keep additions grouped
// here (like auth_files_anticorr.go) so they survive future upstream syncs.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// PatchAuthFileAccountScheduling sets or clears the operator-controlled
// adaptive-scheduling overrides on an account -- account_scheduling.tier_override,
// account_scheduling.rate_scale and account_scheduling.first_production_at
// (openspec add-adaptive-account-scheduling design §8.3/§8.4/§8.5).
//
// It mirrors PatchAuthFileAccountSettings (dedicated fork-only PATCH: locate by
// name/auth_index, mutate metadata, persist + make the running selector observe
// it via h.authManager.Update, then return the refreshed projection) but writes
// ONLY the scheduling override sub-keys, routed through coreauth's namespaced
// writers (SetAccountTierOverride / ClearAccountTierOverride /
// SetAccountRateScale / ClearAccountRateScale /
// SetAccountFirstProductionAt / ClearAccountFirstProductionAt) so they land in the
// top-level account_scheduling object that survives the ~45min quota refresh.
//
// Request body (application/json):
//
//	{
//	  "name": "<auth id | display name>",           // required
//	  "auth_index": "<optional stable index>",       // optional, for disambiguation
//	  "tier_override": "max_5x" | "" | null,         // present => set/clear; absent => untouched
//	  "rate_scale": 0.5 | "" | null,                  // present => set/clear; absent => untouched
//	  "first_production_at": "2026-01-02T15:04:05Z" | "" | null // present => set/clear; absent => untouched
//	}
//
// Field presence is what distinguishes intent (decoded via json.RawMessage so an
// absent field is never confused with an explicit null): at least one of
// tier_override / rate_scale / first_production_at must be present or the request
// is a 400. An explicit empty string or JSON null clears that override --
// clearing tier_override lets tier_source fall back to "auto" (derived on read by
// the projection), clearing rate_scale falls back to the config default (else
// 1.0), and clearing first_production_at re-opens the append-only auto-mint path
// (the next real serving success re-stamps a fresh anchor).
//
// Validation: tier_override must be a legal, provider-appropriate tier (claude:
// max_20x/max_5x/pro; codex: codex_pro/codex_plus) or a 400 with the legal set is
// returned; rate_scale must parse as a number > 0 or a 400 is returned;
// first_production_at must be a legal RFC3339 timestamp that is NOT in the future
// or a 400 is returned.
//
// first_production_at is the operator-explicit backfill channel (design §8.5),
// deliberately distinct from EnsureAuthFirstProductionAt's append-only auto-mint:
// it exists to migrate accounts that were already aged/in-production BEFORE
// adaptive scheduling was enabled (which otherwise get auto-minted as brand-new
// and clamped to the strictest warm-up stage) to their true first-production
// date. Only a future timestamp is rejected here; any past date is accepted and
// the operator owns its correctness. WARNING to operators: setting an anchor
// EARLIER than the truth makes the account look more mature than it is (less
// warm-up), which is the account-safety-risky direction -- only backfill dates
// you have actually confirmed for genuinely aged accounts.
//
// Auth: registered under the same /v0/management admin auth as its siblings (no
// new exemption).
func (h *Handler) PatchAuthFileAccountScheduling(c *gin.Context) {
	if h.authManager == nil {
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

	name, ok := decodeAccountSchedulingStringField(req, "name")
	if !ok || name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
		return
	}
	authIndex, _ := decodeAccountSchedulingStringField(req, "auth_index")

	tierRaw, tierPresent := req["tier_override"]
	rateRaw, ratePresent := req["rate_scale"]
	fpRaw, fpPresent := req["first_production_at"]
	if !tierPresent && !ratePresent && !fpPresent {
		c.JSON(http.StatusBadRequest, gin.H{"error": "at least one of tier_override, rate_scale or first_production_at is required"})
		return
	}

	// Locate the account the same way the sibling auth-file endpoints do:
	// id/filename (+ optional auth_index) via lookupAuthFile, falling back to the
	// display-name resolution used by the account-settings endpoint.
	targetAuth, _ := h.lookupAuthFile(name, authIndex)
	if targetAuth == nil {
		targetAuth = findAuthByName(h.authManager, name)
	}
	if targetAuth == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "auth file not found"})
		return
	}
	if coreauth.IsPluginVirtualAuth(targetAuth) {
		c.JSON(http.StatusConflict, gin.H{"error": errPluginVirtualAuth.Error()})
		return
	}

	if targetAuth.Metadata == nil {
		targetAuth.Metadata = make(map[string]any)
	}

	if tierPresent {
		value, errDecode := decodeAuthFileFieldValue(tierRaw)
		if errDecode != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid tier_override"})
			return
		}
		if isClearAccountSchedulingValue(value) {
			targetAuth.ClearAccountTierOverride()
		} else {
			str, isStr := value.(string)
			if !isStr {
				c.JSON(http.StatusBadRequest, gin.H{"error": "tier_override must be a string"})
				return
			}
			normalized, valid := coreauth.NormalizeTierOverride(targetAuth.Provider, str)
			if !valid {
				c.JSON(http.StatusBadRequest, gin.H{
					"error":        fmt.Sprintf("invalid tier_override %q for provider %q", strings.TrimSpace(str), strings.TrimSpace(targetAuth.Provider)),
					"legal_values": coreauth.LegalTierOverrideValues(targetAuth.Provider),
				})
				return
			}
			targetAuth.SetAccountTierOverride(normalized)
		}
	}

	if ratePresent {
		value, errDecode := decodeAuthFileFieldValue(rateRaw)
		if errDecode != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid rate_scale"})
			return
		}
		if isClearAccountSchedulingValue(value) {
			targetAuth.ClearAccountRateScale()
		} else {
			scale, valid := coreauth.ParseRateScaleValue(value)
			if !valid {
				c.JSON(http.StatusBadRequest, gin.H{"error": "rate_scale must be a number greater than 0"})
				return
			}
			targetAuth.SetAccountRateScale(scale)
		}
	}

	if fpPresent {
		value, errDecode := decodeAuthFileFieldValue(fpRaw)
		if errDecode != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid first_production_at"})
			return
		}
		if isClearAccountSchedulingValue(value) {
			// Empty string / JSON null -> clear the operator override, handing the
			// anchor back to the append-only auto-mint (ClearAccountFirstProductionAt).
			targetAuth.ClearAccountFirstProductionAt()
		} else {
			str, isStr := value.(string)
			if !isStr {
				c.JSON(http.StatusBadRequest, gin.H{"error": "first_production_at must be an RFC3339 timestamp string"})
				return
			}
			parsed, errParse := time.Parse(time.RFC3339, strings.TrimSpace(str))
			if errParse != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "first_production_at must be a valid RFC3339 timestamp"})
				return
			}
			// Fail-safe direction (design §8.5): a future anchor is meaningless
			// (an account cannot be younger than "now") and is rejected. Any past
			// date is accepted -- the operator owns backfilling only confirmed,
			// genuinely-aged accounts (setting it too early = artificially mature).
			if parsed.After(time.Now()) {
				c.JSON(http.StatusBadRequest, gin.H{"error": "first_production_at must not be in the future"})
				return
			}
			targetAuth.SetAccountFirstProductionAt(parsed)
		}
	}

	targetAuth.UpdatedAt = time.Now()
	if _, err := h.authManager.Update(c.Request.Context(), targetAuth); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to update auth: %v", err)})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"name":               authDisplayName(targetAuth),
		"account_scheduling": h.buildAccountSchedulingView(targetAuth),
	})
}

// decodeAccountSchedulingStringField unmarshals a string field from the raw
// request map, returning ok=false when the field is absent or not a JSON string.
// The returned value is whitespace-trimmed.
func decodeAccountSchedulingStringField(req map[string]json.RawMessage, key string) (string, bool) {
	raw, ok := req[key]
	if !ok {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return strings.TrimSpace(s), true
}

// isClearAccountSchedulingValue reports whether a decoded override value carries
// the explicit "clear this override" intent: a JSON null (decoded to nil) or a
// blank/whitespace-only string. Every other value is treated as a set request.
func isClearAccountSchedulingValue(value any) bool {
	if value == nil {
		return true
	}
	if s, ok := value.(string); ok {
		return strings.TrimSpace(s) == ""
	}
	return false
}
