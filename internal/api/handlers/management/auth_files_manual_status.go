package management

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/clienterror"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// RefreshOrdinaryAuthFileStatus renews the selected supported grant and records
// the outcome locally. Disabled and unsupported credentials are read-only.
func (h *Handler) RefreshOrdinaryAuthFileStatus(c *gin.Context) {
	var req struct {
		Name      string `json:"name"`
		AuthIndex string `json:"auth_index"`
		Trigger   string `json:"trigger"`
	}
	if c.ShouldBindJSON(&req) != nil {
		writeManualActionError(c, http.StatusBadRequest, "invalid request body")
		return
	}
	if trigger := strings.TrimSpace(req.Trigger); trigger != "" && !strings.EqualFold(trigger, "manual") {
		writeManualActionError(c, http.StatusBadRequest, "only manual status checks are supported")
		return
	}
	target, status, msg := h.ordinaryManualCredential(req.Name, req.AuthIndex)
	if status != 0 {
		writeManualActionError(c, status, msg)
		return
	}
	source, err := os.OpenRoot(target.dir)
	if err != nil {
		writeManualActionError(c, http.StatusServiceUnavailable, "credential history is unavailable")
		return
	}
	defer source.Close()

	before := authStatusHistorySnapshotFromAuth(target.auth)
	current := target.auth
	refreshReason := h.ordinaryStatusReadOnlyReason(target)
	var refreshErr error
	if refreshReason == "" {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 35*time.Second)
		defer cancel()
		current, refreshErr = target.manager.ForceRefreshSelectedAuth(ctx, target.auth)
		if refreshErr != nil {
			if errors.Is(refreshErr, coreauth.ErrSelectedRefreshUnavailable) {
				refreshReason = "refresh is disabled for this credential"
				refreshErr = nil
			}
			if errors.Is(refreshErr, coreauth.ErrSelectedRefreshChanged) {
				writeManualActionError(c, http.StatusConflict, "credential changed during refresh; reload the credentials list")
				return
			}
			latest, exists := target.manager.GetByID(target.auth.ID)
			if !exists || latest.RegistrationEpoch != target.auth.RegistrationEpoch ||
				latest.CredentialVersion != target.auth.CredentialVersion {
				writeManualActionError(c, http.StatusConflict, "credential changed during refresh; reload the credentials list")
				return
			}
			current = latest
		}
	}
	if current == nil || !h.manualCredentialSameScope(target, current) {
		writeManualActionError(c, http.StatusConflict, "credential changed; reload the credentials list")
		return
	}
	file := h.buildAuthFileEntry(current, authFileQuotaContext{
		scope: &authFileListScope{dir: target.dir, root: source},
	})
	if file == nil {
		writeManualActionError(c, http.StatusConflict, "credential changed; reload the credentials list")
		return
	}
	after := authStatusHistorySnapshotFromAuth(current)
	after.Status, _ = file["status"].(string)
	after.Unavailable, _ = file["unavailable"].(bool)
	if refreshErr == nil {
		after.StatusMessage, _ = file["status_message"].(string)
	}
	message := ""
	status = http.StatusOK
	if refreshErr != nil {
		status, message = manualUpstreamError(c, refreshErr, "the provider did not refresh this credential")
		// A provider's raw error can contain private grant details.
		after.StatusMessage = message
		file["status_message"] = message
	}
	event := authStatusHistoryEvent{
		source: source, EventType: deriveAuthStatusHistoryEventType(before, after, refreshErr),
		AuthName: after.AuthName, Provider: after.Provider, Trigger: authStatusHistoryTriggerManual,
		PreviousStatus: before.Status, PreviousMessage: before.StatusMessage,
		Status: after.Status, StatusMessage: after.StatusMessage, Error: message,
	}
	if err := h.appendAuthStatusHistoryEvent(event); err != nil {
		if refreshErr != nil {
			c.JSON(status, gin.H{"status": "error", "error": message, "file": file, "credential_error": true, "history_saved": false})
			return
		}
		c.JSON(http.StatusServiceUnavailable, gin.H{"status": "error", "error": "credential status history could not be written", "file": file, "refreshed": refreshReason == ""})
		return
	}
	if refreshErr != nil {
		c.JSON(status, gin.H{"status": "error", "error": message, "file": file, "credential_error": true})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "file": file, "refreshed": refreshReason == "", "refresh_unavailable_reason": refreshReason})
}

