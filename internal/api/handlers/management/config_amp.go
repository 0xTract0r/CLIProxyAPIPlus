package management

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// GetAmpCode returns the complete ampcode configuration.
func (h *Handler) GetAmpCode(c *gin.Context) {
	cfg := h.configSnapshot()
	if cfg == nil {
		c.JSON(200, gin.H{"ampcode": config.AmpCode{}})
		return
	}
	c.JSON(200, gin.H{"ampcode": cfg.AmpCode})
}

// GetAmpUpstreamURL returns the ampcode upstream URL.
func (h *Handler) GetAmpUpstreamURL(c *gin.Context) {
	cfg := h.configSnapshot()
	if cfg == nil {
		c.JSON(200, gin.H{"upstream-url": ""})
		return
	}
	c.JSON(200, gin.H{"upstream-url": cfg.AmpCode.UpstreamURL})
}

// PutAmpUpstreamURL updates the ampcode upstream URL.
func (h *Handler) PutAmpUpstreamURL(c *gin.Context) {
	updateAmpScalar(h, c, func(amp *config.AmpCode, v string) { amp.UpstreamURL = strings.TrimSpace(v) })
}

// DeleteAmpUpstreamURL clears the ampcode upstream URL.
func (h *Handler) DeleteAmpUpstreamURL(c *gin.Context) {
	h.updateAmpCode(c, func(amp *config.AmpCode) {
		amp.UpstreamURL = ""
	})
}

// GetAmpUpstreamAPIKey returns the ampcode upstream API key.
func (h *Handler) GetAmpUpstreamAPIKey(c *gin.Context) {
	cfg := h.configSnapshot()
	if cfg == nil {
		c.JSON(200, gin.H{"upstream-api-key": ""})
		return
	}
	c.JSON(200, gin.H{"upstream-api-key": cfg.AmpCode.UpstreamAPIKey})
}

// PutAmpUpstreamAPIKey updates the ampcode upstream API key.
func (h *Handler) PutAmpUpstreamAPIKey(c *gin.Context) {
	updateAmpScalar(h, c, func(amp *config.AmpCode, v string) { amp.UpstreamAPIKey = strings.TrimSpace(v) })
}

// DeleteAmpUpstreamAPIKey clears the ampcode upstream API key.
func (h *Handler) DeleteAmpUpstreamAPIKey(c *gin.Context) {
	h.updateAmpCode(c, func(amp *config.AmpCode) {
		amp.UpstreamAPIKey = ""
	})
}

// GetAmpRestrictManagementToLocalhost returns the localhost restriction setting.
func (h *Handler) GetAmpRestrictManagementToLocalhost(c *gin.Context) {
	cfg := h.configSnapshot()
	if cfg == nil {
		c.JSON(200, gin.H{"restrict-management-to-localhost": true})
		return
	}
	c.JSON(200, gin.H{"restrict-management-to-localhost": cfg.AmpCode.RestrictManagementToLocalhost})
}

// PutAmpRestrictManagementToLocalhost updates the localhost restriction setting.
func (h *Handler) PutAmpRestrictManagementToLocalhost(c *gin.Context) {
	updateAmpScalar(h, c, func(amp *config.AmpCode, v bool) { amp.RestrictManagementToLocalhost = v })
}

// GetAmpModelMappings returns the ampcode model mappings.
func (h *Handler) GetAmpModelMappings(c *gin.Context) {
	cfg := h.configSnapshot()
	if cfg == nil {
		c.JSON(200, gin.H{"model-mappings": []config.AmpModelMapping{}})
		return
	}
	c.JSON(200, gin.H{"model-mappings": cfg.AmpCode.ModelMappings})
}

