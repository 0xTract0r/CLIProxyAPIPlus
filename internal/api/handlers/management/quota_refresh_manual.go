package management

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// RefreshOrdinaryQuota reports availability for one explicitly selected
// credential. The legacy snapshot UI has no public quota contract for the
// stored OAuth sessions, so those sessions remain local read-only here.
func (h *Handler) RefreshOrdinaryQuota(c *gin.Context) {
	var req struct {
		AuthID    string `json:"auth_id"`
		AuthIndex string `json:"auth_index"`
		Name      string `json:"name"`
		Provider  string `json:"provider"`
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 16<<10)
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil || decoder.Decode(new(json.RawMessage)) != io.EOF {
		writeManualActionError(c, http.StatusBadRequest, "invalid quota refresh request")
		return
	}
	req.AuthID = strings.TrimSpace(req.AuthID)
	if req.AuthID != "" {
		h.mu.Lock()
		manager := h.authManager
		h.mu.Unlock()
		if manager == nil {
			writeManualActionError(c, http.StatusServiceUnavailable, "core auth manager unavailable")
			return
		}
		selected, found := manager.GetByID(req.AuthID)
		if !found || selected == nil {
			writeManualActionError(c, http.StatusNotFound, "auth file not found")
			return
		}
		index := lockedAuthIndex(selected)
		if req.AuthIndex != "" && strings.TrimSpace(req.AuthIndex) != index {
			writeManualActionError(c, http.StatusConflict, "credential identity does not match the selected file")
			return
		}
		req.AuthIndex = index
	}
	target, status, message := h.ordinaryManualCredential(req.Name, req.AuthIndex)
	if status != 0 {
		writeManualActionError(c, status, message)
		return
	}
	if req.AuthID != "" && target.auth.ID != req.AuthID {
		writeManualActionError(c, http.StatusConflict, "credential identity does not match the selected file")
		return
	}
	provider := strings.ToLower(strings.TrimSpace(target.auth.Provider))
	if wanted := strings.TrimSpace(req.Provider); wanted != "" && !strings.EqualFold(wanted, provider) {
		writeManualActionError(c, http.StatusConflict, "provider does not match the selected credential")
		return
	}
	entries := make([]storedQuotaEntry, 0, 1)
	if provider == "codex" || provider == "claude" {
		entries = append(entries, makeStoredQuotaEntry(target.auth, provider))
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusNotImplemented, gin.H{
		"status":    "error",
		"supported": false,
		"error":     "the selected credential has no public per-credential quota endpoint for this view; the saved snapshot remains available",
		"entries":   entries,
	})
}