func (h *Handler) ordinaryStatusReadOnlyReason(target ordinaryAccountSettingsTarget) string {
	record := target.auth
	if record.Disabled || record.Status == coreauth.StatusDisabled || record.AutoQuarantined || record.RefreshDisabled() {
		return "refresh is disabled for this credential"
	}
	cfg := h.configSnapshot()
	if cfg == nil || cfg.Home.Enabled {
		return "manual refresh for a local credential is unavailable in this service mode"
	}
	if !target.file.Valid() || !strings.EqualFold(record.Provider, "codex") || !record.HasSingleRefreshGrant() {
		return "this credential has no supported local refresh grant"
	}
	if _, ok := target.manager.Executor(record.Provider); !ok {
		return "no local refresh executor is registered for this credential"
	}
	return ""
}

func (h *Handler) manualCredentialSameScope(target ordinaryAccountSettingsTarget, current *coreauth.Auth) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.authManager == target.manager && resolvedV8SynthesisAuthDir(h.cfg) == target.dir &&
		current.RegistrationEpoch == target.auth.RegistrationEpoch && current.Provider == target.auth.Provider &&
		authAttribute(current, "path") == authAttribute(target.auth, "path")
}

func manualUpstreamError(c *gin.Context, err error, fallback string) (int, string) {
	status := clienterror.HTTPStatusFromError(err)
	if status < 400 || status > 599 {
		status = http.StatusBadGateway
	}
	var withHeaders interface{ Headers() http.Header }
	if errors.As(err, &withHeaders) {
		copyManualRetryAfter(c, withHeaders.Headers())
	}
	if c.Writer.Header().Get("Retry-After") == "" {
		var withDelay interface{ RetryAfter() *time.Duration }
		if errors.As(err, &withDelay) {
			if delay := withDelay.RetryAfter(); delay != nil && *delay >= 0 {
				seconds := int64(*delay / time.Second)
				if *delay%time.Second != 0 {
					seconds++
				}
				c.Header("Retry-After", strconv.FormatInt(seconds, 10))
			}
		}
	}
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return status, "the provider rejected the selected credential"
	case http.StatusConflict:
		return status, "the selected credential changed; reload the credentials list"
	case http.StatusTooManyRequests:
		return status, "the provider requires a wait before trying again"
	default:
		return status, fallback
	}
}

func copyManualRetryAfter(c *gin.Context, header http.Header) {
	raw := strings.TrimSpace(header.Get("Retry-After"))
	if raw == "" {
		return
	}
	if _, err := strconv.ParseUint(raw, 10, 63); err == nil {
		c.Header("Retry-After", raw)
	} else if _, err = http.ParseTime(raw); err == nil {
		c.Header("Retry-After", raw)
	}
}

// ordinaryManualCredential requires one explicitly selected, scoped record.
func (h *Handler) ordinaryManualCredential(name, index string) (ordinaryAccountSettingsTarget, int, string) {
	name = strings.TrimSpace(name)
	index = strings.TrimSpace(index)
	if name != "" && (name == "." || name == ".." || isUnsafeAuthFileName(name)) {
		return ordinaryAccountSettingsTarget{}, http.StatusBadRequest, "invalid credential name"
	}
	return h.ordinaryAccountSettingsTarget(ordinaryAccountSettingsRequest{name: name, index: index})
}

func writeManualActionError(c *gin.Context, status int, message string) {
	c.JSON(status, gin.H{"status": "error", "error": message, "supported": status != http.StatusNotImplemented})
}
