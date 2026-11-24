package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdkauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

const ordinarySettingsRoute = "/v0/management/auth-files/account-settings"

func ordinarySettingsRequest(s *Server, method, route string, body []byte, authorized bool, etag string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, route, bytes.NewReader(body))
	r.RemoteAddr = "127.0.0.1:10771"
	r.Header.Set("Content-Type", "application/json")
	if authorized {
		r.Header.Set("Authorization", "Bearer "+ordinaryCredentialPassword)
	}
	if etag != "" {
		r.Header.Set("If-Match", etag)
	}
	w := httptest.NewRecorder()
	s.engine.ServeHTTP(w, r)
	return w
}

func TestOrdinaryAccountSettingsPreservesFileAndConflict(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	f := newOrdinaryCredentialFixture(t, ordinaryCredentialV0)
	legacy := []byte(`{"type":"synthetic", "note" : "old", "disabled":false,
  "account_settings" : { "future" : [ 9007199254740993, {"property":"verbatim"} ], "refresh_enabled": false },
  "another-field": { "future-key": null }}
`)
	if err := os.WriteFile(f.path, legacy, 0600); err != nil {
		t.Fatal(err)
	}
	manager := f.newManager()
	if err := manager.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	server := f.newServer(manager)
	var hooks int
	server.mgmt.SetPostAuthPersistHook(func(context.Context, *coreauth.Auth) error {
		hooks++
		return nil
	})
	query := ordinarySettingsRoute + "?name=" + ordinaryCredentialFile
	if denied := ordinarySettingsRequest(server, http.MethodGet, query, nil, false, ""); denied.Code != 401 {
		t.Fatalf("unauthorized settings status=%d", denied.Code)
	}
	got := ordinarySettingsRequest(server, http.MethodGet, query, nil, true, "")
	etag := got.Header().Get("ETag")
	if got.Code != 200 || etag == "" || !strings.Contains(got.Body.String(), `"editable_fields":["note","disabled"]`) ||
		strings.Contains(got.Body.String(), "future") || strings.Contains(got.Body.String(), "refresh_enabled") {
		t.Fatalf("incompatible ordinary GET response code=%d body=%s", got.Code, got.Body)
	}
	patch := func(extra, expectedETag string) *httptest.ResponseRecorder {
		body := []byte(`{"name":"` + ordinaryCredentialFile + `",` + extra + `}`)
		return ordinarySettingsRequest(server, http.MethodPatch, ordinarySettingsRoute, body, true, expectedETag)
	}
	if invalid := patch(`"note":"should not publish","refresh_enabled":true`, etag); invalid.Code != 400 ||
		!strings.Contains(invalid.Body.String(), "unsupported account settings field: refresh_enabled") {
		t.Fatal("unsupported field did not reject the entire draft")
	}
	if invalid := patch(`"disabled":null`, etag); invalid.Code != 400 {
		t.Fatal("explicit nil bool accepted")
	}
	if old, err := os.ReadFile(f.path); err != nil || !bytes.Equal(old, legacy) || hooks != 0 {
		t.Fatal("invalid draft touched disk or hooks")
	}
	external := bytes.Replace(legacy, []byte(`"verbatim"`), []byte(`"external update"`), 1)
	if err := os.WriteFile(f.path, external, 0600); err != nil {
		t.Fatal(err)
	}
	if stale := patch(`"note":"would be stale"`, etag); stale.Code != 412 {
		t.Fatalf("stale revision status=%d", stale.Code)
	}
	if current, err := os.ReadFile(f.path); err != nil || !bytes.Equal(current, external) || hooks != 0 {
		t.Fatal("revision conflict changed disk")
	}
	etag = ordinarySettingsRequest(server, http.MethodGet, query, nil, true, "").Header().Get("ETag")
	ok := patch(`"note":"submitted","disabled":true`, etag)
	if ok.Code != 200 || hooks != 1 || !strings.Contains(ok.Body.String(), `"note":"submitted"`) ||
		ok.Header().Get("ETag") == etag {
		t.Fatalf("normal legacy edit status=%d hooks=%d body=%s", ok.Code, hooks, ok.Body)
	}
	want := bytes.Replace(external, []byte(`"note" : "old"`), []byte(`"note" : "submitted"`), 1)
	want = bytes.Replace(want, []byte(`"disabled":false`), []byte(`"disabled":true`), 1)
	after, err := os.ReadFile(f.path)
	if err != nil || !bytes.Equal(after, want) {
		t.Fatal("old untouched JSON bytes were not preserved")
	}
	st, err := os.Stat(f.path)
	if err != nil || st.Mode().Perm() != 0600 {
		t.Fatal("file permissions changed")
	}
	reopened := f.newManager()
	if err := reopened.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	read := ordinarySettingsRequest(f.newServer(reopened), http.MethodGet, query, nil, true, "")
	if read.Code != 200 || !strings.Contains(read.Body.String(), `"disabled":true`) ||
		!strings.Contains(read.Body.String(), `"note":"submitted"`) {
		t.Fatalf("cold settings mismatch: code=%d body=%s", read.Code, read.Body)
	}
}

