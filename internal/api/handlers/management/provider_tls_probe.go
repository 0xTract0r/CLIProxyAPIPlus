package management

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	runtimehelps "github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
)

type providerTLSProbeRequest struct {
	Name          string `json:"name"`
	Provider      string `json:"provider"`
	TargetHost    string `json:"target_host"`
	Method        string `json:"method"`
	Path          string `json:"path"`
	CorrelationID string `json:"correlation_id"`
}

var runProviderTLSProbe = runtimehelps.RunProviderTLSProbe

func (h *Handler) RunProviderTLSProbe(c *gin.Context) {
	if h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}

	var req providerTLSProbeRequest
	if c.Request != nil && c.Request.Body != nil {
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
			return
		}
	}
	name := strings.TrimSpace(firstNonEmpty(req.Name, c.Query("name"), c.Query("auth_name")))
	if name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
		return
	}

	targetAuth := findAuthByName(h.authManager, name)
	if targetAuth == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "auth file not found"})
		return
	}
	if provider := strings.ToLower(strings.TrimSpace(req.Provider)); provider != "" && provider != strings.ToLower(strings.TrimSpace(targetAuth.Provider)) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "provider does not match auth file"})
		return
	}

	correlationID := strings.TrimSpace(req.CorrelationID)
	if correlationID == "" && c.Request != nil {
		correlationID = strings.TrimSpace(firstNonEmpty(
			c.Request.Header.Get("X-Correlation-ID"),
			c.Request.Header.Get("X-Request-ID"),
			c.Request.Header.Get("X-Client-Request-ID"),
		))
	}

	result, err := runProviderTLSProbe(c.Request.Context(), h.cfg, targetAuth, runtimehelps.ProviderTLSProbeOptions{
		CorrelationID: correlationID,
		TargetHost:    req.TargetHost,
		Method:        req.Method,
		Path:          req.Path,
	})
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, result)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
