package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api/handlers/management"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestRegisteredProviderListsCommitBeforeVisibility(t *testing.T) {
	const admin = "provider-list-test-admin"
	t.Setenv("MANAGEMENT_PASSWORD", admin)
	gin.SetMode(gin.TestMode)

	for _, provider := range []string{"gemini-api-key", "interactions-api-key", "vertex-api-key", "meta-api-key", "xai-api-key", "openai-compatibility"} {
		for _, method := range []string{"PUT", "PATCH", "DELETE"} {
			t.Run(provider+"/"+method, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "config.yaml")
				if err := os.WriteFile(path, []byte("# test configuration\nport: 8317\nserver: {server-id: 11111111-1111-4111-8111-111111111111}\n"), 0600); err != nil {
					t.Fatal(err)
				}
				open := func() *Server {
					t.Helper()
					cfg, err := config.LoadConfig(path)
					if err != nil {
						t.Fatal(err)
					}
					s := &Server{cfg: cfg, engine: gin.New(), mgmt: management.NewHandler(cfg, path, nil)}
					s.managementRoutesEnabled.Store(true)
					s.registerManagementRoutes()
					return s
				}
				send := func(server *Server, verb, endpoint string, body any) (*httptest.ResponseRecorder, error) {
					raw, err := json.Marshal(body)
					if err != nil {
						return nil, err
					}
					req := httptest.NewRequest(verb, "/v0/management/"+endpoint, bytes.NewReader(raw))
					req.RemoteAddr = "127.0.0.1:10000"
					req.Header.Set("X-Management-Key", admin)
					req.Header.Set("Content-Type", "application/json")
					rec := httptest.NewRecorder()
					server.engine.ServeHTTP(rec, req)
					return rec, nil
				}
				call := func(server *Server, verb, query string, body any, status int) []byte {
					t.Helper()
					rec, err := send(server, verb, provider+query, body)
					if err != nil {
						t.Fatal(err)
					}
					if rec.Code != status {
						t.Fatalf("%s %s returned %d, want %d", verb, provider, rec.Code, status)
					}
					return rec.Body.Bytes()
				}
				const a = "https://first.example.invalid/v1"
				const b = "https://sentinel.example.invalid/v1"
				const next = "https://updated.example.invalid/v1"
				const right = "https://parallel-second.example.invalid/v1"
				type fixtureKey struct {
					APIKey string `json:"api-key"`
				}
				urls := func(server *Server) []string {
					t.Helper()
					var out map[string][]struct {
						Name          string       `json:"name"`
						APIKey        string       `json:"api-key"`
						APIKeyEntries []fixtureKey `json:"api-key-entries"`
						BaseURL       string       `json:"base-url"`
					}
					if err := json.Unmarshal(call(server, "GET", "", nil, 200), &out); err != nil {
						t.Fatal(err)
					}
					var got []string
					for _, v := range out[provider] {
						id := "first"
						if v.BaseURL == b || v.BaseURL == right {
							id = "sentinel"
						}
						wantKey := "synthetic-" + id
						if provider == "openai-compatibility" {
							if v.Name != id || len(v.APIKeyEntries) != 1 || v.APIKeyEntries[0].APIKey != wantKey {
								t.Fatal("unexpected compatibility provider or nested key for target")
							}
						} else if v.APIKey != wantKey {
							t.Fatal("unexpected credential for target")
						}
						got = append(got, v.BaseURL)
					}
					return got
				}
				row := func(id, url string) map[string]any {
					if provider == "openai-compatibility" {
						return map[string]any{"name": id, "base-url": url, "api-key-entries": []fixtureKey{{APIKey: "synthetic-" + id}}}
					}
					return map[string]any{"api-key": "synthetic-" + id, "base-url": url}
				}
				server := open()
				initial := []map[string]any{row("first", a), row("sentinel", b)}
				call(server, "PUT", "", initial, 200)
				if got := urls(server); !slices.Equal(got, []string{a, b}) {
					t.Fatalf("initial read returned %d entries with unexpected targets", len(got))
				}
				var body any
				query := ""
				expected := []string{next, b}
				switch method {
				case "PUT":
					body = []map[string]any{row("first", next), row("sentinel", b)}
				case "PATCH":
					body = map[string]any{"index": 0, "value": map[string]string{"base-url": next}}
				case "DELETE":
					query = "?index=0"
					expected = []string{b}
				}
				original, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				saved := path + ".saved"
				if err = os.Rename(path, saved); err != nil {
					t.Fatal(err)
				}
				if err = os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
				rejected := call(server, method, query, body, http.StatusInternalServerError)
				if bytes.Contains(rejected, []byte("synthetic-")) {
					t.Fatal("save error exposed a fixture key")
				}
				snapshot, err := os.ReadFile(saved)
				if err != nil || !bytes.Equal(snapshot, original) {
					t.Fatal("failed mutation changed committed bytes")
				}
				if got := urls(server); !slices.Equal(got, []string{a, b}) {
					t.Errorf("failed %s became visible in this process", method)
				}
				if err = os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err = os.Rename(saved, path); err != nil {
					t.Fatal(err)
				}
				if got := urls(open()); !slices.Equal(got, []string{a, b}) {
					t.Fatal("failed mutation changed the reopened list")
				}
				call(server, method, query, body, 200)
				if got := urls(server); !slices.Equal(got, expected) {
					t.Fatalf("unexpected live list after %s", method)
				}
				if got := urls(open()); !slices.Equal(got, expected) {
					t.Fatalf("unexpected persisted list after %s", method)
				}
				if provider == "gemini-api-key" && method == http.MethodPatch {
					const left = "https://parallel-first.example.invalid/v1"
					var writers sync.WaitGroup

					for index, target := range []string{left, right} {
						writers.Go(func() {
							rec, err := send(server, http.MethodPatch, provider, map[string]any{
								"index": index, "value": map[string]string{"base-url": target},
							})
							if err != nil {
								t.Error(err)
							} else if rec.Code != http.StatusOK {
								t.Errorf("concurrent PATCH returned %d", rec.Code)
							}
						})
					}
					writers.Wait()
					for _, got := range [][]string{urls(server), urls(open())} {
						if !slices.Equal(got, []string{left, right}) {
							t.Fatalf("concurrent writes lost an independent edit: %v", got)
						}
					}
				}
				server.mgmt.SetConfig(nil)
				call(server, method, query, body, http.StatusServiceUnavailable)

			})
		}
	}
}
