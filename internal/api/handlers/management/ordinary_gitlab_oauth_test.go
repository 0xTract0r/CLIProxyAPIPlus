package management

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	gitlabauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/gitlab"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

type gitLabFakeTransport func(*http.Request) (*http.Response, error)

func (f gitLabFakeTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestOrdinaryGitLabOAuthAndPAT(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "temporary-management-key")
	t.Setenv("GITLAB_BASE_URL", "https://gitlab.example.invalid")
	t.Setenv("GITLAB_OAUTH_CLIENT_ID", "fake-app")
	t.Setenv("GITLAB_OAUTH_CLIENT_SECRET", "")
	t.Setenv("GITLAB_OAUTH_REDIRECT_URI", "https://manager.example.invalid/gitlab/callback")

	previous := newGitLabWebClient
	t.Cleanup(func() { newGitLabWebClient = previous })
	var requests atomic.Int32
	newGitLabWebClient = func(cfg *config.Config) *gitlabauth.AuthClient {
		client := ordinaryWebOAuthHTTPClient(cfg)
		client.Transport = gitLabFakeTransport(func(r *http.Request) (*http.Response, error) {
			requests.Add(1)
			if r.URL.Host != "gitlab.example.invalid" || r.URL.Scheme != "https" {
				t.Errorf("unexpected outbound request %s", r.URL)
			}
			data, status := "", http.StatusOK
			h := make(http.Header)
			h.Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/oauth/token":
				raw, _ := io.ReadAll(r.Body)
				values, _ := url.ParseQuery(string(raw))
				if r.Method != http.MethodPost || values.Get("code") != "fake-callback" ||
					values.Get("client_id") != "fake-app" || values.Get("code_verifier") == "" ||
					values.Get("redirect_uri") != "https://manager.example.invalid/gitlab/callback" {
					t.Error("incorrect OAuth code exchange")
				}
				data = `{"access_token":"fake-grant","refresh_token":"fake-refresh","expires_in":120}`
			case "/api/v4/user":
				if r.Header.Get("Authorization") == "Bearer rejected-redirection" {
					status = http.StatusTemporaryRedirect
					h.Set("Location", "https://external.invalid/receive")
				} else {
					data = `{"id":9,"username":"fixture-user"}`
				}
			case "/api/v4/personal_access_tokens/self":
				data = `{"id":8,"user_id":9,"name":"fixture PAT","scopes":["api"]}`
			default:
				t.Errorf("unexpected outbound path %s", r.URL.Path)
				status = http.StatusNotFound
			}
			return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(data))}, nil
		})
		return gitlabauth.NewAuthClientWithHTTPClient(client)
	}

	dir := t.TempDir()
	handler := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: dir}, nil)
	handler.SetLocalPassword("temporary-management-key")
	engine := gin.New()
	mgmt := engine.Group("/v0/management", handler.Middleware())
	mgmt.GET("/gitlab-auth-url", handler.RequestGitLabToken)
	mgmt.POST("/gitlab-auth-url", handler.RequestGitLabPATToken)
	mgmt.DELETE("/oauth-session", handler.CancelAuthSession)
	mgmt.GET("/get-auth-status", handler.GetAuthStatus)
	engine.GET("/gitlab/callback", func(c *gin.Context) {
		if _, err := handler.WriteFileOAuthCallback("gitlab", c.Query("state"), c.Query("code"), ""); err != nil {
			c.Status(http.StatusBadRequest)
			return
		}
		c.Status(http.StatusOK)
	})
	request := func(method, path, input string, authorized bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(input))
		r.Header.Set("Content-Type", "application/json")
		r.RemoteAddr = "127.0.0.1:1234"
		if authorized {
			r.Header.Set("Authorization", "Bearer temporary-management-key")
		}
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, r)
		return w
	}
	const entry = "/v0/management/gitlab-auth-url"
	if w := request(http.MethodGet, entry, "", false); w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized start: %d", w.Code)
	}
	if w := request(http.MethodGet, entry+"?base_url=https%3A%2F%2Fwrong.invalid", "", true); w.Code != http.StatusBadRequest {
		t.Fatalf("unexpected GitLab origin: %d", w.Code)
	}
	start := func() string {
		t.Helper()
		w := request(http.MethodGet, entry, "", true)
		var response struct{ URL, State string }
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &response) != nil || response.State == "" {
			t.Fatalf("start: %d %s", w.Code, w.Body.String())
		}
		approval, err := url.Parse(response.URL)
		if err != nil || approval.Host != "gitlab.example.invalid" || approval.Query().Get("state") != response.State ||
			approval.Query().Get("code_challenge_method") != "S256" || approval.Query().Get("code_challenge") == "" {
			t.Fatal("authorization URL missing pinned origin or PKCE")
		}
		return response.State
	}

	state := start()
	if w := request(http.MethodGet, "/gitlab/callback?state="+state+"&code=fake-callback", "", false); w.Code != http.StatusOK {
		t.Fatalf("valid callback: %d", w.Code)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		status := request(http.MethodGet, "/v0/management/get-auth-status?state="+state, "", true)
		if strings.Contains(status.Body.String(), `"saved":true`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("completion: %d %s", status.Code, status.Body.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	oauth, err := os.ReadFile(filepath.Join(dir, "gitlab-fixture-user.json"))
	if err != nil || !strings.Contains(string(oauth), `"refresh_token":"fake-refresh"`) {
		t.Fatalf("OAuth file missing saved grant: %v", err)
	}

	cancelled := start()
	if w := request(http.MethodDelete, "/v0/management/oauth-session?state="+cancelled, "", true); !strings.Contains(w.Body.String(), `"cancelled":true`) {
		t.Fatalf("cancel: %d %s", w.Code, w.Body.String())
	}
	if w := request(http.MethodGet, "/gitlab/callback?state="+cancelled+"&code=fake-callback", "", false); w.Code != http.StatusBadRequest {
		t.Fatalf("cancelled callback: %d", w.Code)
	}
	if requests.Load() != 2 {
		t.Fatalf("cancelled callback reached upstream transport: %d", requests.Load())
	}

	if w := request(http.MethodPost, entry, `{"personal_access_token":"rejected-redirection"}`, true); w.Code != http.StatusBadGateway {
		t.Fatalf("redirected PAT: %d", w.Code)
	}
	if requests.Load() != 3 {
		t.Fatalf("unsafe redirect followed: %d HTTP round trips", requests.Load())
	}
	if w := request(http.MethodPost, entry, `{"personal_access_token":"fake-pat"}`, true); w.Code != http.StatusOK || strings.Contains(w.Body.String(), "fake-pat") {
		t.Fatalf("PAT import HTTP response: %d %s", w.Code, w.Body.String())
	}
	path := filepath.Join(dir, "gitlab-fixture-user-pat.json")
	pat, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(pat), `"personal_access_token":"fake-pat"`) || !strings.Contains(string(pat), `"pat_scopes":["api"]`) {
		t.Fatalf("PAT file missing stored grant: %v", err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("PAT file permissions: %v %v", info, err)
	}
}