func TestOrdinaryAccountSettingsDoesNotSelectWrongSameName(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	f := newOrdinaryCredentialFixture(t, ordinaryCredentialV0)
	if err := os.Mkdir(filepath.Join(f.cfg.AuthDir, "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(f.cfg.AuthDir, "nested", ordinaryCredentialFile)
	base := []byte(`{"type":"synthetic","note":"nested"}`)
	if err := os.WriteFile(nested, base, 0600); err != nil {
		t.Fatal(err)
	}
	other, err := f.manager.Register(coreauth.WithSkipPersist(context.Background()), &coreauth.Auth{
		ID: "different-id", FileName: ordinaryCredentialFile, Provider: "synthetic",
		Metadata: map[string]any{"type": "synthetic", "note": "nested"}, Attributes: map[string]string{"path": nested},
	})
	if err != nil {
		t.Fatal(err)
	}
	index := other.EnsureIndex()
	if got := ordinarySettingsRequest(f.server, http.MethodGet, ordinarySettingsRoute+"?name="+ordinaryCredentialFile, nil, true, ""); got.Code != 409 {
		t.Fatalf("ambiguous name status=%d", got.Code)
	}
	post := func(selected string) *httptest.ResponseRecorder {
		body := []byte(`{"name":"` + ordinaryCredentialFile + `","note":"only nested"` + selected + `}`)
		return ordinarySettingsRequest(f.server, http.MethodPatch, ordinarySettingsRoute, body, true, "")
	}
	if got := post(""); got.Code != 409 {
		t.Fatalf("ambiguous patch status=%d", got.Code)
	}
	if got := post(`,"auth_index":"` + index + `"`); got.Code != 200 {
		t.Fatalf("selected index status=%d body=%s", got.Code, got.Body)
	}
	if outside, err := os.ReadFile(f.path); err != nil || bytes.Contains(outside, []byte("only nested")) {
		t.Fatal("same-name sibling file was changed")
	}
	if updated, err := os.ReadFile(nested); err != nil || !bytes.Contains(updated, []byte("only nested")) {
		t.Fatal("selected credential file was not changed")
	}
	foreign := filepath.Join(t.TempDir(), "foreign.json")
	if err := os.WriteFile(foreign, []byte(`{"type":"synthetic","note":"foreign"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.manager.Register(coreauth.WithSkipPersist(context.Background()), &coreauth.Auth{
		ID: "unrelated-root", FileName: "foreign.json", Provider: "synthetic",
		Metadata: map[string]any{"type": "synthetic", "note": "foreign"}, Attributes: map[string]string{"path": foreign},
	}); err != nil {
		t.Fatal(err)
	}
	if got := ordinarySettingsRequest(f.server, http.MethodPatch, ordinarySettingsRoute, []byte(`{"name":"foreign.json","note":"changed"}`), true, ""); got.Code != 404 {
		t.Fatalf("foreign account settings patch status=%d", got.Code)
	}
}

type ordinarySettingsVersionStore struct {
	saved *coreauth.Auth
	err   error
}

func (s *ordinarySettingsVersionStore) List(context.Context) ([]*coreauth.Auth, error) {
	if s.saved == nil {
		return nil, nil
	}
	return []*coreauth.Auth{s.saved.Clone()}, nil
}
func (s *ordinarySettingsVersionStore) Save(_ context.Context, record *coreauth.Auth) (string, error) {
	if s.err != nil {
		return "", s.err
	}
	s.saved = record.Clone()
	return record.ID, nil
}
func (s *ordinarySettingsVersionStore) Delete(context.Context, string) error { return nil }

func TestOrdinaryAccountSettingsUsesExistingNonFileStore(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	f := newOrdinaryCredentialFixture(t, "")
	store := &ordinarySettingsVersionStore{saved: &coreauth.Auth{
		ID: "remote-fixture", FileName: "remote.json", Provider: "synthetic",
		Metadata: map[string]any{"type": "synthetic", "note": "from-store", "future_number": json.Number("9007199254740993")},
	}}
	manager := coreauth.NewManager(store, nil, nil)
	if err := manager.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	prior := sdkauth.GetTokenStore()
	sdkauth.RegisterTokenStore(store)
	t.Cleanup(func() { sdkauth.RegisterTokenStore(prior) })
	server := f.newServer(manager)
	write := func() *httptest.ResponseRecorder {
		return ordinarySettingsRequest(server, http.MethodPatch, ordinarySettingsRoute, []byte(`{"name":"remote.json","note":"new persisted"}`), true, "")
	}
	store.err = errors.New("synthetic storage failure")
	if got := write(); got.Code != 500 {
		t.Fatalf("failing store status=%d body=%s", got.Code, got.Body)
	}
	if store.saved.Metadata["note"] != "from-store" {
		t.Fatal("failing store published a draft")
	}
	store.err = nil
	if got := write(); got.Code != 200 || store.saved.Metadata["note"] != "new persisted" ||
		store.saved.Metadata["future_number"] != json.Number("9007199254740993") {
		t.Fatalf("existing store lost non-edited values or type status=%d", got.Code)
	}
}

func TestOrdinaryAccountSettingsWriteFailureKeepsBytesAndHooks(t *testing.T) {
	const childEnv = "CPA_TEST_ORDINARY_ACCOUNT_SETTINGS_FSIZE_CHILD"
	if os.Getenv(childEnv) != "1" {
		testBinary, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, testBinary, "-test.run=^"+t.Name()+"$", "-test.v", "-test.count=1")
		cmd.Env = append(os.Environ(), childEnv+"=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("isolated file failure: %v\n%s", err, out)
		}
		return
	}
	t.Setenv("MANAGEMENT_PASSWORD", "")
	f := newOrdinaryCredentialFixture(t, ordinaryCredentialV0)
	before, err := os.ReadFile(f.path)
	if err != nil {
		t.Fatal(err)
	}
	var calls int
	f.server.mgmt.SetPostAuthPersistHook(func(context.Context, *coreauth.Auth) error {
		calls++
		return nil
	})
	note := strings.Repeat("synthetic replacement ", 100)
	body, err := json.Marshal(map[string]any{"name": ordinaryCredentialFile, "note": note, "disabled": true})
	if err != nil {
		t.Fatal(err)
	}
	call := func() *httptest.ResponseRecorder {
		return ordinarySettingsRequest(f.server, http.MethodPatch, ordinarySettingsRoute, body, true, "")
	}
	failed := func() *httptest.ResponseRecorder {
		restore := limitOrdinaryFileWrite(t, uint64(len(before)+128))
		defer restore()
		return call()
	}()
	raw, err := os.ReadFile(f.path)
	visible := ordinarySettingsRequest(f.server, http.MethodGet, ordinarySettingsRoute+"?name="+ordinaryCredentialFile, nil, true, "")
	if failed.Code != 500 || err != nil || !bytes.Equal(raw, before) || calls != 0 ||
		!strings.Contains(visible.Body.String(), ordinaryCredentialNote) || !strings.Contains(visible.Body.String(), `"disabled":false`) {
		t.Fatalf("failed account settings save affected disk, live settings, or hooks: status=%d calls=%d", failed.Code, calls)
	}
	if persisted := call(); persisted.Code != 200 || calls != 1 {
		t.Fatalf("same edit could not be retried: status=%d hooks=%d", persisted.Code, calls)
	}
}
