package management

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestPluginConfigWritesRejectUnavailableConfig(t *testing.T) {
	for _, test := range []struct {
		name, method, path string
		handle             func(*Handler, *gin.Context)
	}{
		{"put", http.MethodPut, "/v0/management/plugins/sample/config", (*Handler).PutPluginConfig},
		{"patch", http.MethodPatch, "/v0/management/plugins/sample/config", (*Handler).PatchPluginConfig},
		{"enabled", http.MethodPatch, "/v0/management/plugins/sample/enabled", (*Handler).PatchPluginEnabled},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := &Handler{}
			response := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(response)
			c.Params = gin.Params{{Key: "id", Value: "sample"}}
			c.Request = httptest.NewRequest(test.method, test.path, strings.NewReader(`{"enabled":true}`))
			c.Request.Header.Set("Content-Type", "application/json")
			test.handle(h, c)
			if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "config unavailable") {
				t.Fatalf("got status %d and body %s, want unavailable config", response.Code, response.Body)
			}
		})
	}
}
