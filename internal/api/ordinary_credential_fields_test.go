//go:build darwin || linux

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api/handlers/management"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	fileauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

const (
	ordinaryCredentialV0       = "/v0/management/auth-files"
	ordinaryCredentialV8       = "/v8/management/credentials"
	ordinaryCredentialFile     = "synthetic-note.json"
	ordinaryCredentialID       = "synthetic-metadata-only"
	ordinaryCredentialPassword = "synthetic-field-patch-password"
	ordinaryCredentialNote     = "仅本地的备注"
	ordinaryCredentialPriority = 3
)

type ordinaryCredentialFixture struct {
	cfg     *config.Config
	cfgPath string
	path    string
	manager *coreauth.Manager
	server  *Server
}

func newOrdinaryCredentialFixture(t *testing.T, uploadRoute string) ordinaryCredentialFixture {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	const testConfig = "port: 8317\nremote-management: {secret-key: " + ordinaryCredentialPassword + "}\n"
	if err := os.WriteFile(cfgPath, []byte(testConfig), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg.AuthDir = filepath.Join(dir, "auth")
	if err := os.Mkdir(cfg.AuthDir, 0700); err != nil {
		t.Fatal(err)
	}
	f := ordinaryCredentialFixture{cfg: cfg, cfgPath: cfgPath, path: filepath.Join(cfg.AuthDir, ordinaryCredentialFile)}
	f.manager = f.newManager()
	f.server = f.newServer(f.manager)
	metadata := map[string]any{
		"type": "synthetic", "note": ordinaryCredentialNote, "priority": ordinaryCredentialPriority, "display_marker": "preserved",
	}
	if uploadRoute != "" {
		body, err := json.Marshal(metadata)
		if err != nil {
			t.Fatal(err)
		}
		response := ordinaryCredentialRequest(f.server, http.MethodPost, uploadRoute+"?name="+ordinaryCredentialFile, body, true)
		if response.Code != http.StatusOK {
			t.Fatalf("register credential via POST %s: status=%d body=%s", uploadRoute, response.Code, response.Body.String())
		}
		return f
	}
	_, err = f.manager.Register(context.Background(), &coreauth.Auth{
		ID: ordinaryCredentialID, FileName: ordinaryCredentialFile, Provider: "synthetic",
		Attributes: map[string]string{"path": f.path},
		Metadata:   metadata,
	})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f ordinaryCredentialFixture) newManager() *coreauth.Manager {
	store := fileauth.NewFileTokenStore()
	store.SetBaseDir(f.cfg.AuthDir)
	return coreauth.NewManager(store, nil, nil)
}

func (f ordinaryCredentialFixture) newServer(m *coreauth.Manager) *Server {
	h := management.NewHandler(f.cfg, f.cfgPath, m)
	h.SetLocalPassword(ordinaryCredentialPassword)
	s := &Server{cfg: f.cfg, engine: gin.New(), mgmt: h}
	s.managementRoutesEnabled.Store(true)
	s.registerManagementRoutes()
	return s
}

func ordinaryCredentialRequest(server *Server, method, route string, data []byte, authorized bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, route, bytes.NewReader(data))
	req.RemoteAddr = "127.0.0.1:1234"
	if authorized {
		req.Header.Set("Authorization", "Bearer "+ordinaryCredentialPassword)
	}
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	server.engine.ServeHTTP(recorder, req)
	return recorder
}

