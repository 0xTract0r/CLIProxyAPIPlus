package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"gopkg.in/yaml.v3"
)

// A comment-preserving management save must keep an actual YAML boolean.
func TestManagementQuotedRequestLogPersistsTypedBoolean(t *testing.T) {
	for _, tc := range []struct {
		name, source string
		initial, v8  bool
	}{
		{"legacy_quoted_yes", "request-log: 'yes'\n", true, false},
		{"legacy_quoted_off", "request-log: \"off\"\n", false, false},
		{"v8_quoted_off", "config-version: 8\nobservability: {logs: {request-log: 'off'}}\n", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const key = "temporary-synthetic-admin-key"
			t.Setenv("MANAGEMENT_PASSWORD", key)
			s := newTestServerWithConfig(t, &config.Config{})
			if err := os.WriteFile(s.configFilePath, []byte("port: 16743\n"), 0600); err != nil {
				t.Fatal(err)
			}
			call := func(method, route, data string) *httptest.ResponseRecorder {
				t.Helper()
				req := httptest.NewRequest(method, "/v0/management/"+route, strings.NewReader(data))
				req.RemoteAddr = "127.0.0.1:1234"
				req.Header.Set("Authorization", "Bearer "+key)
				req.Header.Set("Content-Type", "application/json")
				rec := httptest.NewRecorder()
				s.Handler().ServeHTTP(rec, req)
				return rec
			}
			memory := func() (int, bool) {
				t.Helper()
				w := call("GET", "request-log", "")
				var p map[string]bool
				if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
					t.Fatal(err)
				}
				return w.Code, p["request-log"]
			}
			initial := call("PUT", "config.yaml", tc.source)
			code, beforeVal := memory()
			if initial.Code != 200 || code != 200 || beforeVal != tc.initial {
				t.Fatalf("initial YAML=%d GET=%d bool=%t", initial.Code, code, beforeVal)
			}
			before, err := os.ReadFile(s.configFilePath)
			if err != nil {
				t.Fatal(err)
			}
			toggle := call("PUT", "request-log", fmt.Sprintf(`{"value":%t}`, !tc.initial))
			code, runtime := memory()
			if code != 200 || toggle.Code != 200 {
				t.Fatalf("post toggle=%d GET=%d", toggle.Code, code)
			}
			after, err := os.ReadFile(s.configFilePath)
			if err != nil {
				t.Fatal(err)
			}
			var tree struct {
				Legacy        any `yaml:"request-log"`
				Observability struct{ Logs map[string]any }
			}
			if err := yaml.Unmarshal(after, &tree); err != nil {
				t.Fatal(err)
			}
			diskValue := tree.Legacy
			if tc.v8 {
				diskValue = tree.Observability.Logs["request-log"]
			}
			reopened, reopenErr := config.LoadConfig(s.configFilePath)
			reopenVal := false
			if reopenErr == nil {
				reopenVal = reopened.RequestLog
			}
			t.Logf("initial YAML=%d GET=200/%t; toggle=%d GET=%d/%t; bytes unchanged=%t scalar=%T(%v); reopen bool=%t error=%v", initial.Code, beforeVal, toggle.Code, code, runtime, bytes.Equal(before, after), diskValue, diskValue, reopenVal, reopenErr)
			if runtime != !tc.initial || bytes.Equal(before, after) || reopenErr != nil || reopenVal != !tc.initial || diskValue != !tc.initial {
				t.Fatalf("toggle did not persist a real boolean: runtime=%t, disk=%#v, reload=%t, err=%v", runtime, diskValue, reopenVal, reopenErr)
			}
		})
	}
}
