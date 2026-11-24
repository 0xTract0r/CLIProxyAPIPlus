package api

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

const routeQAKey = "offline-fixture-management"
const routeQABase = "config-version: 8\nserver: {host: 127.0.0.7, port: 8317}\naccess: {api-keys: [retained-inbound-fixture]}\n"

type routeQA struct {
	t      *testing.T
	file   string
	server *Server
}

func newRouteQA(t *testing.T, contents string) routeQA {
	t.Helper()
	t.Setenv("MANAGEMENT_PASSWORD", routeQAKey)
	root := t.TempDir()
	t.Chdir(root)
	file := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(file, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	fixture := routeQA{t: t, file: file}
	fixture.server = fixture.reopen()
	return fixture
}

func (f routeQA) reopen() *Server {
	f.t.Helper()
	cfg, err := config.LoadConfig(f.file)
	if err != nil {
		f.t.Fatal(err)
	}
	cfg.AuthDir = filepath.Dir(f.file)
	cfg.Plugins.Dir = filepath.Join(cfg.AuthDir, "plugins")
	cfg.RemoteManagement.DisableControlPanel = true
	cfg.UsageStatisticsEnabled = true
	return NewServer(cfg, nil, nil, f.file, WithIsolatedUsage())
}

func (f routeQA) call(method, path, body, auth, etag string, status int) *httptest.ResponseRecorder {
	f.t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.RemoteAddr = "127.0.0.1:32010"
	r.Header.Set("Content-Type", "application/json")
	if strings.HasSuffix(path, ".yaml") {
		r.Header.Set("Content-Type", "application/yaml")
	}
	if auth != "" {
		r.Header.Set("Authorization", "Bearer "+auth)
	}
	if etag != "" {
		r.Header.Set("If-Match", etag)
	}
	w := httptest.NewRecorder()
	f.server.Handler().ServeHTTP(w, r)
	if w.Code != status {
		f.t.Fatalf("%s %s got %d want %d: %s", method, path, w.Code, status, w.Body.String())
	}
	return w
}

func routeQARead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A directory at the YAML path induces a real save error even as root or
// on systems where chmod does not deny writes. Preserve the original inode.
func routeQAWithBlockedConfigPath(t *testing.T, file string, check func()) {
	t.Helper()
	backup := file + ".fixture-backup"
	if err := os.Rename(file, backup); err != nil {
		t.Fatal(err)
	}
	createdDirectory := false
	defer func() {
		if createdDirectory {
			if err := os.Remove(file); err != nil {
				t.Error(err)
				return
			}
		}
		if err := os.Rename(backup, file); err != nil {
			t.Error(err)
		}
	}()
	if err := os.Mkdir(file, 0700); err != nil {
		t.Fatal(err)
	}
	createdDirectory = true
	check()
	entries, err := os.ReadDir(file)
	if err != nil || len(entries) != 0 {
		t.Fatalf("failed save altered the blocked config path: %v", err)
	}
}

func TestRouteBatchMetaWrites(t *testing.T) {
	const endpoint = "/v0/management/meta-api-key"
	const env = routeQABase + `api-keys:
  meta:
    - name: qa-group
      base-url: https://irrelevant.example.invalid
      keys:
        - api-key: fixture-meta-a
        - api-key: fixture-meta-b
`
	for _, item := range []struct {
		name, method, path, body, invalidPath, invalidBody, wanted, retained string
		invalidStatus                                                        int
	}{
		{"put", "PUT", endpoint, `[{"api-key":"fixture-meta-new","base-url":"https://irrelevant.example.invalid"},{"api-key":"fixture-meta-b","base-url":"https://irrelevant.example.invalid"}]`, endpoint, "{", "fixture-meta-new", "fixture-meta-b", 400},
		{"patch", "PATCH", endpoint, `{"match":"fixture-meta-a","value":{"api-key":"fixture-meta-new"}}`, endpoint, `{"match":"missing","value":{"prefix":"x"}}`, "fixture-meta-new", "fixture-meta-b", 404},
		{"delete", "DELETE", endpoint + "?index=0", "", "" + endpoint, "", "", "fixture-meta-b", 400},
	} {
		t.Run(item.name, func(t *testing.T) {
			f := newRouteQA(t, env)
			before := routeQARead(t, f.file)
			snapshot := func() []byte { return f.call("GET", endpoint, "", routeQAKey, "", 200).Body.Bytes() }
			original := snapshot()
			if !bytes.Contains(original, []byte("fixture-meta-a")) || !bytes.Contains(original, []byte(item.retained)) {
				t.Fatalf("Meta config fixture was not loaded: %s", original)
			}
			f.call(item.method, item.path, item.body, "", "", 401)
			f.call(item.method, item.invalidPath, item.invalidBody, routeQAKey, "", item.invalidStatus)
			if !bytes.Equal(original, snapshot()) || !bytes.Equal(before, routeQARead(t, f.file)) {
				t.Fatal("authorization or invalid request changed Meta state")
			}
			routeQAWithBlockedConfigPath(t, f.file, func() {
				w := f.call(item.method, item.path, item.body, routeQAKey, "", 500)
				if !strings.Contains(w.Body.String(), "failed to save config") {
					t.Fatalf("expected file write error, got %s", w.Body.String())
				}
			})
			afterFailure := snapshot()
			if !bytes.Equal(before, routeQARead(t, f.file)) {
				t.Error("save failure changed persisted configuration")
			}
			if !bytes.Equal(original, afterFailure) {
				t.Error("save failure published uncommitted Meta state to authorized GET")
			}
			// Retry on the same handler. A failed PATCH must not remove its match key,
			// and an index-based retry must not accidentally delete the next entry.
			f.call(item.method, item.path, item.body, routeQAKey, "", 200)
			live := snapshot()
			f.server = f.reopen()
			cold := snapshot()
			if !bytes.Equal(semanticMetaProjection(t, live), semanticMetaProjection(t, cold)) {
				t.Fatal("committed Meta fields differ between live and cold read")
			}
			if bytes.Contains(cold, []byte("fixture-meta-a")) || !bytes.Contains(cold, []byte(item.retained)) ||
				(item.wanted != "" && !bytes.Contains(cold, []byte(item.wanted))) {
				t.Fatal("successful mutation did not read back the selected Meta change after reopen")
			}
		})
	}
}

// Some legacy provider groups encode an empty model list as [] after a cold
// load and as null live. Both represent zero models; compare every other field.
func semanticMetaProjection(t *testing.T, data []byte) []byte {
	t.Helper()
	var view struct {
		Entries []map[string]json.RawMessage `json:"meta-api-key"`
	}
	if err := json.Unmarshal(data, &view); err != nil {
		t.Fatal(err)
	}
	for _, entry := range view.Entries {
		if bytes.Equal(entry["models"], []byte("null")) {
			entry["models"] = json.RawMessage(`[]`)
		}
	}
	out, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestRouteBatchMetaKeepsLegacySelectorsAndConcurrentSiblings(t *testing.T) {
	const endpoint = "/v0/management/meta-api-key"
	f := newRouteQA(t, routeQABase)
	fixture := `{"items":[{"api-key":"fixture-a","base-url":"https://a.example.invalid"},{"api-key":"fixture-b","base-url":"https://b.example.invalid"},{"api-key":"fixture-b","base-url":"https://b2.example.invalid"}]}`
	f.call("PUT", endpoint, fixture, routeQAKey, "", 200)
	before := routeQARead(t, f.file)
	f.call("DELETE", endpoint+"?api-key=fixture-b", "", routeQAKey, "", 400)
	if !bytes.Equal(before, routeQARead(t, f.file)) {
		t.Fatal("ambiguous legacy key selector changed configuration")
	}

	// Two list mutations and a normal unrelated provider-independent setting
	// must each apply to the latest committed state under the same Handler lock.
	run := []func(){
		func() {
			f.call("PATCH", endpoint, `{"match":"fixture-a","value":{"prefix":"new-prefix"}}`, routeQAKey, "", 200)
		},
		func() {
			f.call("DELETE", endpoint+"?api-key=fixture-b&base-url=https%3A%2F%2Fb2.example.invalid", "", routeQAKey, "", 200)
		},
		func() { f.call("PUT", "/v0/management/proxy-url", `{"value":"direct"}`, routeQAKey, "", 200) },
	}
	start := make(chan struct{})
	var pending sync.WaitGroup
	for _, action := range run {
		pending.Add(1)
		go func() {
			defer pending.Done()
			<-start
			action()
		}()
	}
	close(start)
	pending.Wait()
	cold, err := config.LoadConfig(f.file)
	if err != nil {
		t.Fatal(err)
	}
	if cold.ProxyURL != "direct" || len(cold.APIKeys) != 1 || cold.APIKeys[0] != "retained-inbound-fixture" || len(cold.MetaKey) != 2 ||
		cold.MetaKey[0].APIKey != "fixture-a" || cold.MetaKey[0].Prefix != "new-prefix" ||
		cold.MetaKey[1].APIKey != "fixture-b" || cold.MetaKey[1].BaseURL != "https://b.example.invalid" {
		t.Fatal("concurrent writes lost an unrelated config value, sibling Meta key, or selected mutation")
	}
	f.call("PUT", endpoint, `[]`, routeQAKey, "", 200)
	cold, err = config.LoadConfig(f.file)
	if err != nil || len(cold.MetaKey) != 0 || cold.ProxyURL != "direct" {
		t.Fatalf("legacy clear failed or changed unrelated configuration: %v", err)
	}
}
