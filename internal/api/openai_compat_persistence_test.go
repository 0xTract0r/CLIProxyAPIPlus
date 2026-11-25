package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v8/sdk/access"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestOpenAICompatibilityRejectedSaveStaysUnpublished(t *testing.T) {
	const endpoint = "/v0/management/openai-compatibility"
	const sibling = "/v0/management/logging-to-file"
	const managementKey = "local-provider-persistence-fixture"
	type provider struct {
		Name  string
		Alias string
	}
	initial := provider{"fixture", "original"}
	t.Setenv("MANAGEMENT_PASSWORD", managementKey)
	for _, tc := range []struct {
		name, method, query, body string
		want                      provider
	}{
		{"put", "PUT", "", `[{"name":" changed ","base-url":" https://provider.invalid/v1 ","disabled":true,"models":[{"name":"synthetic-model","alias":"put-alias"}]}]`, provider{"changed", "put-alias"}},
		{"patch_name_fallback", "PATCH", "", `{"index":10,"name":" fixture ","value":{"name":" patched ","models":[{"name":"synthetic-model","alias":"patch-alias"}]}}`, provider{"patched", "patch-alias"}},
		{"patch_blank_base_url", "PATCH", "", `{"index":0,"value":{"base-url":"   "}}`, provider{}},
		{"delete_name", "DELETE", "?name=fixture", "", provider{}},
		{"delete_index", "DELETE", "?index=0", "", provider{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("WRITABLE_PATH", root)
			path := filepath.Join(root, "config.yaml")
			seed := []byte(`# fixture comments
openai-compatibility:
  - name: fixture
    disabled: true
    base-url: https://provider.invalid/v1
    models:
      - name: synthetic-model
        alias: original
logging-to-file: false
`)
			if err := os.WriteFile(path, seed, 0600); err != nil {
				t.Fatal(err)
			}
			read := func(name string) []byte {
				t.Helper()
				raw, err := os.ReadFile(name)
				if err != nil {
					t.Fatal(err)
				}
				return raw
			}
			load := func() *config.Config {
				t.Helper()
				cfg, err := config.LoadConfig(path)
				if err != nil {
					t.Fatal(err)
				}
				return cfg
			}
			cfg := load()
			cfg.AuthDir = root
			cfg.RemoteManagement.DisableControlPanel = true
			reloads := make(chan *config.Config, 4)
			server := NewServer(cfg, coreauth.NewManager(nil, nil, nil), sdkaccess.NewManager(), path,
				WithConfigReloadHook(func(_ context.Context, snapshot *config.Config) { reloads <- snapshot }))
			call := func(method, url, body string, status int) []byte {
				t.Helper()
				req := httptest.NewRequest(method, url, strings.NewReader(body))
				req.RemoteAddr = "127.0.0.1:32599"
				req.Header.Set("Authorization", "Bearer "+managementKey)
				req.Header.Set("Content-Type", "application/json")
				rec := httptest.NewRecorder()
				server.Handler().ServeHTTP(rec, req)
				if rec.Code != status {
					t.Fatalf("%s %s returned %d, want %d: %s", method, url, rec.Code, status, rec.Body.String())
				}
				return rec.Body.Bytes()
			}
			state := func(entries []config.OpenAICompatibility) provider {
				t.Helper()
				if len(entries) == 0 {
					return provider{}
				}
				if len(entries) != 1 || len(entries[0].Models) != 1 ||
					entries[0].Models[0].Name != "synthetic-model" || entries[0].BaseURL != "https://provider.invalid/v1" ||
					!entries[0].Disabled || len(entries[0].APIKeyEntries) != 0 {
					t.Fatalf("unexpected ordinary provider shape: %#v", entries)
				}
				return provider{entries[0].Name, entries[0].Models[0].Alias}
			}
			get := func() provider {
				t.Helper()
				var response struct {
					Entries []config.OpenAICompatibility `json:"openai-compatibility"`
				}
				if err := json.Unmarshal(call("GET", endpoint, "", http.StatusOK), &response); err != nil {
					t.Fatal(err)
				}
				return state(response.Entries)
			}
			onDisk := func(stage string, want provider, logging bool) {
				t.Helper()
				cold := load()
				if got := state(cold.OpenAICompatibility); got != want || cold.LoggingToFile != logging {
					t.Errorf("%s: cold file provider=%+v logging=%t, want %+v logging=%t", stage, got, cold.LoggingToFile, want, logging)
				}
			}
			reloaded := func(stage string, want provider) {
				t.Helper()
				select {
				case snapshot := <-reloads:
					if got := state(snapshot.OpenAICompatibility); got != want || snapshot.SourceHash != config.ComputeSourceHash(read(path)) {
						t.Errorf("%s: reload provider=%+v, want %+v with current disk hash", stage, got, want)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("successful save missed reload")
				}
			}
			if got := get(); got != initial {
				t.Fatalf("initial GET=%+v, want %+v", got, initial)
			}
			before := read(path)
			oldHash := cfg.SourceHash
			backup := path + ".before"
			if err := os.Rename(path, backup); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(path, 0700); err != nil {
				t.Fatal(err)
			}
			var failure struct{ Error string }
			if err := json.Unmarshal(call(tc.method, endpoint+tc.query, tc.body, http.StatusInternalServerError), &failure); err != nil ||
				!strings.HasPrefix(failure.Error, "failed to save config: ") {
				t.Fatalf("wrong public write error: %q (%v)", failure.Error, err)
			}
			if !bytes.Equal(read(backup), before) {
				t.Fatal("failed save changed existing file bytes")
			}
			if got := get(); got != initial || cfg.SourceHash != oldHash {
				t.Errorf("after 500: live GET=%+v, want %+v, original source hash kept=%t", got, initial, cfg.SourceHash == oldHash)
			}
			select {
			case <-reloads:
				t.Error("failed save invoked reload")
			default:
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(backup, path); err != nil {
				t.Fatal(err)
			}
			onDisk("after 500", initial, false)
			call("PUT", sibling, `{"value":true}`, http.StatusOK)
			reloaded("sibling save", initial)
			onDisk("sibling save", initial, true)
			// A leaked rename/delete can prevent the original selector from finding the item.
			if t.Failed() {
				return
			}
			call(tc.method, endpoint+tc.query, tc.body, http.StatusOK)
			reloaded("retry", tc.want)
			onDisk("retry", tc.want, true)
			if got := get(); got != tc.want {
				t.Errorf("after retry: GET=%+v, want %+v", got, tc.want)
			}
			if _, err := os.Stat(backup); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("fault fixture was not restored: %v", err)
			}
		})
	}
}