// PutAmpModelMappings replaces all ampcode model mappings.
func (h *Handler) PutAmpModelMappings(c *gin.Context) {
	var body struct {
		Value []config.AmpModelMapping `json:"value"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(400, gin.H{"error": "invalid body"})
		return
	}
	h.updateAmpCode(c, func(amp *config.AmpCode) {
		amp.ModelMappings = body.Value
	})
}

// PatchAmpModelMappings adds or updates model mappings.
func (h *Handler) PatchAmpModelMappings(c *gin.Context) {
	var body struct {
		Value []config.AmpModelMapping `json:"value"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(400, gin.H{"error": "invalid body"})
		return
	}

	h.updateAmpCode(c, func(amp *config.AmpCode) {
		existing := make(map[string]int)
		for i, m := range amp.ModelMappings {
			existing[strings.TrimSpace(m.From)] = i
		}

		for _, newMapping := range body.Value {
			from := strings.TrimSpace(newMapping.From)
			if idx, ok := existing[from]; ok {
				amp.ModelMappings[idx] = newMapping
			} else {
				amp.ModelMappings = append(amp.ModelMappings, newMapping)
				existing[from] = len(amp.ModelMappings) - 1
			}
		}
	})
}

// DeleteAmpModelMappings removes specified model mappings by "from" field.
func (h *Handler) DeleteAmpModelMappings(c *gin.Context) {
	var body struct {
		Value []string `json:"value"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || len(body.Value) == 0 {
		h.updateAmpCode(c, func(amp *config.AmpCode) { amp.ModelMappings = nil })
		return
	}

	toRemove := make(map[string]bool)
	for _, from := range body.Value {
		toRemove[strings.TrimSpace(from)] = true
	}

	h.updateAmpCode(c, func(amp *config.AmpCode) {
		newMappings := make([]config.AmpModelMapping, 0, len(amp.ModelMappings))
		for _, m := range amp.ModelMappings {
			if !toRemove[strings.TrimSpace(m.From)] {
				newMappings = append(newMappings, m)
			}
		}
		amp.ModelMappings = newMappings
	})
}

// GetAmpForceModelMappings returns whether model mappings are forced.
func (h *Handler) GetAmpForceModelMappings(c *gin.Context) {
	cfg := h.configSnapshot()
	if cfg == nil {
		c.JSON(200, gin.H{"force-model-mappings": false})
		return
	}
	c.JSON(200, gin.H{"force-model-mappings": cfg.AmpCode.ForceModelMappings})
}

// PutAmpForceModelMappings updates the force model mappings setting.
func (h *Handler) PutAmpForceModelMappings(c *gin.Context) {
	updateAmpScalar(h, c, func(amp *config.AmpCode, v bool) { amp.ForceModelMappings = v })
}

// GetAmpUpstreamAPIKeys returns the ampcode upstream API keys mapping.
func (h *Handler) GetAmpUpstreamAPIKeys(c *gin.Context) {
	cfg := h.configSnapshot()
	if cfg == nil {
		c.JSON(200, gin.H{"upstream-api-keys": []config.AmpUpstreamAPIKeyEntry{}})
		return
	}
	c.JSON(200, gin.H{"upstream-api-keys": cfg.AmpCode.UpstreamAPIKeys})
}

// PutAmpUpstreamAPIKeys replaces all ampcode upstream API keys mappings.
func (h *Handler) PutAmpUpstreamAPIKeys(c *gin.Context) {
	var body struct {
		Value []config.AmpUpstreamAPIKeyEntry `json:"value"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(400, gin.H{"error": "invalid body"})
		return
	}
	// Normalize entries: trim whitespace, filter empty
	normalized := normalizeAmpUpstreamAPIKeyEntries(body.Value)
	h.updateAmpCode(c, func(amp *config.AmpCode) {
		amp.UpstreamAPIKeys = normalized
	})
}

