package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
)

func TestAPIKeyListConcurrentPersistence(t *testing.T) {
	const route = "/v0/management/api-keys"
	const initial = "port: 16345\napi-keys: [local-alpha, local-beta, local-gamma]\n"
	original := []string{"local-alpha", "local-beta", "local-gamma"}
	for _, tc := range []struct {
		name string
		fail bool
	}{
		{name: "failed_writes_preserve_published_list", fail: true},
		{name: "successful_writes_match_disk"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, path, router := newIssue6631Handler(t, initial)
			router.GET(route, h.GetAPIKeys)
			router.PUT(route, h.PutAPIKeys)
			router.PATCH(route, h.PatchAPIKeys)
			router.DELETE(route, h.DeleteAPIKeys)
			request := func(method, query, body string) *httptest.ResponseRecorder {
				r := httptest.NewRequest(method, route+query, strings.NewReader(body))
				r.Header.Set("Content-Type", "application/json")
				w := httptest.NewRecorder()
				router.ServeHTTP(w, r)
				return w
			}
			read := func() []string {
				w := request(http.MethodGet, "", "")
				var response struct {
					Keys []string `json:"api-keys"`
				}
				if err := json.Unmarshal(w.Body.Bytes(), &response); w.Code != http.StatusOK || err != nil {
					t.Errorf("GET status=%d decode=%v", w.Code, err)
				}
				return response.Keys
			}

			backup := path + ".backup"
			if tc.fail {
				if err := os.Rename(path, backup); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			wantStatus := http.StatusOK
			if tc.fail {
				wantStatus = http.StatusInternalServerError
			}
			mutations := []struct{ method, query, body string }{
				{http.MethodPut, "", `["local-put","local-beta","local-gamma"]`},
				{http.MethodPatch, "", `{"index":0,"value":"local-patch"}`},
				{http.MethodDelete, "?value=local-beta", ""},
			}
			var workers sync.WaitGroup
			start := make(chan struct{})
			for range 4 {
				for _, mutation := range mutations {
					workers.Go(func() {
						<-start
						got := request(mutation.method, mutation.query, mutation.body)
						if got.Code != wantStatus {
							t.Errorf("%s status=%d, want %d", mutation.method, got.Code, wantStatus)
						}
					})
				}
				workers.Go(func() {
					<-start
					got := read()
					if tc.fail && !slices.Equal(got, original) {
						t.Errorf("GET published uncommitted API keys: %v", got)
					}
				})
			}
			close(start)
			workers.Wait()

			if tc.fail {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(backup, path); err != nil {
					t.Fatal(err)
				}
			}
			current := read()
			onDisk := readIssue6631Disk(t, path).APIKeys
			if !slices.Equal(current, onDisk) {
				t.Errorf("management GET differs from reopened configuration: %v vs %v", current, onDisk)
			}
			if tc.fail && !slices.Equal(onDisk, original) {
				t.Errorf("failed writes changed configuration: %v", onDisk)
			}
		})
	}
}
