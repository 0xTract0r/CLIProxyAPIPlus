package management

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/kilo"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestOrdinaryKiloDeviceGrantAndCancel(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "kilo-fixture-management-key")
	previous := newKiloWebClient
	t.Cleanup(func() { newKiloWebClient = previous })
	var starts, approvedPolls, cancelledPolls atomic.Int32
	newKiloWebClient = func(cfg *config.Config) *kilo.KiloAuth {
		client := ordinaryWebOAuthHTTPClient(cfg)
		client.Transport = gitLabFakeTransport(func(r *http.Request) (*http.Response, error) {
			if r.URL.Host != "api.kilo.ai" || r.URL.Scheme != "https" {
				t.Errorf("unexpected Kilo request %s", r.URL)
			}
			const endpoint = "/api/device-auth/codes"
			raw, status := "", http.StatusOK
			switch r.URL.Path {
			case endpoint:
				if r.Method != http.MethodPost {
					t.Errorf("device authorization method = %s", r.Method)
				}
				if starts.Add(1) == 1 {
					raw = `{"code":"approved-fake","verificationUrl":"https://app.kilo.ai/device","expiresIn":30}`
				} else {
					raw = `{"code":"cancelled-fake","verificationUrl":"https://app.kilo.ai/device","expiresIn":30}`
				}
			case endpoint + "/approved-fake":
				approvedPolls.Add(1)
				raw = `{"status":"approved","token":"fake-kilo-grant","userEmail":"ordinary@example.invalid"}`
			case endpoint + "/cancelled-fake":
				cancelledPolls.Add(1)
				t.Error("device grant was polled after cancellation")
				raw = `{"status":"pending"}`
			case "/api/profile":
				if r.Header.Get("Authorization") != "Bearer fake-kilo-grant" {
					t.Error("profile call missing the approved grant")
				}
				raw = `{"email":"ordinary@example.invalid","organizations":[{"id":"org-fake"}]}`
			case "/api/organizations/org-fake/defaults":
				raw = `{"model":"fixture-model"}`
			default:
				t.Errorf("unexpected Kilo path %s", r.URL.Path)
				status = http.StatusNotFound
			}
			return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(raw))}, nil
		})
		return kilo.NewKiloAuthWithHTTPClient(client)
	}

	dir := t.TempDir()
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: dir}, nil)
	sibling := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: dir}, nil)
	newRouter := func(handler *Handler) *gin.Engine {
		router := gin.New()
		group := router.Group("/v0/management", handler.Middleware())
		group.GET("/kilo-auth-url", handler.RequestKiloToken)
		group.GET("/get-auth-status", handler.GetAuthStatus)
		group.DELETE("/oauth-session", handler.CancelAuthSession)
		return router
	}
	engine, other := newRouter(h), newRouter(sibling)
	call := func(router *gin.Engine, method, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/v0/management/"+path, nil)
		req.Header.Set("Authorization", "Bearer kilo-fixture-management-key")
		req.RemoteAddr = "127.0.0.1:12345"
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}
	start := func(want string) string {
		t.Helper()
		w := call(engine, http.MethodGet, "kilo-auth-url")
		var started struct {
			State string
			Code  string `json:"user_code"`
		}
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &started) != nil || started.Code != want || started.State == "" {
			t.Fatalf("start %s: %d %s", want, w.Code, w.Body.String())
		}
		return started.State
	}
	state := start("approved-fake")
	cancelled := start("cancelled-fake")
	cancelPath := "oauth-session?state=" + cancelled
	if w := call(other, http.MethodDelete, cancelPath); !strings.Contains(w.Body.String(), `"cancelled":false`) {
		t.Fatalf("other handler cancelled a grant in the same AuthDir: %d %s", w.Code, w.Body.String())
	}
	if w := call(engine, http.MethodDelete, cancelPath); !strings.Contains(w.Body.String(), `"cancelled":true`) {
		t.Fatalf("cancel: %d %s", w.Code, w.Body.String())
	}
	if _, err := h.WriteFileOAuthCallback("kilo", state, "synthetic-code", ""); err == nil {
		t.Fatal("device grant accepted an external callback file")
	}

	deadline := time.Now().Add(8 * time.Second)
	for {
		w := call(engine, http.MethodGet, "get-auth-status?state="+state)
		if strings.Contains(w.Body.String(), `"saved":true`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("completion: %d %s", w.Code, w.Body.String())
		}
		time.Sleep(25 * time.Millisecond)
	}
	path := filepath.Join(dir, "kilo-ordinary@example.invalid.json")
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), `"kilocodeToken":"fake-kilo-grant"`) ||
		!strings.Contains(string(data), `"kilocodeOrganizationId":"org-fake"`) ||
		!strings.Contains(string(data), `"kilocodeModel":"fixture-model"`) {
		t.Fatalf("Kilo token file incomplete: %v", err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("Kilo file permissions: %v %v", info, err)
	}
	if approvedPolls.Load() != 1 || cancelledPolls.Load() != 0 {
		t.Fatalf("poll counts approved=%d cancelled=%d", approvedPolls.Load(), cancelledPolls.Load())
	}
}
