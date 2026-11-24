package management

import "github.com/gin-gonic/gin"

// PreviewAuthFileAccountScheduling reports the same retirement as the write endpoint.
func (h *Handler) PreviewAuthFileAccountScheduling(c *gin.Context) {
	unavailableLegacyAccountScheduling(c)
}
