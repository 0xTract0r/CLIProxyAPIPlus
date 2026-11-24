package management

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestLocalAPIKeySaveFailureAndRetry(t *testing.T) {
	const route = "/v0/management/api-keys"
	old := []string{"fixture-a", "fixture-a", "fixture-b"}
	for _, tc := range []struct {
		method, suffix, body string
		want                 []string
	}{
		{"PUT", "", `{"items":["fixture-x"]}`, []string{"fixture-x"}},
		{"PATCH", "", `{"old":"fixture-a","new":"fixture-x"}`, []string{"fixture-x", "fixture-a", "fixture-b"}},
		{"DELETE", "?index=90&value=fixture-a", "", []string{"fixture-b"}},
	} {
		t.Run(tc.method, func(t *testing.T) {
			h, path, router := newIssue6631Handler(t, "api-keys: [fixture-a, fixture-a, fixture-b]\nrequest-retry: 1\n")
			router.GET(route, h.GetAPIKeys)
			router.PUT(route, h.PutAPIKeys)
			router.PATCH(route, h.PatchAPIKeys)
			router.DELETE(route, h.DeleteAPIKeys)
			reloaded := make(chan *config.Config, 4)
			h.SetConfigReloadHook(func(_ context.Context, cfg *config.Config) { reloaded <- cfg })
			call := func(method, url, body string, want int) []byte {
				t.Helper()
				w := httptest.NewRecorder()
				r := httptest.NewRequest(method, url, strings.NewReader(body))
				r.Header.Set("Content-Type", "application/json")
				router.ServeHTTP(w, r)
				if w.Code != want {
					t.Fatalf("%s status %d want %d", method, w.Code, want)
				}
				return w.Body.Bytes()
			}
			live := func() []string {
				t.Helper()
				var result struct {
					Keys []string `json:"api-keys"`
				}
				if err := json.Unmarshal(call("GET", route, "", 200), &result); err != nil {
					t.Fatal(err)
				}
				return result.Keys
			}
			wantReload := func(want []string) {
				t.Helper()
				select {
				case cfg := <-reloaded:
					if !reflect.DeepEqual(cfg.APIKeys, want) || cfg.SourceHash == "" || cfg.SourceHash != h.cfg.SourceHash {
						t.Error("reload snapshot differs from published config")
					}
				case <-time.After(3 * time.Second):
					t.Fatal("reload did not run")
				}
			}
			backup := path + ".before"
			if err := os.Rename(path, backup); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(path, 0700); err != nil {
				t.Fatal(err)
			}
			priorHash := h.cfg.SourceHash
			call(tc.method, route+tc.suffix, tc.body, 500)
			if !slices.Equal(live(), old) || h.cfg.SourceHash != priorHash {
				t.Error("failed save published a list or a hash")
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(backup, path); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(readIssue6631Disk(t, path).APIKeys, old) {
				t.Error("failed save changed disk")
			}
			call("PUT", "/v0/management/request-retry", `{"value":2}`, 200)
			wantReload(old)
			if disk := readIssue6631Disk(t, path); !slices.Equal(disk.APIKeys, old) || disk.RequestRetry != 2 {
				t.Error("sibling save persisted a rejected API key edit")
			}
			call(tc.method, route+tc.suffix, tc.body, 200)
			wantReload(tc.want)
			if !slices.Equal(live(), tc.want) || !slices.Equal(readIssue6631Disk(t, path).APIKeys, tc.want) {
				t.Error("retry differs from committed list")
			}
		})
	}
}

func TestLocalAPIKeysEmptyJSONShape(t *testing.T) {
	h, _, r := newIssue6631Handler(t, "api-keys: [fixture-only]\n")
	r.PUT("/keys", h.PutAPIKeys)
	r.DELETE("/keys", h.DeleteAPIKeys)
	r.GET("/keys", h.GetAPIKeys)
	call := func(method, path, body string, want int) []byte {
		t.Helper()
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
		if w.Code != want {
			t.Fatalf("got %d want %d", w.Code, want)
		}
		return w.Body.Bytes()
	}
	for _, s := range []struct{ verb, path, body, want string }{
		{"DELETE", "/keys?index=0", "", "[]"},
		{"PUT", "/keys", "null", "null"},
		{"DELETE", "/keys?value=fixture-none", "", "[]"},
		{"PUT", "/keys", "[]", "null"},
	} {
		call(s.verb, s.path, s.body, 200)
		var wire map[string]json.RawMessage
		if err := json.Unmarshal(call("GET", "/keys", "", 200), &wire); err != nil {
			t.Fatal(err)
		}
		if string(wire["api-keys"]) != s.want {
			t.Errorf("%s %s: wire %s want %s", s.verb, s.path, wire["api-keys"], s.want)
		}
	}
}
