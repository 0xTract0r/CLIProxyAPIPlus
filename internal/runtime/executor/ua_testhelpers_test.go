package executor

import (
	"context"
	"net/http/httptest"

	"github.com/gin-gonic/gin"
)

// ginContextWithUA builds a context carrying a gin request with the given
// User-Agent so applyCloaking's ShouldCloak gate can be exercised.
//
// This shared UA test helper previously lived in the normalize-account-env test
// file; that file was removed with the normalize feature, but the helper is still
// used by unrelated surviving guard tests (real-path billing alignment and
// header/body consistency), so it is kept here as a standalone test helper.
func ginContextWithUA(userAgent string) context.Context {
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	if userAgent != "" {
		req.Header.Set("User-Agent", userAgent)
	}
	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ginCtx.Request = req
	//nolint:staticcheck // applyCloaking reads the gin context via the "gin" string key.
	return context.WithValue(context.Background(), "gin", ginCtx)
}
