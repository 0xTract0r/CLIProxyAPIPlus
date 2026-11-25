package cliproxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"mime/multipart"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"gopkg.in/yaml.v3"
)

func TestIndependentVertexRealWatcherDirectorySwitch(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "synthetic-lifecycle-admin")
	previous := sdkAuth.GetTokenStore()
	var root sdkAuth.FileTokenStore
	sdkAuth.RegisterTokenStore(&root)
	t.Cleanup(func() { sdkAuth.RegisterTokenStore(previous) })
	dirA, dirB, dirC := t.TempDir(), t.TempDir(), t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	build := func(dir string, start bool) *Service {
		t.Helper()
		cfg := &config.Config{AuthDir: dir}
		cfg.RemoteManagement.DisableControlPanel = true
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(fmt.Sprintf("auth-dir: %q\n", dir)), 0600); err != nil {
			t.Fatal(err)
		}
		s, err := NewBuilder().WithConfig(cfg).WithConfigPath(path).WithCoreAuthManager(nil).Build()
		if err != nil {
			t.Fatal(err)
		}
		s.server = api.NewServer(cfg, s.coreManager, s.accessManager, path, s.serverOptions...)
		if start {
			w, err := defaultWatcherFactory(path, dir, func(next *config.Config) { s.applyWatcherConfigUpdate(next) })
			if err != nil {
				t.Fatal(err)
			}
			s.setWatcher(w)
			s.ensureAuthUpdateQueue(ctx)
			w.SetAuthUpdateQueue(s.authUpdates)
			w.SetConfig(cfg)
			if err := w.Start(ctx); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = w.Stop() })
		}
		return s
	}
	generated, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	key := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(generated)}))
	importOne := func(s *Service, id string) {
		t.Helper()
		var b bytes.Buffer
		mw := multipart.NewWriter(&b)
		f, err := mw.CreateFormFile("file", "synthetic.json")
		if err != nil {
			t.Fatal(err)
		}
		if err := json.NewEncoder(f).Encode(map[string]string{"type": "service_account", "project_id": id, "client_email": id + "@fixture.invalid", "private_key": key}); err != nil {
			t.Fatal(err)
		}
		if err := mw.Close(); err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest("POST", "/v0/management/vertex/import", &b)
		req.RemoteAddr = "127.0.0.1:1234"
		req.Header.Set("X-Management-Key", "synthetic-lifecycle-admin")
		req.Header.Set("Content-Type", mw.FormDataContentType())
		out := httptest.NewRecorder()
		s.server.Handler().ServeHTTP(out, req)
		if out.Code != 200 {
			t.Fatalf("import %s status=%d", id, out.Code)
		}
	}
	members := func(s *Service, id string) bool { _, ok := s.coreManager.GetByID("vertex-" + id + ".json"); return ok }
	checkFile := func(dir, id string, want bool) {
		t.Helper()
		path := filepath.Join(dir, "vertex-"+id+".json")
		raw, err := os.ReadFile(path)
		if want && (err != nil || !bytes.Contains(raw, []byte(id))) {
			t.Errorf("%s expected in selected directory: %v", id, err)
		}
		if !want && !os.IsNotExist(err) {
			t.Errorf("%s appeared in other directory: %v", id, err)
		}
	}
	a, b := build(dirA, true), build(dirB, true)
	importOne(a, "fixture-a")
	importOne(b, "fixture-b")
	if !members(a, "fixture-a") || !members(b, "fixture-b") || members(a, "fixture-b") || members(b, "fixture-a") {
		t.Error("immediate Service callback did not partition records")
	}
	for _, x := range []struct{ dir, own, other string }{{dirA, "fixture-a", "fixture-b"}, {dirB, "fixture-b", "fixture-a"}} {
		checkFile(x.dir, x.own, true)
		checkFile(x.dir, x.other, false)
		cold := build(x.dir, true)
		until := time.Now().Add(3 * time.Second)
		for !members(cold, x.own) && time.Now().Before(until) {
			time.Sleep(10 * time.Millisecond)
		}
		if !members(cold, x.own) || members(cold, x.other) {
			t.Errorf("cold watcher did not partition %s", x.own)
		}
	}
	wait := func(match func() bool) bool {
		deadline := time.Now().Add(2500 * time.Millisecond)
		for !match() && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		return match()
	}
	writeOrdinary := func(dir, name, marker string) {
		t.Helper()
		data := []byte(fmt.Sprintf(`{"type":"local-fixture","note":%q}`, marker))
		if err := os.WriteFile(filepath.Join(dir, name+".json"), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeOrdinary(dirA, "same", "old")
	writeOrdinary(dirC, "same", "new")
	writeOrdinary(dirC, "preloaded", "new")
	if !wait(func() bool { _, ok := a.coreManager.GetByID("same.json"); return ok }) {
		t.Fatal("control: old watcher did not see local file")
	}
	reload := func(dir string) {
		t.Helper()
		body := []byte(fmt.Sprintf("auth-dir: %q\n", dir))
		if err := os.WriteFile(a.configPath, body, 0600); err != nil {
			t.Fatal(err)
		}
		if !wait(func() bool { return a.currentConfig().AuthDir == dir }) {
			t.Fatal("real config watcher did not publish directory change")
		}
	}
	transition := t.TempDir()
	reload(transition)
	reload(dirC)
	pre := wait(func() bool { _, ok := a.coreManager.GetByID("preloaded.json"); return ok })
	writeOrdinary(dirC, "after-subscription", "new")
	newEvents := wait(func() bool { _, ok := a.coreManager.GetByID("after-subscription.json"); return ok })
	writeOrdinary(dirA, "late-old", "old")
	time.Sleep(400 * time.Millisecond)
	_, oldEvents := a.coreManager.GetByID("late-old.json")
	same, _ := a.coreManager.GetByID("same.json")
	note := "<missing>"
	if same != nil {
		note = fmt.Sprint(same.Metadata["note"])
	}
	t.Logf("actual A→intermediate→C reload: C preseed=%t C fsnotify=%t A late event=%t same basename note=%q", pre, newEvents, oldEvents, note)
	if !pre || !newEvents || oldEvents || note != "new" {
		t.Fatal("watcher did not follow accepted directory")
	}

	importOne(a, "fixture-c")
	checkFile(dirC, "fixture-c", true)
	checkFile(dirC, "fixture-a", false)
	checkFile(dirB, "fixture-c", false)
	checkFile(dirA, "fixture-c", false)
	checkFile(dirA, "fixture-a", true)
	if !members(a, "fixture-c") || members(b, "fixture-c") {
		t.Error("post-switch registration leaked or disappeared")
	}
	_, oldRuntime := a.coreManager.GetByID("vertex-fixture-a.json")
	if oldRuntime {
		t.Fatal("old runtime credential remained active")
	}
	if err := a.coreManager.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if members(a, "fixture-a") || !members(a, "fixture-c") {
		t.Fatal("Manager.Load restored the obsolete directory")
	}
	oldPath := filepath.Join(dirA, "late-old.json")
	oldBytes, err := os.ReadFile(oldPath)
	if err != nil {
		t.Fatal(err)
	}
	late := &coreauth.Auth{ID: "late-old.json", Provider: "local-fixture", Attributes: map[string]string{coreauth.AttributePath: oldPath}, Metadata: map[string]any{"note": "old-late"}}
	late.BindFileSnapshot(oldPath, oldBytes)
	if err := a.runtimeAuthSyncHook()(ctx, late); err != nil {
		t.Fatal(err)
	}
	_, lateVisible := a.coreManager.GetByID("late-old.json")
	if lateVisible {
		t.Fatal("old post-persist callback registered in C")
	}
	unsafeDir := t.TempDir()
	unsafe := fmt.Sprintf("auth-dir: %q\nport: 8317\nserver: {server-id: 11111111-1111-4111-8111-111111111111}\nrouting: {session-affinity: true, session-affinity-rebind-on: quota}\n", unsafeDir)
	if err := os.WriteFile(a.configPath, []byte(unsafe), 0600); err != nil {
		t.Fatal(err)
	}
	a.reloadConfigFromWatcher()
	if a.currentConfig().AuthDir != dirC {
		t.Fatal("unsafe routing edit advanced the accepted directory")
	}
	writeOrdinary(dirC, "after-unsafe-routing", "still-c")
	if !wait(func() bool { _, ok := a.coreManager.GetByID("after-unsafe-routing.json"); return ok }) {
		t.Fatal("unsafe routing edit displaced the previous directory subscription")
	}
	t.Log("unsafe route and new directory rejected together; live previous subscription retained")
	for _, knownID := range []bool{true, false} {
		bad := filepath.Join(t.TempDir(), "not-dir")
		if err := os.WriteFile(bad, nil, 0600); err != nil {
			t.Fatal(err)
		}
		raw := fmt.Sprintf("auth-dir: %q\nport: 8317\n", bad)
		if knownID {
			raw += "server: {server-id: 11111111-1111-4111-8111-111111111111}\n"
		}
		badConfig := []byte(raw)
		if err := os.WriteFile(a.configPath, badConfig, 0600); err != nil {
			t.Fatal(err)
		}
		time.Sleep(600 * time.Millisecond)
		if a.currentConfig().AuthDir != dirC {
			t.Fatal("invalid file edit replaced the accepted directory")
		}
		name := fmt.Sprintf("after-invalid-%t", knownID)
		writeOrdinary(dirC, name, "still-c")
		if !wait(func() bool { _, ok := a.coreManager.GetByID(name + ".json"); return ok }) {
			t.Fatal("rejected file edit lost accepted-directory watcher")
		}
		disk, err := os.ReadFile(a.configPath)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Equal(disk, badConfig) {
			t.Logf("rejected file preserved byte-for-byte (preexisting server-id=%t)", knownID)
			continue
		}
		if knownID {
			t.Fatal("rejected edit rewrote a configuration with a valid server-id")
		}
		var original, updated map[string]any
		if yaml.Unmarshal(badConfig, &original) != nil || yaml.Unmarshal(disk, &updated) != nil {
			t.Fatal("failed to compare rejected configuration")
		}
		rawServer, ok := updated["server"].(map[string]any)
		if !ok || len(rawServer) != 1 {
			t.Fatal("rejected config changed more than the optional server-id metadata")
		}
		serverID, _ := rawServer["server-id"].(string)
		if parsed, err := uuid.Parse(serverID); err != nil || parsed == uuid.Nil {
			t.Fatal("expected a generated server-id")
		}
		delete(updated, "server")
		if !reflect.DeepEqual(original, updated) {
			t.Fatal("rejected edit rewrote user configuration parameters")
		}
		t.Log("legacy file changed only to persist a generated server-id; rejection left all user parameters intact")
	}
}

type fixtureInjectedStore struct{ saved string }

func (s *fixtureInjectedStore) Save(_ context.Context, a *coreauth.Auth) (string, error) {
	s.saved = a.ID
	return a.ID, nil
}
func (*fixtureInjectedStore) List(context.Context) ([]*coreauth.Auth, error) { return nil, nil }
func (*fixtureInjectedStore) Delete(context.Context, string) error           { return nil }
func TestIndependentVertexServiceInjectedStore(t *testing.T) {
	custom := &fixtureInjectedStore{}
	manager := coreauth.NewManager(custom, nil, nil)
	dir := t.TempDir()
	s, err := NewBuilder().WithConfig(&config.Config{AuthDir: dir}).WithConfigPath(filepath.Join(dir, "config.yaml")).WithCoreAuthManager(manager).Build()
	if err != nil || s.coreManager != manager {
		t.Fatalf("injected manager was replaced: %v", err)
	}
	auth := &coreauth.Auth{ID: "synthetic.json", Provider: "local-fixture", Metadata: map[string]any{"type": "local-fixture"}}
	if _, err := s.coreManager.Register(context.Background(), auth); err != nil || custom.saved != auth.ID {
		t.Fatalf("custom persistence did not run: %v", err)
	}
}
