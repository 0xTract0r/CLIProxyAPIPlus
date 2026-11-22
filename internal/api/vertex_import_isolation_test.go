package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"mime/multipart"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestOrdinaryVertexImportDefaultStoreIsolation(t *testing.T) {
	const key = "synthetic-vertex-admin"
	const v0 = "/v0/management/vertex/import"
	const v8 = "/v8/management/oauth/import?provider=vertex"
	t.Setenv("MANAGEMENT_PASSWORD", key)
	aDir, bDir := t.TempDir(), t.TempDir()
	build := func(dir string, m *auth.Manager, opts ...ServerOption) *Server {
		cfg := &config.Config{AuthDir: dir}
		cfg.RemoteManagement.DisableControlPanel = true
		if m != nil {
			opts = append(opts, WithPostAuthPersistHook(func(ctx context.Context, record *auth.Auth) error {
				_, err := m.Register(auth.WithSkipPersist(ctx), record)
				return err
			}))
		}
		return NewServer(cfg, m, nil, "", opts...)
	}
	encodedKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pemBody := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(encodedKey)}))
	importOne := func(s *Server, path, id string) int {
		var b bytes.Buffer
		mw := multipart.NewWriter(&b)
		f, err := mw.CreateFormFile("file", "local-fixture.json")
		if err != nil {
			panic(err)
		}
		if err := json.NewEncoder(f).Encode(map[string]any{"type": "service_account", "project_id": id, "client_email": id + "@fixture.invalid", "private_key": pemBody}); err != nil {
			panic(err)
		}
		if err := mw.Close(); err != nil {
			panic(err)
		}
		r := httptest.NewRequest("POST", path, &b)
		r.RemoteAddr = "127.0.0.1:1234"
		r.Header.Set("X-Management-Key", key)
		r.Header.Set("Content-Type", mw.FormDataContentType())
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		return w.Code
	}
	exists := func(dir, id string) bool {
		_, err := os.Stat(filepath.Join(dir, "vertex-"+id+".json"))
		return err == nil
	}
	aM, bM := auth.NewManager(nil, nil, nil), auth.NewManager(nil, nil, nil)
	entered, release := make(chan struct{}), make(chan struct{})
	reloadEntered, reloadRelease := make(chan struct{}), make(chan struct{})
	defer func() {
		for _, ch := range []chan struct{}{release, reloadRelease} {
			select {
			case <-ch:
			default:
				close(ch)
			}
		}
	}()
	a := build(aDir, aM, WithPostAuthHook(func(_ context.Context, rec *auth.Auth) error {
		switch rec.ID {
		case "vertex-a-overlap.json":
			close(entered)
			<-release
		case "vertex-before-reload.json":
			close(reloadEntered)
			<-reloadRelease
		}
		return nil
	}))
	b := build(bDir, bM)
	for _, x := range []struct {
		s             *Server
		m             *auth.Manager
		dir, path, id string
	}{
		{a, aM, aDir, v0, "a-alone"}, {b, bM, bDir, v8, "b-alone"},
	} {
		if status := importOne(x.s, x.path, x.id); status != 200 {
			t.Fatalf("single import status=%d", status)
		}
		_, has := x.m.GetByID("vertex-" + x.id + ".json")
		if !has || !exists(x.dir, x.id) {
			t.Fatalf("single import %s storage=%t registration=%t", x.id, exists(x.dir, x.id), has)
		}
		reopen := build(x.dir, nil)
		req := httptest.NewRequest("GET", "/v0/management/auth-files", nil)
		req.RemoteAddr = "127.0.0.1:1234"
		req.Header.Set("X-Management-Key", key)
		w := httptest.NewRecorder()
		reopen.Handler().ServeHTTP(w, req)
		if w.Code != 200 || !strings.Contains(w.Body.String(), "vertex-"+x.id+".json") {
			t.Fatalf("reopened on-disk list status=%d", w.Code)
		}
	}
	broken := filepath.Join(t.TempDir(), "file-instead-of-dir")
	if err := os.WriteFile(broken, nil, 0600); err != nil {
		t.Fatal(err)
	}
	brokenM := auth.NewManager(nil, nil, nil)
	failed := importOne(build(broken, brokenM), v8, "broken")
	_, registered := brokenM.GetByID("vertex-broken.json")
	if failed != 500 || registered || exists(aDir, "broken") || exists(bDir, "broken") {
		t.Fatalf("failed write status=%d registered=%t", failed, registered)
	}
	done := make(chan int, 1)
	go func() { done <- importOne(a, v0, "a-overlap") }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("A did not reach pre-save barrier")
	}
	bStatus := importOne(b, v8, "b-overlap")
	close(release)
	var aStatus int
	select {
	case aStatus = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("A did not finish")
	}
	_, aReg := aM.GetByID("vertex-a-overlap.json")
	_, bReg := bM.GetByID("vertex-a-overlap.json")
	aFile, bFile := exists(aDir, "a-overlap"), exists(bDir, "a-overlap")
	t.Logf("overlap A=%d B=%d A-file=%t B-file-for-A=%t A-registration=%t B-registration-for-A=%t", aStatus, bStatus, aFile, bFile, aReg, bReg)
	if aStatus != 200 || bStatus != 200 || !aFile || bFile || !aReg || bReg || !exists(bDir, "b-overlap") || exists(aDir, "b-overlap") {
		t.Error("concurrent standard import lost per-instance storage isolation")
	}

	go func() { done <- importOne(a, v0, "before-reload") }()
	select {
	case <-reloadEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("import did not reach the save before reload")
	}
	nextDir := t.TempDir()
	cfg := &config.Config{AuthDir: nextDir}
	cfg.RemoteManagement.DisableControlPanel = true
	a.mgmt.SetConfig(cfg)
	close(reloadRelease)
	select {
	case status := <-done:
		if status != 200 || !exists(aDir, "before-reload") || exists(nextDir, "before-reload") {
			t.Errorf("in-flight import lost its original directory: status=%d", status)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight import did not finish after reload")
	}
	if status := importOne(a, v8, "after-reload"); status != 200 || !exists(nextDir, "after-reload") || exists(aDir, "after-reload") || exists(bDir, "after-reload") || !exists(aDir, "a-alone") {
		t.Fatalf("ordinary import after config directory change: status=%d", status)
	}
}