func TestIndependentOrdinaryCredentialFieldsPersistence(t *testing.T) {
	// The injected OS limit and signal disposition must not affect any other test.
	const childEnv = "CPA_TEST_ORDINARY_CREDENTIAL_FSIZE_HELPER"
	if os.Getenv(childEnv) != "1" {
		testBinary, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, testBinary, "-test.run=^"+t.Name()+"$", "-test.v", "-test.count=1")
		cmd.Env = append(os.Environ(), childEnv+"=1")
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("isolated credential fault replay: %v\n%s", err, output)
		}
		t.Logf("isolated credential fault replay:\n%s", output)
		return
	}
	t.Setenv("MANAGEMENT_PASSWORD", "")
	const v0, v8 = ordinaryCredentialV0, ordinaryCredentialV8
	const credential, logicalID = ordinaryCredentialFile, ordinaryCredentialID
	const label, initialPriority = ordinaryCredentialNote, ordinaryCredentialPriority
	const newPriority = 19

	for _, routes := range [][2]string{{v0, v8}, {v8, v0}} {
		first, second := routes[0], routes[1]
		t.Run(strings.Split(first, "/")[1], func(t *testing.T) {
			f := newOrdinaryCredentialFixture(t, "")
			manager, server, cfg, path := f.manager, f.server, f.cfg, f.path
			newManager, newServer, call := f.newManager, f.newServer, ordinaryCredentialRequest
			readDisk := func() []byte {
				t.Helper()
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				return raw
			}
			checkDisk := func(note string, priority int) {
				t.Helper()
				var disk map[string]any
				raw := readDisk()
				if err := json.Unmarshal(raw, &disk); err != nil || disk["note"] != note || disk["priority"] != float64(priority) || disk["display_marker"] != "preserved" {
					t.Errorf("disk length=%d noteLen=%d priority=%v marker=%v JSON error=%v", len(raw), ordinaryCredentialNoteLength(disk["note"]), disk["priority"], disk["display_marker"], err)
				}
			}
			checkList := func(server *Server, note string, priority int) {
				t.Helper()
				for _, route := range []string{v0, v8} {
					got := call(server, http.MethodGet, route, nil, true)
					var list struct {
						Files []struct {
							Name, Note string
							Priority   int
						}
					}
					if err := json.Unmarshal(got.Body.Bytes(), &list); err != nil || got.Code != 200 || len(list.Files) != 1 {
						t.Errorf("GET %s: status=%d rows=%d err=%v", route, got.Code, len(list.Files), err)
						continue
					}
					row := list.Files[0]
					if row.Name != credential || row.Note != note || row.Priority != priority {
						t.Errorf("GET %s: name=%s noteLen=%d/%d priority=%d/%d", route, row.Name, len(row.Note), len(note), row.Priority, priority)
					}
				}
			}
			makePatch := func(name, note string) []byte {
				t.Helper()
				raw, err := json.Marshal(map[string]any{"name": name, "note": note, "priority": newPriority})
				if err != nil {
					t.Fatal(err)
				}
				return raw
			}
			checkDisk(label, initialPriority)
			checkList(server, label, initialPriority)
			before := readDisk()
			longNote := strings.Repeat("synthetic note ", 140) + "end"
			body := makePatch(logicalID, longNote)
			if got := call(server, http.MethodPatch, first+"/fields", body, false); got.Code != http.StatusUnauthorized {
				t.Fatalf("unauthenticated PATCH %s got %d", first, got.Code)
			}
			failed := func() *httptest.ResponseRecorder {
				restore := limitOrdinaryFileWrite(t, uint64(len(before)+128))
				defer restore()
				return call(server, http.MethodPatch, first+"/fields", body, true)
			}()
			after := readDisk()
			t.Logf("%s blocked write status=%d oldBytes=%d currentBytes=%d validJSON=%t", first, failed.Code, len(before), len(after), json.Valid(after))
			if failed.Code != http.StatusInternalServerError {
				t.Errorf("failed management patch status=%d body=%s, want 500", failed.Code, failed.Body.String())
			}
			if !bytes.Equal(after, before) {
				t.Error("failed management patch mutated previous disk bytes")
			}
			checkList(server, label, initialPriority)
			if current, ok := manager.GetByID(logicalID); !ok || current.Metadata["note"] != label {
				t.Error("failed management patch published uncommitted metadata")
			}
			if got := call(server, http.MethodPatch, second+"/fields", makePatch(credential, longNote), true); got.Code != http.StatusOK || !strings.Contains(got.Body.String(), "\"status\":\"ok\"") {
				t.Fatalf("retry through %s: status=%d body=%s", second, got.Code, got.Body.String())
			}
			checkDisk(longNote, newPriority)
			checkList(server, longNote, newPriority)
			reopened := newManager()
			if err := reopened.Load(context.Background()); err != nil {
				t.Fatal(err)
			}
			checkList(newServer(reopened), longNote, newPriority)
			t.Logf("%s retry persisted ordinary fields and both versions read them from a fresh manager", second)
			t.Run("status-retries", func(t *testing.T) {
				checkDisabled := func(s *Server, want bool) {
					t.Helper()
					for _, route := range []string{v0, v8} {
						got := call(s, http.MethodGet, route, nil, true)
						var list struct{ Files []struct{ Disabled bool } }
						if err := json.Unmarshal(got.Body.Bytes(), &list); err != nil || got.Code != 200 || len(list.Files) != 1 || list.Files[0].Disabled != want {
							t.Errorf("GET %s disabled=%t: status=%d rows=%d err=%v", route, want, got.Code, len(list.Files), err)
						}
					}
				}
				for _, disabled := range []bool{true, false} {
					raw, err := json.Marshal(map[string]any{"name": credential, "disabled": disabled})
					if err != nil {
						t.Fatal(err)
					}
					beforeStatus := readDisk()
					func() {
						restore := limitOrdinaryFileWrite(t, uint64(len(beforeStatus)/2))
						defer restore()
						for _, route := range routes {
							got := call(server, http.MethodPatch, route+"/status", raw, true)
							if got.Code != http.StatusInternalServerError || strings.Contains(got.Body.String(), cfg.AuthDir) {
								t.Errorf("failed status change via %s returned %d body=%s", route, got.Code, got.Body.String())
							}
							if !bytes.Equal(readDisk(), beforeStatus) {
								t.Error("failed status change mutated prior disk bytes")
							}
							if current, ok := manager.GetByID(logicalID); !ok || current.Disabled == disabled {
								t.Error("failed status change published uncommitted state")
							}
							checkDisabled(server, !disabled)
						}
					}()
					if got := call(server, http.MethodPatch, second+"/status", raw, true); got.Code != http.StatusOK {
						t.Fatalf("retry status change returned %d body=%s", got.Code, got.Body.String())
					}
					var stored struct{ Disabled bool }
					if err := json.Unmarshal(readDisk(), &stored); err != nil || stored.Disabled != disabled {
						t.Errorf("disk disabled=%t, want %t, err=%v", stored.Disabled, disabled, err)
					}
					checkDisabled(server, disabled)
					fresh := newManager()
					if err := fresh.Load(context.Background()); err != nil {
						t.Fatal(err)
					}
					checkDisabled(newServer(fresh), disabled)
					t.Logf("two injected failures kept old state, retry and reopen agree disabled=%t", disabled)
				}
			})
			t.Run("ordinary-SDK-methods-stay-best-effort", func(t *testing.T) {
				ctx := context.Background()
				beforeSDK := readDisk()
				restore := limitOrdinaryFileWrite(t, uint64(len(beforeSDK)/2))
				defer restore()
				updated, committed, err := manager.Modify(ctx, logicalID, 0, func(a *coreauth.Auth) error {
					a.Metadata["sdk_marker"] = "modify"
					return nil
				})
				if err != nil || !committed || updated == nil || updated.Metadata["sdk_marker"] != "modify" {
					t.Fatalf("existing SDK Modify did not commit in memory: committed=%t err=%v", committed, err)
				}
				snapshot, ok := manager.GetByID(logicalID)
				if !ok {
					t.Fatal("auth missing after Modify")
				}
				snapshot.Metadata["sdk_marker"] = "update"
				updated, err = manager.Update(ctx, snapshot)
				if err != nil || updated == nil || updated.Metadata["sdk_marker"] != "update" {
					t.Fatalf("existing SDK Update did not commit in memory: err=%v", err)
				}
				if !bytes.Equal(readDisk(), beforeSDK) {
					t.Error("SDK storage fault changed prior disk bytes")
				}
				t.Log("ordinary SDK Modify and Update kept prior best-effort behavior after two injected storage faults")
			})
		})
	}
}

