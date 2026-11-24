package management

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// PatchAuthFileAccountScheduling handles clients that still submit legacy settings.
func (h *Handler) PatchAuthFileAccountScheduling(c *gin.Context) {
	unavailableLegacyAccountScheduling(c)
}

func unavailableLegacyAccountScheduling(c *gin.Context) {
	c.JSON(http.StatusGone, gin.H{"error": "legacy account scheduling is unavailable in this runtime"})
}

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
