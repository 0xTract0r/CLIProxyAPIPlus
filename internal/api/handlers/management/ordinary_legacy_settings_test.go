package management

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestLegacySettingsEndpointsRejectWithoutMutation(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "old.json")
	before := []byte("{\n  \"note\": \"keep\"\n}\n")
	if err := os.WriteFile(file, before, 0600); err != nil {
		t.Fatal(err)
	}
	h := &Handler{cfg: &config.Config{AuthDir: dir}}
	engine := gin.New()
	base := "/v0/management/auth-files/account-scheduling"
	engine.PATCH(base, h.PatchAuthFileAccountScheduling)
	engine.POST(base+"/preview", h.PreviewAuthFileAccountScheduling)
	for method, path := range map[string]string{http.MethodPatch: base, http.MethodPost: base + "/preview"} {
		res := httptest.NewRecorder()
		engine.ServeHTTP(res, httptest.NewRequest(method, path, strings.NewReader("{\"name\":\"old.json\"}")))
		if res.Code != http.StatusGone {
			t.Fatalf("%s got %d", method, res.Code)
		}
	}
	after, err := os.ReadFile(file)
	if err != nil || string(after) != string(before) {
		t.Fatal("original file changed")
	}
}
