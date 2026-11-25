package management

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/copilot"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestOrdinaryCopilotDeviceOAuthRoutesAndScope(t *testing.T) {
	for _, name := range []string{"legacy save", "post save", "cancel before grant", "reload away and back", "startup reload", "grant refused", "target rejected", "invalid configured proxy"} {
		t.Run(name, func(t *testing.T) {
			dir, otherDir := t.TempDir(), t.TempDir()
			startCfg := &config.Config{AuthDir: dir, SDKConfig: config.SDKConfig{ProxyURL: "direct"}}
			if name == "invalid configured proxy" {
				startCfg.ProxyURL = "http://:8080"
			}
			h := &Handler{cfg: startCfg, tokenStore: sdkAuth.NewFileTokenStore()}
			other := &Handler{cfg: &config.Config{AuthDir: dir}, tokenStore: sdkAuth.NewFileTokenStore()}
			release := make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			t.Cleanup(unblock)
			returned := make(chan struct{})
			var starts atomic.Int32
			h.copilotDeviceGrant = &copilotDeviceGrant{
				start: func(_ context.Context, client *http.Client) (copilot.DeviceCode, error) {
					starts.Add(1)
					transport, ok := client.Transport.(*http.Transport)
					if !ok || transport.Proxy != nil || client.Timeout != 30*time.Second {
						t.Error("expected configured standard direct transport")
					}
					if name == "startup reload" {
						h.SetConfig(&config.Config{AuthDir: otherDir})
						h.SetConfig(&config.Config{AuthDir: dir})
					}
					return copilot.DeviceCode{VerificationURI: "https://fixture.invalid/verify", UserCode: "FIXTURE", ExpiresIn: 30}, nil
				},
				complete: func(context.Context, *http.Client, copilot.DeviceCode) (*coreauth.Auth, error) {
					defer close(returned)
					<-release
					if name == "grant refused" {
						return nil, errors.New("fixture grant refused")
					}
					return &coreauth.Auth{ID: "copilot-fixture.json", FileName: "copilot-fixture.json", Provider: copilot.Provider, Metadata: map[string]any{"type": copilot.Provider, "access_token": "synthetic"}}, nil
				},
			}
			router := gin.New()
			router.GET("/v0/management/github-auth-url", h.RequestCopilotDeviceOAuth)
			router.POST("/v8/management/oauth/auth-url", h.StartOAuthV8)
			router.DELETE("/v0/management/oauth-session", h.CancelAuthSession)
			otherRouter := gin.New()
			otherRouter.DELETE("/v0/management/oauth-session", other.CancelAuthSession)
			method, path, body := http.MethodGet, "/v0/management/github-auth-url", ""
			if name == "post save" || name == "target rejected" {
				method, path, body = http.MethodPost, "/v8/management/oauth/auth-url", `{"provider":"copilot"}`
			}
			if name == "target rejected" {
				body = `{"provider":"copilot","auth_name":"fixture.json"}`
			}
			reply := copilotFixtureRequest(router, method, path, body)
			want := http.StatusOK
			switch name {
			case "target rejected":
				want = http.StatusBadRequest
			case "invalid configured proxy":
				want = http.StatusServiceUnavailable
			case "startup reload":
				want = http.StatusConflict
			}
			if reply.Code != want {
				t.Fatalf("start status %d, want %d; %s", reply.Code, want, reply.Body)
			}
			if name == "target rejected" || name == "invalid configured proxy" {
				if starts.Load() != 0 {
					t.Fatal("rejected request reached grant")
				}
				return
			}
			if name == "startup reload" {
				return
			}
			var start map[string]any
			if err := json.Unmarshal(reply.Body.Bytes(), &start); err != nil {
				t.Fatal(err)
			}
			state, _ := start["state"].(string)
			if starts.Load() != 1 || state == "" || start["url"] != start["verification_uri"] || start["user_code"] != "FIXTURE" || start["flow"] != "device" {
				t.Fatalf("unexpected device response: %v", start)
			}
			if _, ok, _ := oauthSessions.pollScoped(state, other.oauthFileScope()); ok {
				t.Fatal("other handler could read device state")
			}
			qpath := "/v0/management/oauth-session?state=" + url.QueryEscape(state)
			if refused := copilotFixtureRequest(otherRouter, http.MethodDelete, qpath, ""); !strings.Contains(refused.Body.String(), `"cancelled":false`) {
				t.Fatal("other handler could cancel")
			}
			t.Cleanup(func() { CancelOAuthSession(state) })
			ownerScope := h.oauthFileScope()
			switch name {
			case "cancel before grant":
				if r := copilotFixtureRequest(router, http.MethodDelete, qpath, ""); !strings.Contains(r.Body.String(), `"cancelled":true`) {
					t.Fatal("owner could not cancel")
				}
			case "reload away and back":
				h.SetConfig(&config.Config{AuthDir: otherDir})
				h.SetConfig(&config.Config{AuthDir: dir})
				if _, ok, _ := oauthSessions.pollScoped(state, h.oauthFileScope()); ok {
					t.Fatal("new generation can see old state")
				}
			}
			unblock()
			select {
			case <-returned:
			case <-time.After(2 * time.Second):
				t.Fatal("grant was not collected")
			}
			wantFile := name == "legacy save" || name == "post save"
			deadline := time.Now().Add(2 * time.Second)
			for {
				session, ok, cancelled := oauthSessions.pollScoped(state, ownerScope)
				switch {
				case wantFile && ok && session.Completed:
				case name == "grant refused" && ok && session.Status != "":
				case name == "cancel before grant" && cancelled:
				case name == "reload away and back" && cancelled:
				default:
					if time.Now().After(deadline) {
						t.Fatalf("session not settled: %v / %v", ok, session.Status)
					}
					time.Sleep(time.Millisecond)
					continue
				}
				break
			}
			for _, where := range []string{dir, otherDir} {
				raw, err := os.ReadFile(filepath.Join(where, "copilot-fixture.json"))
				if where == dir && wantFile {
					if err != nil {
						t.Fatal(err)
					}
					if !strings.Contains(string(raw), "synthetic") {
						t.Fatal("unexpected persisted fixture")
					}
				} else if !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("unexpected persisted credential: %v", err)
				}
			}
		})
	}
}

func copilotFixtureRequest(router *gin.Engine, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}
