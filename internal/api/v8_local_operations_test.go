package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api/handlers/management"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	executor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

type localV8VerifyExecutor struct {
	coreauth.ProviderExecutor
	seen []string
}

func (*localV8VerifyExecutor) Identifier() string { return "codex" }

func (e *localV8VerifyExecutor) ExecuteStream(_ context.Context, a *coreauth.Auth, r executor.Request, _ executor.Options) (*executor.StreamResult, error) {
	e.seen = append(e.seen, a.ID+":"+r.Model)
	ch := make(chan executor.StreamChunk, 1)
	ch <- executor.StreamChunk{Payload: []byte(`data: {"type":"response.completed","response":{"id":"fixture","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}}` + "\n\n")}
	close(ch)
	return &executor.StreamResult{Chunks: ch}, nil
}

func TestLocalV8Operations(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("MANAGEMENT_PASSWORD", "")
	const (
		target  = "fixture-selected.json"
		sibling = "fixture-sibling.json"
		secret  = "fixture-sibling-credential"
		admin   = "fixture-test-admin"
		model   = "fixture-verification-model"
		initial = `{"type":"codex","note":"original","access_token":"fixture-selected-token","priority":1,"legacy":9007199254740993}`
		other   = `{"type":"codex","note":"sibling","access_token":"` + secret + `"}`
	)
	type fixture struct {
		engine    *gin.Engine
		dir, logs string
		reopen    func() *coreauth.Manager
		verifier  *localV8VerifyExecutor
		live      *coreauth.Manager
	}
	setup := func(t *testing.T) fixture {
		t.Helper()
		dir, logs := t.TempDir(), t.TempDir()
		cfg := &config.Config{AuthDir: dir, LoggingToFile: true}
		cfg.RemoteManagement.SecretKey = admin
		write := func(dir, name, raw string) {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
		}
		write(dir, target, initial)
		write(dir, sibling, other)
		write(logs, "main.log", "main fixture\n")
		write(logs, "main.log.1", "rotated fixture\n")
		write(logs, "error-fixture-a.log", "selected error body\n")
		write(logs, "error-fixture-b.log", "sibling error body\n")
		write(logs, "app-2026-11-24T120000-1234abcd.log", "selected request log\n")
		write(logs, "not-a-log.txt", "unrelated fixture\n")
		reopen := func() *coreauth.Manager {
			m := coreauth.NewManager(sdkAuth.NewFileTokenStore().WithBaseDir(dir), nil, nil)
			m.SetConfig(cfg)
			if err := m.Load(t.Context()); err != nil {
				t.Fatal(err)
			}
			return m
		}
		m := reopen()
		verifier := &localV8VerifyExecutor{}
		m.RegisterExecutor(verifier)
		reg := registry.GetGlobalRegistry()
		reg.RegisterClient(target, "codex", []*registry.ModelInfo{{ID: model}})
		t.Cleanup(func() { reg.UnregisterClient(target) })
		h := management.NewHandler(cfg, filepath.Join(t.TempDir(), "absent.yaml"), m)
		h.SetLocalPassword(admin)
		h.SetLogDirectory(logs)
		e := gin.New()
		s := &Server{cfg: cfg, mgmt: h, engine: e}
		s.managementRoutesEnabled.Store(true)
		s.registerManagementRoutes()
		return fixture{e, dir, logs, reopen, verifier, m}
	}
	type probe struct {
		method, path string
		check        func(t *testing.T, f fixture, request func(path, body string, want int) *httptest.ResponseRecorder)
	}
	probes := []probe{
		{"GET", "/observability/logs/errors", func(t *testing.T, f fixture, req func(string, string, int) *httptest.ResponseRecorder) {
			body := req("/observability/logs/errors", "", 200).Body.String()
			if !strings.Contains(body, "error-fixture-a.log") || !strings.Contains(body, "error-fixture-b.log") || strings.Contains(body, "selected error body") || strings.Contains(body, "not-a-log.txt") {
				t.Fatal("wrong visible error log index")
			}
			for _, name := range []string{"error-fixture-a.log", "error-fixture-b.log"} {
				if err := os.Remove(filepath.Join(f.logs, name)); err != nil {
					t.Fatal(err)
				}
			}
			if !strings.Contains(req("/observability/logs/errors", "", 200).Body.String(), `"files":[]`) {
				t.Fatal("empty index not an array")
			}
		}},
		{"GET", "/observability/logs/errors/:name", func(t *testing.T, f fixture, req func(string, string, int) *httptest.ResponseRecorder) {
			if w := req("/observability/logs/errors/error-fixture-a.log", "", 200); w.Body.String() != "selected error body\n" || !strings.Contains(w.Header().Get("Content-Disposition"), "error-fixture-a.log") {
				t.Fatal("wrong attachment")
			}
			req("/observability/logs/errors/error-missing.log", "", 404)
			req("/observability/logs/errors/not-a-log.txt", "", 404)
		}},
		{"GET", "/observability/logs/requests/:id", func(t *testing.T, f fixture, req func(string, string, int) *httptest.ResponseRecorder) {
			if w := req("/observability/logs/requests/1234abcd", "", 200); w.Body.String() != "selected request log\n" {
				t.Fatal("wrong indexed request log")
			}
			req("/observability/logs/requests/deadbeef", "", 404)
		}},
		{"DELETE", "/observability/logs", func(t *testing.T, f fixture, req func(string, string, int) *httptest.ResponseRecorder) {
			w := req("/observability/logs", "", 200)
			var body struct {
				Success bool
				Removed int
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || !body.Success || body.Removed != 1 {
				t.Fatalf("wrong clear report: %+v, %v", body, err)
			}
			assertV8LocalDisk(t, f.logs, "main.log", "")
			if _, err := os.Stat(filepath.Join(f.logs, "main.log.1")); !os.IsNotExist(err) {
				t.Fatalf("rotated file retained: %v", err)
			}
			assertV8LocalDisk(t, f.logs, "error-fixture-a.log", "selected error body\n")
			assertV8LocalDisk(t, f.logs, "not-a-log.txt", "unrelated fixture\n")
		}},
		{"POST", "/credentials", func(t *testing.T, f fixture, req func(string, string, int) *httptest.ResponseRecorder) {
			const upload = `{"type":"codex","note":"uploaded","access_token":"new-token","legacy":9007199254740993}`
			req("/credentials?name="+target, `{"type":`, 400)
			assertV8LocalDisk(t, f.dir, target, initial)
			req("/credentials?name="+target, upload, 200)
			assertV8LocalDisk(t, f.dir, target, upload)
			got, ok := f.reopen().GetByID(target)
			if !ok || got.Metadata["note"] != "uploaded" {
				t.Fatal("cold store missed upload")
			}
		}},
		{"PATCH", "/credentials/fields", func(t *testing.T, f fixture, req func(string, string, int) *httptest.ResponseRecorder) {
			for _, patch := range []string{`"priority":{}`, `"priority":[]`, `"priority":true`, `"priority":1.25`, `"priority":9223372036854775808`, `"priority":"9223372036854775808"`, `"priority.child":2`} {
				req("/credentials/fields", `{"name":"`+target+`","note":"must not publish",`+patch+`}`, 400)
				assertV8LocalDisk(t, f.dir, target, initial)
				assertV8LocalDisk(t, f.dir, sibling, other)
				got, ok := f.live.GetByID(target)
				if !ok || got.Metadata["note"] != "original" {
					t.Fatal("invalid priority changed memory")
				}
				if got2, ok := f.live.GetByID(sibling); !ok || got2.Metadata["note"] != "sibling" {
					t.Fatal("invalid priority changed sibling memory")
				}
			}
			for _, tc := range []struct{ raw, attr string }{{`9`, "9"}, {`" -11 "`, "-11"}, {`0`, ""}, {`null`, ""}} {
				req("/credentials/fields", `{"name":"`+target+`","priority":`+tc.raw+`,"prefix":"fixture-prefix"}`, 200)
				got, ok := f.reopen().GetByID(target)
				live, liveOK := f.live.GetByID(target)
				if !ok || !liveOK || got.Prefix != "fixture-prefix" {
					t.Fatal("cold store missed prefix")
				}
				// Cold FileStore reads credential metadata; live fields projection also updates the scheduler attribute.
				raw, err := os.ReadFile(filepath.Join(f.dir, target))
				var fields map[string]json.RawMessage
				if err != nil || json.Unmarshal(raw, &fields) != nil || string(fields["legacy"]) != "9007199254740993" || string(fields["priority"]) != tc.raw || string(fields["note"]) != `"original"` || live.Attributes["priority"] != tc.attr {
					t.Fatalf("priority %s did not persist with compatible semantics, live priority=%q", tc.raw, live.Attributes["priority"])
				}
				coldRaw, _ := json.Marshal(got.Metadata["priority"])
				if string(coldRaw) != tc.raw {
					t.Fatalf("cold priority %s, want %s", coldRaw, tc.raw)
				}
			}
		}},
		{"PATCH", "/credentials/status", func(t *testing.T, f fixture, req func(string, string, int) *httptest.ResponseRecorder) {
			req("/credentials/status", `{"name":"`+target+`"}`, 400)
			assertV8LocalDisk(t, f.dir, target, initial)
			for _, step := range []struct {
				raw      string
				disabled bool
			}{{"true", true}, {"false", false}} {
				req("/credentials/status", `{"name":"`+target+`","disabled":`+step.raw+`}`, 200)
				got, ok := f.reopen().GetByID(target)
				if !ok || got.Disabled != step.disabled || got.Metadata["note"] != "original" {
					t.Fatalf("cold disable status mismatch: want %v", step.disabled)
				}
			}
		}},
		{"DELETE", "/credentials", func(t *testing.T, f fixture, req func(string, string, int) *httptest.ResponseRecorder) {
			req("/credentials?name=..%2Fselected.json", "", 400)
			assertV8LocalDisk(t, f.dir, target, initial)
			req("/credentials?name="+target, "", 200)
			req("/credentials?name="+target, "", 404)
			if _, err := os.Stat(filepath.Join(f.dir, target)); !os.IsNotExist(err) {
				t.Fatalf("deleted file remains: %v", err)
			}
			if _, ok := f.reopen().GetByID(target); ok {
				t.Fatal("cold store restored a deleted credential")
			}
		}},
		{"POST", "/credentials/verify", func(t *testing.T, f fixture, req func(string, string, int) *httptest.ResponseRecorder) {
			req("/credentials/verify", `{}`, 400)
			req("/credentials/verify", `{"name":"missing.json","model":"`+model+`"}`, 404)
			if len(f.verifier.seen) != 0 {
				t.Fatal("invalid verification dispatched")
			}
			w := req("/credentials/verify", `{"name":"`+target+`","model":"`+model+`"}`, 200)
			var body struct {
				Verified   bool
				Credential struct{ Name string }
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || !body.Verified || body.Credential.Name != target || len(f.verifier.seen) != 1 || f.verifier.seen[0] != target+":"+model {
				t.Fatal("verification did not use only the selected credential and model")
			}
		}},
	}
	for _, tc := range probes {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			f := setup(t)
			do := func(path, body, key string) *httptest.ResponseRecorder {
				r := httptest.NewRequest(tc.method, "/v8/management"+path, strings.NewReader(body))
				r.RemoteAddr = "127.0.0.1:12000"
				r.Header.Set("Content-Type", "application/json")
				if key != "" {
					r.Header.Set("Authorization", "Bearer "+key)
				}
				w := httptest.NewRecorder()
				f.engine.ServeHTTP(w, r)
				return w
			}
			probePath := strings.NewReplacer(":name", "error-fixture-a.log", ":id", "1234abcd").Replace(tc.path)
			if w := do(probePath, "", ""); w.Code != 401 || !strings.Contains(w.Body.String(), "missing management key") {
				t.Fatalf("anonymous denial=%d", w.Code)
			}
			check := func(path, body string, want int) *httptest.ResponseRecorder {
				w := do(path, body, admin)
				if w.Code != want {
					var msg struct{ Error string }
					_ = json.Unmarshal(w.Body.Bytes(), &msg)
					t.Fatalf("%s status=%d expected=%d reason=%q", path, w.Code, want, msg.Error)
				}
				if bytes.Contains(w.Body.Bytes(), []byte(secret)) || bytes.Contains(w.Body.Bytes(), []byte(admin)) || bytes.Contains(w.Body.Bytes(), []byte("fixture-selected-token")) {
					t.Fatal("response disclosed private fixture data")
				}
				return w
			}
			tc.check(t, f, check)
			assertV8LocalDisk(t, f.dir, sibling, other)
		})
	}
}

func assertV8LocalDisk(t *testing.T, dir, name, want string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil || string(data) != want {
		t.Fatalf("unexpected disk result for %s: %v", name, err)
	}
}
