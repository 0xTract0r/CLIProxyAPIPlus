package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api/handlers/management"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestPluginConfigWritePreservesLastSavedValue(t *testing.T) {
	const root = "/v0/management/plugins/example/"
	const localPassword = "local-plugin-test"
	t.Setenv("MANAGEMENT_PASSWORD", localPassword)
	operations := []struct {
		name, method, suffix, body, expected string
	}{
		{"replace", http.MethodPut, "config", `{"enabled":true,"option":"new"}`, `{"enabled":true,"option":"new"}`},
		{"patch", http.MethodPatch, "config", `{"option":"new","remove":null}`, `{"enabled":false,"option":"new"}`},
		{"enable", http.MethodPatch, "enabled", `{"enabled":true}`, `{"enabled":true,"option":"old","remove":"keep"}`},
	}
	for _, op := range operations {
		t.Run(op.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.yaml")
			if err := os.WriteFile(path, []byte("plugins:\n  configs:\n    example: {enabled: false, option: old, remove: keep}\n"), 0600); err != nil {
				t.Fatal(err)
			}
			register := func() *gin.Engine {
				t.Helper()
				cfg, err := config.LoadConfig(path)
				if err != nil {
					t.Fatal(err)
				}
				h := management.NewHandler(cfg, path, nil)
				h.SetLocalPassword(localPassword)
				s := &Server{cfg: cfg, engine: gin.New(), mgmt: h}
				s.managementRoutesEnabled.Store(true)
				s.registerManagementRoutes()
				return s.engine
			}
			router := register()
			request := func(method, suffix, body string, want int) []byte {
				t.Helper()
				req := httptest.NewRequest(method, root+suffix, strings.NewReader(body))
				req.RemoteAddr = "127.0.0.1:12345"
				req.Header.Set("Authorization", "Bearer "+localPassword)
				req.Header.Set("Content-Type", "application/json")
				response := httptest.NewRecorder()
				router.ServeHTTP(response, req)
				if response.Code != want {
					t.Fatalf("%s %s returned %d, want %d: %s", method, suffix, response.Code, want, response.Body.String())
				}
				return response.Body.Bytes()
			}
			read := func() map[string]any {
				t.Helper()
				var value map[string]any
				if err := json.Unmarshal(request(http.MethodGet, "config", "", http.StatusOK), &value); err != nil {
					t.Fatal(err)
				}
				return value
			}
			before := read()
			if err := os.Rename(path, path+".previous"); err != nil {
				t.Fatal(err)
			}
			request(op.method, op.suffix, op.body, http.StatusInternalServerError)
			if got := read(); !reflect.DeepEqual(got, before) {
				t.Fatalf("failed write changed runtime config: got %v, want %v", got, before)
			}
			if err := os.Rename(path+".previous", path); err != nil {
				t.Fatal(err)
			}
			request(op.method, op.suffix, op.body, http.StatusOK)
			var expected map[string]any
			if err := json.Unmarshal([]byte(op.expected), &expected); err != nil {
				t.Fatal(err)
			}
			if got := read(); !reflect.DeepEqual(got, expected) {
				t.Fatalf("after saving got %v, want %v", got, expected)
			}
			router = register()
			if got := read(); !reflect.DeepEqual(got, expected) {
				t.Fatalf("after reopening got %v, want %v", got, expected)
			}
		})
	}
}
