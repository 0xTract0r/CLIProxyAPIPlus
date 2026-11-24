package management

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
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

func TestOrdinaryCopilotV8DeviceLoginTargetsOneVerifiedFile(t *testing.T) {
	const name = "selected-fixture.json"
	const proxy = "http://enterprise.fixture.invalid:8080"
	const override = "http://wrong.fixture.invalid:8080"
	for _, c := range []struct {
		name string
		want int
	}{
		{"target same subject", 200},
		{"target inferred", 200},
		{"target by index", 200},
		{"conflicting selectors", 400},
		{"same requested route", 200},
		{"override denied", 400},
		{"global route cannot be overridden", 400},
		{"missing stable ID", 409},
		{"provider mismatch", 400},
		{"wrong login subject", 200},
		{"login failure", 200},
		{"file replaced", 200},
		{"cancel before grant", 200},
		{"auth directory reload", 200},
		{"global route reload", 200},
		{"runtime revocation", 200},
		{"new login explicit enterprise proxy", 200},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir, other := t.TempDir(), t.TempDir()
			f := filepath.Join(dir, name)
			fileProxy := proxy
			if c.name == "global route cannot be overridden" || c.name == "global route reload" {
				fileProxy = ""
			}
			provider := copilot.Provider
			if c.name == "provider mismatch" {
				provider = "claude"
			}
			metadata := map[string]any{"type": provider, "account_id": "42", "proxy_url": fileProxy, "note": "fixture note", "access_token": "old-fixture"}
			if c.name == "missing stable ID" {
				delete(metadata, "account_id")
				metadata["username"] = "fixture"
			}
			old, err := json.Marshal(metadata)
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(f, old, 0600); err != nil {
				t.Fatal(err)
			}
			store := sdkAuth.NewFileTokenStore()
			manager := coreauth.NewManager(store.WithBaseDir(dir), nil, nil)
			record := &coreauth.Auth{ID: name, FileName: name, Provider: provider, ProxyURL: fileProxy, Attributes: map[string]string{coreauth.AttributePath: f}, Metadata: metadata}
			if _, err = manager.Register(coreauth.WithSkipPersist(t.Context()), record); err != nil {
				t.Fatal(err)
			}
			h := &Handler{cfg: &config.Config{AuthDir: dir, SDKConfig: config.SDKConfig{ProxyURL: "direct"}}, tokenStore: store, authManager: manager}
			if c.name == "global route reload" {
				h.cfg.ProxyURL = proxy
			}
			router := gin.New()
			router.POST("/v8/management/oauth/auth-url", h.StartOAuthV8)
			router.DELETE("/v0/management/oauth-session", h.CancelAuthSession)

			released := make(chan struct{})
			var once sync.Once
			release := func() { once.Do(func() { close(released) }) }
			t.Cleanup(release)
			var calls atomic.Int32
			returned := make(chan struct{})
			h.copilotDeviceGrant = &copilotDeviceGrant{
				start: func(_ context.Context, client *http.Client) (copilot.DeviceCode, error) {
					calls.Add(1)
					transport, ok := client.Transport.(*http.Transport)
					if !ok || transport.Proxy == nil {
						t.Error("expected legal corporate proxy transport")
					} else {
						r := &http.Request{URL: &url.URL{Scheme: "https", Host: "fixture.invalid"}}
						got, err := transport.Proxy(r)
						if err != nil || got == nil || got.String() != proxy {
							t.Error("incorrect authorized proxy transport")
						}
					}
					return copilot.DeviceCode{VerificationURI: "https://fixture.invalid/verify", UserCode: "FIXTURE", ExpiresIn: 30}, nil
				},
				complete: func(context.Context, *http.Client, copilot.DeviceCode) (*coreauth.Auth, error) {
					defer close(returned)
					<-released
					if c.name == "login failure" {
						return nil, errors.New("fixture denied")
					}
					subject := "42"
					if c.name == "wrong login subject" {
						subject = "43"
					}
					newMeta := map[string]any{"type": copilot.Provider, "account_id": subject, "access_token": "new-fixture", "refresh_token": "refresh-fixture", "username": "fixture-renamed", "base_url": "https://fixture.invalid/v1", "expired": "2099-01-01T00:00:00Z"}
					return &coreauth.Auth{ID: "copilot-42.json", FileName: "copilot-42.json", Provider: copilot.Provider, Metadata: newMeta}, nil
				},
			}
			startBody := map[string]string{"provider": "copilot", "name": name}
			switch c.name {
			case "target inferred":
				delete(startBody, "provider")
			case "target by index":
				delete(startBody, "name")
				current, _ := manager.GetByID(name)
				startBody["auth_index"] = lockedAuthIndex(current)
				if startBody["auth_index"] == "" {
					t.Fatal("empty fixture index")
				}
			case "conflicting selectors":
				startBody["target_auth_id"] = "different-fixture.json"
			case "same requested route":
				startBody["proxy_url"] = proxy
			case "override denied", "global route cannot be overridden":
				startBody["proxy_url"] = override
			case "new login explicit enterprise proxy":
				delete(startBody, "name")
				startBody["proxy_url"] = proxy
			}
			body, _ := json.Marshal(startBody)
			reply := copilotFixtureRequest(router, http.MethodPost, "/v8/management/oauth/auth-url", string(body))
			if reply.Code != c.want {
				t.Fatalf("status=%d want %d: %s", reply.Code, c.want, reply.Body)
			}
			if c.want != 200 {
				if calls.Load() != 0 {
					t.Fatal("rejected request reached fake grant")
				}
				return
			}
			var response struct {
				State  string `json:"state"`
				Target string `json:"target_auth_name"`
			}
			if err = json.Unmarshal(reply.Body.Bytes(), &response); err != nil || response.State == "" {
				t.Fatalf("invalid response: %v", err)
			}
			state := response.State
			t.Cleanup(func() { CancelOAuthSession(state) })
			if c.name != "new login explicit enterprise proxy" && response.Target != name {
				t.Fatalf("wrong target echo %q", response.Target)
			}
			originalScope := h.oauthFileScope()
			switch c.name {
			case "file replaced":
				if err = os.Rename(f, f+".old"); err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(f, old, 0600); err != nil {
					t.Fatal(err)
				}
			case "cancel before grant":
				rec := copilotFixtureRequest(router, http.MethodDelete, "/v0/management/oauth-session?state="+url.QueryEscape(state), "")
				if !strings.Contains(rec.Body.String(), `"cancelled":true`) {
					t.Fatal("cancel refused")
				}
			case "auth directory reload":
				h.SetConfig(&config.Config{AuthDir: other})
			case "global route reload":
				h.SetConfig(&config.Config{AuthDir: dir, SDKConfig: config.SDKConfig{ProxyURL: "direct"}})
			case "runtime revocation":
				a, _ := manager.GetByID(name)
				a.Disabled = true
				if _, err = manager.Update(coreauth.WithSkipPersist(t.Context()), a); err != nil {
					t.Fatal(err)
				}
			}
			release()
			select {
			case <-returned:
			case <-time.After(2 * time.Second):
				t.Fatal("grant did not return")
			}
			deadline := time.Now().Add(2 * time.Second)
			for {
				session, ok, cancelled := oauthSessions.pollScoped(state, originalScope)
				if cancelled || (ok && (session.Completed || session.Status != "")) {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("session did not settle")
				}
				time.Sleep(time.Millisecond)
			}
			disk, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			if c.name == "target same subject" || c.name == "target inferred" || c.name == "target by index" || c.name == "same requested route" {
				if !strings.Contains(string(disk), `"new-fixture"`) || !strings.Contains(string(disk), `"fixture note"`) || !strings.Contains(string(disk), proxy) {
					t.Fatal("verified grant did not update only target")
				}
			} else if string(disk) != string(old) {
				t.Fatal("wrong target or failed login changed original file")
			}
			createdPath := filepath.Join(dir, "copilot-42.json")
			fresh, err := os.ReadFile(createdPath)
			if c.name == "new login explicit enterprise proxy" {
				if err != nil || !strings.Contains(string(fresh), proxy) {
					t.Fatal("new credential did not retain explicit enterprise route")
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				t.Fatal("targeted login created a replacement file")
			}
			if files, _ := os.ReadDir(other); len(files) != 0 {
				t.Fatal("credential written to another directory")
			}
		})
	}
}
