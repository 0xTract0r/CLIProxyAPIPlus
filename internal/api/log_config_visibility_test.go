package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api/handlers/management"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestLogSettingsCommitBeforeVisibility(t *testing.T) {
	const secret = "synthetic-manager-key-log-fixture"
	t.Setenv("MANAGEMENT_PASSWORD", secret)
	for _, tc := range []struct{ route, first, secondInput, second, fail string }{
		{"logging-to-file", "true", "false", "false", "true"},
		{"request-log", "true", "false", "false", "true"},
		{"logs-max-total-size-mb", "51", "-3", "0", "91"},
		{"error-logs-max-files", "11", "-3", "10", "23"},
	} {
		t.Run(tc.route, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.yaml")
			logDir := t.TempDir()
			if err := os.WriteFile(path, []byte(fmt.Sprintf("config-version: 8\n# logging fixture\nserver: {port: 8317, server-id: 11111111-1111-4111-8111-111111111111}\nauth-dir: %q\n", dir)), 0600); err != nil {
				t.Fatal(err)
			}
			open := func() *Server {
				t.Helper()
				cfg, err := config.LoadConfig(path)
				if err != nil {
					t.Fatal(err)
				}
				server := &Server{cfg: cfg, engine: gin.New(), mgmt: management.NewHandler(cfg, path, nil)}
				server.mgmt.SetLogDirectory(logDir)
				server.managementRoutesEnabled.Store(true)
				server.registerManagementRoutes()
				return server
			}
			send := func(s *Server, verb, route, raw string) *httptest.ResponseRecorder {
				req := httptest.NewRequest(verb, "/v0/management/"+route, strings.NewReader(raw))
				req.RemoteAddr = "127.0.0.1:10000"
				req.Header.Set("X-Management-Key", secret)
				req.Header.Set("Content-Type", "application/json")
				out := httptest.NewRecorder()
				s.engine.ServeHTTP(out, req)
				return out
			}
			request := func(s *Server, verb, raw string, status int) []byte {
				t.Helper()
				out := send(s, verb, tc.route, raw)
				if out.Code != status {
					t.Fatalf("%s status=%d expected=%d", verb, out.Code, status)
				}
				return out.Body.Bytes()
			}
			get := func(s *Server, want, phase string) {
				t.Helper()
				raw := request(s, http.MethodGet, "", http.StatusOK)
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(raw, &fields); err != nil {
					t.Fatal(err)
				}
				got := string(fields[tc.route])
				t.Logf("%s GET got=%s want=%s", phase, got, want)
				if got != want {
					t.Errorf("%s changed unexpectedly", phase)
				}
			}
			value := func(v string) string { return `{"value":` + v + `}` }
			s := open()
			reloads := make(chan *config.Config, 4)
			s.mgmt.SetConfigReloadHook(func(_ context.Context, cfg *config.Config) { reloads <- cfg })
			var committedHash string
			commit := func(method, input, want string) {
				t.Helper()
				out := request(s, method, value(input), http.StatusOK)
				if string(out) != `{"status":"ok"}` {
					t.Errorf("%s success response=%s", method, out)
				}
				get(s, want, method+" live")
				get(open(), want, method+" reopen")
				disk, err := config.ReadConfigFileSnapshot(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := config.ValidateV8Config(disk.Bytes()); err != nil {
					t.Errorf("%s lost v8 config layout: %v", method, err)
				}
				if !bytes.Contains(disk.Bytes(), []byte("# logging fixture")) {
					t.Errorf("%s lost the config comment", method)
				}
				committedHash = disk.SourceHash()
				select {
				case snapshot := <-reloads:
					if snapshot.SourceHash != committedHash {
						t.Errorf("%s callback did not receive a committed config snapshot", method)
					}
				case <-time.After(2 * time.Second):
					t.Errorf("%s missing config reload callback", method)
				}
			}
			commit(http.MethodPut, tc.first, tc.first)
			commit(http.MethodPatch, tc.secondInput, tc.second)
			request(s, http.MethodPatch, `{"value":null}`, http.StatusBadRequest)
			get(s, tc.second, "invalid PATCH")
			original, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			saved := path + ".saved"
			if err := os.Rename(path, saved); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(path, 0700); err != nil {
				t.Fatal(err)
			}
			failure := request(s, http.MethodPatch, value(tc.fail), http.StatusInternalServerError)
			var details struct{ Error string }
			if err := json.Unmarshal(failure, &details); err != nil || !strings.HasPrefix(details.Error, "failed to save config: ") {
				t.Errorf("failed save response=%s", failure)
			}
			get(s, tc.second, "failed save live")
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(saved, path); err != nil {
				t.Fatal(err)
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(original, after) || config.ComputeSourceHash(after) != committedHash {
				t.Fatal("failed save changed committed bytes")
			}
			get(open(), tc.second, "failed save reopen")
			select {
			case <-reloads:
				t.Error("failed or invalid write unexpectedly scheduled reload")
			default:
			}

			s.mgmt.SetConfigReloadHook(nil)
			// Use the same registered router concurrently so -race covers the scalar readers.
			start := make(chan struct{})
			var workers sync.WaitGroup
			workers.Go(func() {
				<-start
				for i := 0; i < 12; i++ {
					input := tc.first
					if i%2 != 0 {
						input = tc.secondInput
					}
					if out := send(s, http.MethodPatch, tc.route, value(input)); out.Code != http.StatusOK {
						t.Errorf("concurrent PATCH status=%d", out.Code)
					}
				}
			})
			for i := 0; i < 2; i++ {
				workers.Go(func() {
					<-start
					for i := 0; i < 24; i++ {
						out := send(s, http.MethodGet, tc.route, "")
						var fields map[string]json.RawMessage
						if err := json.Unmarshal(out.Body.Bytes(), &fields); err != nil {
							t.Errorf("concurrent GET invalid response: %v", err)
							continue
						}
						got := string(fields[tc.route])
						if out.Code != http.StatusOK || (got != tc.first && got != tc.second) {
							t.Errorf("concurrent GET status=%d value=%s", out.Code, got)
						}
					}
				})
			}
			switch tc.route {
			case "logging-to-file":
				workers.Go(func() {
					<-start
					for i := 0; i < 24; i++ {
						for _, method := range []string{http.MethodGet, http.MethodDelete} {
							out := send(s, method, "logs", "")
							if out.Code != http.StatusOK && out.Code != http.StatusBadRequest {
								t.Errorf("concurrent %s logs status=%d", method, out.Code)
							}
						}
					}
				})
			case "request-log":
				workers.Go(func() {
					<-start
					for i := 0; i < 24; i++ {
						if out := send(s, http.MethodGet, "request-error-logs", ""); out.Code != http.StatusOK {
							t.Errorf("concurrent request error log index status=%d", out.Code)
						}
					}
				})
			}
			close(start)
			workers.Wait()
			get(s, tc.second, "concurrent live")
			get(open(), tc.second, "concurrent reopen")
		})
	}
}
