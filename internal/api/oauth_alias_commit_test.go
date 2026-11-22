package api

import (
	"bytes"
	"encoding/json"
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

func TestRegisteredOAuthModelAliasCommitsBeforeVisibility(t *testing.T) {
	const v0 = "/v0/management/oauth-model-alias"
	const v8 = "/v8/management/config/oauth/model-alias"
	const admin = "ordinary-alias-fixture-admin"
	const seed = `{"codex":[{"name":"fixture-old","alias":"old"}],"claude":[{"name":"fixture-sibling","alias":"keep","fork":true}]}`
	const replacement = `[{"name":"fixture-new","alias":"new"}]`
	const sibling = `[{"name":"fixture-sibling","alias":"keep","fork":true}]`
	t.Setenv("MANAGEMENT_PASSWORD", admin)

	for _, tc := range []struct {
		name, method, suffix, body, want string
	}{
		{"put", "PUT", "", `{"codex":` + replacement + `,"claude":` + sibling + `}`, `{"codex":` + replacement + `,"claude":` + sibling + `}`},
		{"patch replace", "PATCH", "", `{"channel":"codex","aliases":` + replacement + `}`, `{"codex":` + replacement + `,"claude":` + sibling + `}`},
		{"patch empty", "PATCH", "", `{"provider":"codex","aliases":[]}`, `{"claude":` + sibling + `}`},
		{"delete", "DELETE", "?channel=codex", "", `{"claude":` + sibling + `}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			must := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}
			must(os.WriteFile(path, []byte("port: 8317\nserver: {server-id: 11111111-1111-4111-8111-111111111111}\n"), 0600))
			open := func() *Server {
				t.Helper()
				cfg, err := config.LoadConfig(path)
				must(err)
				server := &Server{cfg: cfg, engine: gin.New(), mgmt: management.NewHandler(cfg, path, nil)}
				server.managementRoutesEnabled.Store(true)
				server.registerManagementRoutes()
				return server
			}
			send := func(server *Server, method, route, body string) *httptest.ResponseRecorder {
				request := httptest.NewRequest(method, route, strings.NewReader(body))
				request.RemoteAddr = "127.0.0.1:10000"
				request.Header.Set("X-Management-Key", admin)
				request.Header.Set("Content-Type", "application/json")
				rec := httptest.NewRecorder()
				server.engine.ServeHTTP(rec, request)
				return rec
			}
			call := func(server *Server, method, route, body string, status int) []byte {
				t.Helper()
				rec := send(server, method, route, body)
				if rec.Code != status {
					t.Fatalf("%s %s: status %d, want %d: %s", method, route, rec.Code, status, rec.Body)
				}
				return rec.Body.Bytes()
			}
			type aliases = map[string][]config.OAuthModelAlias
			decode := func(raw []byte) aliases {
				t.Helper()
				var value aliases
				must(json.Unmarshal(raw, &value))
				return value
			}
			read := func(server *Server, route string, want aliases) {
				t.Helper()
				raw := call(server, "GET", route, "", 200)
				if route == v0 {
					var envelope map[string]json.RawMessage
					must(json.Unmarshal(raw, &envelope))
					raw = envelope["oauth-model-alias"]
				}
				if got := decode(raw); !reflect.DeepEqual(got, want) {
					t.Errorf("%s alias readback = %#v, want %#v", route, got, want)
				}
			}
			server := open()
			call(server, "PUT", v0, seed, 200)
			read(server, v0, decode([]byte(seed)))
			before, err := os.ReadFile(path)
			must(err)
			saved := path + ".saved"
			must(os.Rename(path, saved))
			must(os.Mkdir(path, 0700))
			call(server, tc.method, v0+tc.suffix, tc.body, 500)
			read(server, v0, decode([]byte(seed)))
			after, err := os.ReadFile(saved)
			must(err)
			if !bytes.Equal(before, after) {
				t.Fatal("failed write changed the committed config bytes")
			}
			must(os.Remove(path))
			must(os.Rename(saved, path))
			read(server, v8, decode([]byte(seed)))
			call(server, tc.method, v0+tc.suffix, tc.body, 200)
			want := decode([]byte(tc.want))
			read(server, v0, want)
			read(server, v8, want)
			cfg, err := config.LoadConfig(path)
			must(err)
			if !reflect.DeepEqual(cfg.OAuthModelAlias, want) {
				t.Fatalf("cold alias readback = %#v, want %#v", cfg.OAuthModelAlias, want)
			}

			if tc.name == "patch replace" {
				start := make(chan struct{})
				statuses := make(chan int, 3)
				for _, channel := range []string{"alpha", "beta"} {
					go func(channel string) {
						<-start
						body := `{"channel":"` + channel + `","aliases":` + replacement + `}`
						statuses <- send(server, "PATCH", v0, body).Code
					}(channel)
				}
				go func() {
					<-start
					statuses <- send(server, "GET", v0, "").Code
				}()
				close(start)
				for i := 0; i < 3; i++ {
					if code := <-statuses; code != 200 {
						t.Errorf("concurrent alias operation returned %d", code)
					}
				}
				want["alpha"] = want["codex"]
				want["beta"] = want["codex"]
				read(server, v0, want)
				read(server, v8, want)
				read(open(), v0, want)
			}
		})
	}
}
