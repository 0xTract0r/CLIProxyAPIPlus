package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api/handlers/management"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestPluginDeleteLocalRecovery(t *testing.T) {
	const id = "sample-fixture"
	const managementKey = "ordinary-local-delete-fixture"
	t.Setenv("MANAGEMENT_PASSWORD", managementKey)
	extension := ".so"
	switch runtime.GOOS {
	case "darwin":
		extension = ".dylib"
	case "windows":
		extension = ".dll"
	}
	for _, version := range []string{"v0", "v8"} {
		for _, kind := range []string{"both", "config only", "concurrent config only", "file only", "save failure retry", "stage denied", "recovery pending"} {
			t.Run(version+"/"+kind, func(t *testing.T) {
				dir := t.TempDir()
				pluginDir := filepath.Join(dir, "plugins", runtime.GOOS, runtime.GOARCH)
				if err := os.MkdirAll(pluginDir, 0700); err != nil {
					t.Fatal(err)
				}
				body := []byte("plain fixture bytes")
				selected := filepath.Join(pluginDir, id+"-v0.1.0"+extension)
				unselected := filepath.Join(pluginDir, id+"-v0.2.0"+extension)
				hasConfig := kind != "file only"
				hasFile := kind != "config only" && kind != "concurrent config only"
				write := func(path string, data []byte) {
					t.Helper()
					if err := os.WriteFile(path, data, 0600); err != nil {
						t.Fatal(err)
					}
				}
				if hasFile {
					write(selected, body)
					if hasConfig {
						write(unselected, []byte("other version"))
					}
				}
				configPath := filepath.Join(dir, "config.yaml")
				yaml := "plugins:\n  enabled: false\n  dir: " + filepath.Dir(filepath.Dir(pluginDir)) + "\n"
				if hasConfig {
					yaml += "  configs:\n    " + id + ":\n      enabled: false\n      store: {version: '0.1.0'}\n"
				}
				write(configPath, []byte(yaml))
				cfg, err := config.LoadConfig(configPath)
				if err != nil {
					t.Fatal(err)
				}
				h := management.NewHandler(cfg, configPath, nil)
				h.SetLocalPassword(managementKey)
				s := &Server{cfg: cfg, engine: gin.New(), mgmt: h}
				s.managementRoutesEnabled.Store(true)
				s.registerManagementRoutes()
				call := func(method, path string) *httptest.ResponseRecorder {
					req := httptest.NewRequest(method, path, nil)
					req.RemoteAddr = "127.0.0.1:12345"
					req.Header.Set("Authorization", "Bearer "+managementKey)
					rec := httptest.NewRecorder()
					s.engine.ServeHTTP(rec, req)
					return rec
				}
				request := func(method, path string, status int) map[string]any {
					t.Helper()
					rec := call(method, path)
					if rec.Code != status {
						t.Fatalf("%s %s: got %d, want %d: %s", method, path, rec.Code, status, rec.Body)
					}
					var got map[string]any
					if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
						t.Fatal(err)
					}
					return got
				}
				path := fmt.Sprintf("/%s/management/plugins/%s", version, id)
				read := func(name string, want []byte) {
					t.Helper()
					got, err := os.ReadFile(name)
					if err != nil || !bytes.Equal(got, want) {
						t.Fatalf("fixture read: got %q want %q, error %v", got, want, err)
					}
				}
				if kind == "save failure retry" {
					before, err := os.Stat(selected)
					if err != nil {
						t.Fatal(err)
					}
					if err := os.Rename(configPath, configPath+".previous"); err != nil {
						t.Fatal(err)
					}
					if got := request(http.MethodDelete, path, 500); got["error"] != "config_save_failed" {
						t.Fatalf("unexpected save error: %v", got)
					}
					read(selected, body)
					after, err := os.Stat(selected)
					if err != nil || !os.SameFile(before, after) {
						t.Fatalf("restore did not preserve original file identity: %v", err)
					}
					read(configPath+".previous", []byte(yaml))
					request(http.MethodGet, "/v0/management/plugins/"+id+"/config", 200)
					if err := os.Rename(configPath+".previous", configPath); err != nil {
						t.Fatal(err)
					}
				}
				if kind == "recovery pending" {
					// Re-enter with the two real files left by an interrupted restore.
					dir := pluginDir
					if version == "v8" {
						dir = filepath.Dir(filepath.Dir(pluginDir))
					}
					name := fmt.Sprintf(".plugin-delete-%x", sha256.Sum256([]byte(id)))
					recoveryDir := filepath.Join(dir, name)
					if err := os.Mkdir(recoveryDir, 0700); err != nil {
						t.Fatal(err)
					}
					savedFile := filepath.Join(recoveryDir, filepath.Base(selected))
					backup := []byte("original staged fixture bytes")
					replacement := []byte("new replacement fixture bytes")
					write(savedFile, backup)
					write(selected, replacement)
					other := filepath.Join(pluginDir, "other-fixture"+extension)
					write(other, []byte("unrelated fixture bytes"))
					if got := request(http.MethodDelete, path, 409); got["error"] != "plugin_delete_recovery_pending" ||
						got["recovery_directory"] != name {
						t.Fatalf("unexpected pending deletion response: %v", got)
					}
					read(selected, replacement)
					read(savedFile, backup)
					read(configPath, []byte(yaml))
					request(http.MethodGet, "/v0/management/plugins/"+id+"/config", 200)
					request(http.MethodDelete, "/"+version+"/management/plugins/other-fixture", 200)
					if _, err := os.Stat(other); !os.IsNotExist(err) {
						t.Fatalf("unrelated plugin was not deleted: %v", err)
					}
					read(selected, replacement)
					read(savedFile, backup)
					return
				}
				if kind == "stage denied" {
					if os.Getuid() == 0 || runtime.GOOS == "windows" {
						t.Skip("requires Unix directory permission enforcement")
					}
					if err := os.Chmod(pluginDir, 0500); err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = os.Chmod(pluginDir, 0700) })
					if got := request(http.MethodDelete, path, 500); got["error"] != "plugin_delete_failed" {
						t.Fatalf("unexpected staging error: %v", got)
					}
					read(selected, body)
					read(configPath, []byte(yaml))
					request(http.MethodGet, "/v0/management/plugins/"+id+"/config", 200)
					return
				}
				if kind == "concurrent config only" {
					done := make(chan int, 2)
					for range 2 {
						go func() { done <- call(http.MethodDelete, path).Code }()
					}
					first, second := <-done, <-done
					if !(first == 200 && second == 404 || first == 404 && second == 200) {
						t.Fatalf("concurrent deletes returned %d, %d", first, second)
					}
				} else {
					ok := request(http.MethodDelete, path, 200)
					if ok["file_deleted"] != hasFile || ok["configured_removed"] != hasConfig || ok["restart_required"] != false {
						t.Fatalf("incorrect deletion outcome: %v", ok)
					}
				}
				if _, err := os.Stat(selected); !os.IsNotExist(err) {
					t.Fatalf("target file still exists: %v", err)
				}
				if hasConfig && hasFile {
					read(unselected, []byte("other version"))
				}
				if !hasConfig || !hasFile {
					request(http.MethodDelete, path, 404)
				}
				saved, err := config.LoadConfig(configPath)
				if err != nil {
					t.Fatal(err)
				}
				if _, present := saved.Plugins.Configs[id]; present {
					t.Fatal("deleted config still present after reopening")
				}
			})
		}
	}
}