func TestIndependentOrdinaryCredentialMissingFile(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	for _, route := range []string{ordinaryCredentialV0, ordinaryCredentialV8} {
		for _, tc := range []struct {
			name              string
			initiallyDisabled bool
			suffix            string
			patch             string
		}{
			{"edit-active", false, "/fields", "\"note\":\"updated note\""},
			{"disable-active", false, "/status", "\"disabled\":true"},
			{"edit-disabled", true, "/fields", "\"note\":\"updated note\""},
			{"enable-disabled", true, "/status", "\"disabled\":false"},
		} {
			t.Run(strings.Split(route, "/")[1]+"/"+tc.name, func(t *testing.T) {
				f := newOrdinaryCredentialFixture(t, route)
				if tc.initiallyDisabled {
					body := []byte(`{"name":"` + ordinaryCredentialFile + `","disabled":true}`)
					rr := ordinaryCredentialRequest(f.server, http.MethodPatch, route+"/status", body, true)
					if rr.Code != http.StatusOK {
						t.Fatalf("set initial disabled state: status=%d body=%s", rr.Code, rr.Body.String())
					}
				}
				original, err := os.ReadFile(f.path)
				if err != nil || !json.Valid(original) {
					t.Fatalf("initial persisted file is missing or invalid: %v", err)
				}
				registered := f.manager.List()
				if len(registered) != 1 {
					t.Fatalf("registered %d credentials, want one", len(registered))
				}
				before := registered[0]
				if before.FileName != ordinaryCredentialFile || before.Disabled != tc.initiallyDisabled || before.Metadata["note"] != ordinaryCredentialNote {
					t.Fatal("wrong initial manager state")
				}
				detached := filepath.Join(filepath.Dir(f.cfg.AuthDir), "detached-credential.json")
				if err := os.Rename(f.path, detached); err != nil {
					t.Fatal(err)
				}
				body := []byte(`{"name":"` + ordinaryCredentialFile + `",` + tc.patch + `}`)
				rr := ordinaryCredentialRequest(f.server, http.MethodPatch, route+tc.suffix, body, true)
				if rr.Code != http.StatusNotFound && rr.Code != http.StatusConflict {
					t.Errorf("%s when file disappeared: status=%d body=%s, want 404 or 409", route+tc.suffix, rr.Code, rr.Body.String())
				}
				if _, err := os.Lstat(f.path); !os.IsNotExist(err) {
					t.Errorf("missing file was recreated or stat failed unexpectedly: %v", err)
				}
				if file, err := os.ReadFile(detached); err != nil || !bytes.Equal(original, file) {
					t.Errorf("detached file was changed: %v", err)
				}
				after, ok := f.manager.GetByID(before.ID)
				if !ok || after.Disabled != before.Disabled || after.Metadata["note"] != before.Metadata["note"] || after.Generation != before.Generation {
					t.Error("missing-file request published a new runtime snapshot")
				}
				if err := os.Rename(detached, f.path); err != nil {
					t.Fatal(err)
				}
				rr = ordinaryCredentialRequest(f.server, http.MethodPatch, route+tc.suffix, body, true)
				if rr.Code != http.StatusOK {
					t.Fatalf("restored file retry: status=%d body=%s", rr.Code, rr.Body.String())
				}
				var stored struct {
					Note     string
					Disabled bool
				}
				file, err := os.ReadFile(f.path)
				if err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(file, &stored); err != nil {
					t.Fatal(err)
				}
				after, ok = f.manager.GetByID(before.ID)
				if !ok || stored.Note != after.Metadata["note"] || stored.Disabled != after.Disabled {
					t.Fatal("restored file retry did not match runtime")
				}
				if tc.suffix == "/fields" && stored.Note != "updated note" || tc.suffix == "/status" && stored.Disabled == tc.initiallyDisabled {
					t.Fatal("restored file retry did not persist the edit")
				}
				t.Logf("%s rejected the vanished file and saved the edit after it returned", route+tc.suffix)
			})
		}
	}
	t.Run("SDK-keeps-file-creation-policy", func(t *testing.T) {
		f := newOrdinaryCredentialFixture(t, "")
		const route = ordinaryCredentialV0 + "/status"
		rr := ordinaryCredentialRequest(f.server, http.MethodPatch, route, []byte(`{"name":"`+ordinaryCredentialFile+`","disabled":true}`), true)
		if rr.Code != http.StatusOK {
			t.Fatalf("set initial disabled: status=%d body=%s", rr.Code, rr.Body.String())
		}
		if err := os.Remove(f.path); err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()
		updated, committed, err := f.manager.Modify(ctx, ordinaryCredentialID, 0, func(current *coreauth.Auth) error {
			current.Metadata["note"] = "sdk note"
			return nil
		})
		if err != nil || !committed || updated == nil || updated.Metadata["note"] != "sdk note" {
			t.Fatalf("ordinary SDK Modify did not update memory: committed=%t err=%v", committed, err)
		}
		if _, err := os.Lstat(f.path); !os.IsNotExist(err) {
			t.Fatalf("disabled SDK update recreated a missing file: %v", err)
		}
		updated.Disabled = false
		updated.Status = coreauth.StatusActive
		if _, err := f.manager.Update(ctx, updated); err != nil {
			t.Fatal(err)
		}
		if raw, err := os.ReadFile(f.path); err != nil || !json.Valid(raw) {
			t.Fatalf("enabled SDK update did not preserve its file creation policy: %v", err)
		}
	})
}

func ordinaryCredentialNoteLength(value any) int {
	note, _ := value.(string)
	return len(note)
}

func limitOrdinaryFileWrite(t *testing.T, maxSize uint64) func() {
	t.Helper()
	var previous syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &previous); err != nil {
		t.Fatal(err)
	}
	limited := previous
	if limited.Cur <= maxSize {
		t.Fatalf("process file-size limit %d prevents an isolated failure fixture", limited.Cur)
	}
	limited.Cur = maxSize
	signal.Ignore(syscall.SIGXFSZ)
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &limited); err != nil {
		signal.Reset(syscall.SIGXFSZ)
		t.Fatal(err)
	}
	return func() {
		if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &previous); err != nil {
			t.Fatal(err)
		}
		signal.Reset(syscall.SIGXFSZ)
	}
}
