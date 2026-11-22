//go:build darwin || linux

package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	fileauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
)

func TestOrdinaryCredentialDeleteUsesFileStoreLock(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	for _, route := range []string{ordinaryCredentialV0, ordinaryCredentialV8} {
		for _, tc := range []struct {
			name, query, body string
			code              int
		}{
			{"named", "?name=" + ordinaryCredentialFile, "", http.StatusOK},
			{"all", "?all=true", "", http.StatusOK},
			{"partial batch", "", `{"names":["synthetic-note.json","not-registered.json"]}`, http.StatusMultiStatus},
		} {
			t.Run(strings.Split(route, "/")[1]+"/"+tc.name, func(t *testing.T) {
				f := newOrdinaryCredentialFixture(t, route)
				entered := make(chan struct{})
				release := make(chan struct{})
				locked := make(chan error, 1)
				go func() {
					locked <- fileauth.WithExistingAuthFile(f.path, func(os.FileInfo) error {
						close(entered)
						<-release
						return nil
					})
				}()
				select {
				case <-entered:
				case err := <-locked:
					t.Fatalf("file lock ended before entering fixture: %v", err)
				case <-time.After(5 * time.Second):
					t.Fatal("file lock did not start")
				}
				done := make(chan *httptest.ResponseRecorder, 1)
				go func() {
					done <- ordinaryCredentialRequest(f.server, http.MethodDelete, route+tc.query, []byte(tc.body), true)
				}()
				select {
				case got := <-done:
					close(release)
					t.Fatalf("management deletion skipped file-store serialization: %d %s", got.Code, got.Body)
				case <-time.After(50 * time.Millisecond):
				}
				close(release)
				select {
				case err := <-locked:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("file lock did not end")
				}
				select {
				case got := <-done:
					if got.Code != tc.code {
						t.Fatalf("DELETE status=%d body=%s, want %d", got.Code, got.Body, tc.code)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("management deletion did not finish")
				}
				if _, err := os.Lstat(f.path); !os.IsNotExist(err) {
					t.Fatalf("deleted credential still exists: %v", err)
				}
			})
		}
	}
}
