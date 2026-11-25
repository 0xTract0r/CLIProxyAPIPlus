package management

import (
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/clienterror"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// RefreshAuthFiles triggers active refresh for a single auth file or all auth files.
// Accepts query parameters (?all=true, ?name=file.json) or JSON body ({"all": true, "name": "file.json"}).
func (h *Handler) RefreshAuthFiles(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}
	h.mu.Lock()
	manager := h.authManager
	managerGeneration, directoryGeneration := h.authManagerGeneration, h.fileOAuthGeneration
	h.mu.Unlock()
	if manager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}

	var req struct {
		Name      string `json:"name"`
		AuthIndex string `json:"auth_index"`
		All       bool   `json:"all"`
	}
	if c.Request.Body != nil && c.Request.ContentLength != 0 {
		if errBind := c.ShouldBindJSON(&req); errBind != nil && !errors.Is(errBind, io.EOF) {
			c.JSON(clienterror.HTTPStatusFromErrorOr(errBind, http.StatusBadRequest), gin.H{"error": "invalid request body: " + errBind.Error()})
			return
		}
	}
	if c.Query("all") == "true" {
		req.All = true
	}
	if queryName := strings.TrimSpace(c.Query("name")); queryName != "" && req.Name == "" {
		req.Name = queryName
	}
	if queryAuthIndex := strings.TrimSpace(c.Query("auth_index")); queryAuthIndex != "" && req.AuthIndex == "" {
		req.AuthIndex = queryAuthIndex
	}

	ctx := c.Request.Context()

	sameScope := func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.authManager == manager && h.authManagerGeneration == managerGeneration && h.fileOAuthGeneration == directoryGeneration
	}

	if req.All {
		if !sameScope() {
			c.JSON(http.StatusConflict, gin.H{"error": "credential scope changed during refresh"})
			return
		}
		results := manager.ForceRefreshAll(ctx)
		c.JSON(http.StatusOK, gin.H{
			"ok":      true,
			"results": results,
		})
		return
	}

	name := strings.TrimSpace(req.Name)
	if name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name or all=true is required"})
		return
	}

	targetAuth, ok := lookupAuthFileInManager(manager, name, req.AuthIndex)
	if !ok || targetAuth == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "auth file not found"})
		return
	}

	if !sameScope() {
		c.JSON(http.StatusConflict, gin.H{"error": "auth file changed during refresh"})
		return
	}
	beginCommit := func() (func(), bool) {
		h.authUploadScopeMu.RLock()
		if h.authManager != manager || h.authManagerGeneration != managerGeneration || h.fileOAuthGeneration != directoryGeneration {
			h.authUploadScopeMu.RUnlock()
			return nil, false
		}
		return h.authUploadScopeMu.RUnlock, true
	}
	ctx = coreauth.WithAuthPublicationFence(ctx, &coreauth.AuthPublicationFence{
		BeginPublication: func() (func(), bool) {
			// Publication holds manager.mu: never queue behind a scope writer.
			if !h.authUploadScopeMu.TryRLock() {
				return nil, false
			}
			if h.authManager != manager || h.authManagerGeneration != managerGeneration || h.fileOAuthGeneration != directoryGeneration {
				h.authUploadScopeMu.RUnlock()
				return nil, false
			}
			return h.authUploadScopeMu.RUnlock, true
		},
	})
	refreshed, err := manager.ForceRefreshObservedAuth(ctx, targetAuth, beginCommit)
	if errors.Is(err, coreauth.ErrSelectedRefreshChanged) {
		c.JSON(http.StatusConflict, gin.H{"error": "auth file changed during refresh"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": err.Error(),
		})
		return
	}

	if refreshed == nil || !sameScope() {
		c.JSON(http.StatusConflict, gin.H{"error": "auth file changed during refresh"})
		return
	}
	current, exists := manager.GetByID(refreshed.ID)
	if !exists || current.RegistrationEpoch != refreshed.RegistrationEpoch || current.CredentialVersion != refreshed.CredentialVersion {
		c.JSON(http.StatusConflict, gin.H{"error": "auth file changed during refresh"})
		return
	}

	if c.GetBool(ConfigV8ContextKey) {
		h.mu.Lock()
		host := h.pluginHost
		h.mu.Unlock()
		quota := authFileQuotaContext{v8: true}
		if host != nil {
			quota.pluginIDs = host.QuotaProviderPluginIDs(ctx)
		}
		c.JSON(http.StatusOK, gin.H{
			"ok":   true,
			"auth": h.buildAuthFileEntry(refreshed, quota),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"ok":   true,
		"auth": refreshed,
	})
}