// PatchAmpUpstreamAPIKeys adds or updates upstream API keys entries.
// Matching is done by upstream-api-key value.
func (h *Handler) PatchAmpUpstreamAPIKeys(c *gin.Context) {
	var body struct {
		Value []config.AmpUpstreamAPIKeyEntry `json:"value"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(400, gin.H{"error": "invalid body"})
		return
	}

	h.updateAmpCode(c, func(amp *config.AmpCode) {
		existing := make(map[string]int)
		for i, entry := range amp.UpstreamAPIKeys {
			existing[strings.TrimSpace(entry.UpstreamAPIKey)] = i
		}

		for _, newEntry := range body.Value {
			upstreamKey := strings.TrimSpace(newEntry.UpstreamAPIKey)
			if upstreamKey == "" {
				continue
			}
			normalizedEntry := config.AmpUpstreamAPIKeyEntry{
				UpstreamAPIKey: upstreamKey,
				APIKeys:        normalizeAPIKeysList(newEntry.APIKeys),
			}
			if idx, ok := existing[upstreamKey]; ok {
				amp.UpstreamAPIKeys[idx] = normalizedEntry
			} else {
				amp.UpstreamAPIKeys = append(amp.UpstreamAPIKeys, normalizedEntry)
				existing[upstreamKey] = len(amp.UpstreamAPIKeys) - 1
			}
		}
	})
}

// DeleteAmpUpstreamAPIKeys removes specified upstream API keys entries.
// Body must be JSON: {"value": ["<upstream-api-key>", ...]}.
// If "value" is an empty array, clears all entries.
// If JSON is invalid or "value" is missing/null, returns 400 and does not persist any change.
func (h *Handler) DeleteAmpUpstreamAPIKeys(c *gin.Context) {
	var body struct {
		Value []string `json:"value"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(400, gin.H{"error": "invalid body"})
		return
	}

	if body.Value == nil {
		c.JSON(400, gin.H{"error": "missing value"})
		return
	}

	// Empty array means clear all
	if len(body.Value) == 0 {
		h.updateAmpCode(c, func(amp *config.AmpCode) { amp.UpstreamAPIKeys = nil })
		return
	}

	toRemove := make(map[string]bool)
	for _, key := range body.Value {
		trimmed := strings.TrimSpace(key)
		if trimmed == "" {
			continue
		}
		toRemove[trimmed] = true
	}
	if len(toRemove) == 0 {
		c.JSON(400, gin.H{"error": "empty value"})
		return
	}

	h.updateAmpCode(c, func(amp *config.AmpCode) {
		newEntries := make([]config.AmpUpstreamAPIKeyEntry, 0, len(amp.UpstreamAPIKeys))
		for _, entry := range amp.UpstreamAPIKeys {
			if !toRemove[strings.TrimSpace(entry.UpstreamAPIKey)] {
				newEntries = append(newEntries, entry)
			}
		}
		amp.UpstreamAPIKeys = newEntries
	})
}

// normalizeAmpUpstreamAPIKeyEntries normalizes a list of upstream API key entries.
func normalizeAmpUpstreamAPIKeyEntries(entries []config.AmpUpstreamAPIKeyEntry) []config.AmpUpstreamAPIKeyEntry {
	if len(entries) == 0 {
		return nil
	}
	out := make([]config.AmpUpstreamAPIKeyEntry, 0, len(entries))
	for _, entry := range entries {
		upstreamKey := strings.TrimSpace(entry.UpstreamAPIKey)
		if upstreamKey == "" {
			continue
		}
		apiKeys := normalizeAPIKeysList(entry.APIKeys)
		out = append(out, config.AmpUpstreamAPIKeyEntry{
			UpstreamAPIKey: upstreamKey,
			APIKeys:        apiKeys,
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// normalizeAPIKeysList trims and filters empty strings from a list of API keys.
func normalizeAPIKeysList(keys []string) []string {
	if len(keys) == 0 {
		return nil
	}
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		trimmed := strings.TrimSpace(k)
		if trimmed != "" {
			out = append(out, trimmed)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func updateAmpScalar[T string | bool](h *Handler, c *gin.Context, apply func(*config.AmpCode, T)) {
	var body struct {
		Value *T `json:"value"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Value == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	h.updateAmpCode(c, func(amp *config.AmpCode) { apply(amp, *body.Value) })
}

func (h *Handler) updateAmpCode(c *gin.Context, apply func(*config.AmpCode)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cfg == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "config unavailable"})
		return
	}
	draft := h.cfg.CloneForRuntime()
	apply(&draft.AmpCode)
	persistProviderFieldLocked(h, c, draft, &h.cfg.AmpCode, draft.AmpCode)
}
