package logging

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

func TestOAuthCallbackAccessLogOmitsQuery(t *testing.T) {
	var output bytes.Buffer
	logger := log.StandardLogger()
	oldOutput, oldLevel := logger.Out, logger.GetLevel()
	logger.SetOutput(&output)
	logger.SetLevel(log.InfoLevel)
	t.Cleanup(func() {
		logger.SetOutput(oldOutput)
		logger.SetLevel(oldLevel)
	})
	router := gin.New()
	router.Use(GinLogrusLogger())
	for _, path := range []string{"/callback", "/anthropic/callback", "/codex/callback", "/antigravity/callback", "/devin/callback", "/google/callback", "/gitlab/callback", "/v0/management/oauth-callback", "/v8/management/oauth/callback", "/v8/management/oauth/devin/callback",
		"/callback/", "/future-provider/CALLBACK/", "/future-provider//callback", "/V8//Management/OAuth//Future-Provider/CALLBACK/"} {
		router.GET(path, func(c *gin.Context) {
			if c.Query("code") == "accepted-fixture" {
				c.Status(http.StatusOK)
			} else {
				_ = c.Error(fmt.Errorf("invalid callback: %s", c.Query("error_description")))
				c.Status(http.StatusBadRequest)
			}
		})
		for _, code := range []string{"accepted-fixture", "rejected-fixture"} {
			output.Reset()
			req := httptest.NewRequest(http.MethodGet, path+"?code="+code+"&state=private-state-fixture&error_description=private-error-fixture", nil)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			line := output.String()
			if !strings.Contains(line, path) || !strings.Contains(line, "GET") || !strings.Contains(line, fmt.Sprintf("%d |", rec.Code)) || !strings.Contains(line, "request_id=") {
				t.Errorf("%s: missing access log metadata: %s", path, line)
			}
			for _, secret := range []string{code, "private-state-fixture", "private-error-fixture", "?"} {
				if strings.Contains(line, secret) {
					t.Errorf("%s %d: callback input was logged", path, rec.Code)
					break
				}
			}
		}
	}
	output.Reset()
	router.GET("/ordinary/callback/search", func(c *gin.Context) { c.Status(http.StatusOK) })
	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/ordinary/callback/search?page=2", nil))
	if !strings.Contains(output.String(), "/ordinary/callback/search?page=2") {
		t.Fatal("ordinary access log lost its query")
	}
}
