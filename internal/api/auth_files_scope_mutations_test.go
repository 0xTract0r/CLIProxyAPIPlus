package api

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestOrdinaryCredentialRoutesKeepForeignFilesOutsideMutation(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	f := newOrdinaryCredentialFixture(t, ordinaryCredentialV0)
	original, err := os.ReadFile(f.path)
	if err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(t.TempDir(), "synthetic-outside.json")
	outside := []byte(`{"type":"synthetic","note":"private-file-fixture"}`)
	if err := os.WriteFile(foreign, outside, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(foreign, filepath.Join(f.cfg.AuthDir, "synthetic-link.json")); err != nil {
		t.Fatal(err)
	}
	_, err = f.manager.Register(coreauth.WithSkipPersist(context.Background()), &coreauth.Auth{
		ID: "stale-outside-id", FileName: "stale-outside.json", Provider: "synthetic",
		Attributes: map[string]string{"path": foreign}, Metadata: map[string]any{"type": "synthetic", "note": "old"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, route := range []string{ordinaryCredentialV0, ordinaryCredentialV8} {
		t.Run(route, func(t *testing.T) {
			get := ordinaryCredentialRequest(f.server, http.MethodGet, route+"/download?name="+ordinaryCredentialFile, nil, true)
			if get.Code != http.StatusOK || !bytes.Equal(get.Body.Bytes(), original) {
				t.Fatal("regular local download did not preserve file bytes")
			}
			link := ordinaryCredentialRequest(f.server, http.MethodGet, route+"/download?name=synthetic-link.json", nil, true)
			if link.Code != http.StatusNotFound || bytes.Contains(link.Body.Bytes(), outside) {
				t.Fatal("linked file was visible to authenticated download")
			}
			for suffix, body := range map[string]string{
				"/fields": `{"name":"stale-outside-id","note":"new"}`,
				"/status": `{"name":"stale-outside-id","disabled":true}`,
			} {
				mutated := ordinaryCredentialRequest(f.server, http.MethodPatch, route+suffix, []byte(body), true)
				if mutated.Code != http.StatusNotFound {
					t.Fatalf("%s foreign stale record status=%d", suffix, mutated.Code)
				}
			}
			batch := ordinaryCredentialRequest(f.server, http.MethodDelete, route,
				[]byte(`{"names":["stale-outside-id","nonexistent.json"]}`), true)
			if batch.Code != http.StatusMultiStatus || !strings.Contains(batch.Body.String(), `"deleted":0`) {
				t.Fatalf("batch deletion escaped active directory: status=%d", batch.Code)
			}
			if active, ok := f.manager.GetByID("stale-outside-id"); !ok || active.Disabled || active.Metadata["note"] != "old" {
				t.Fatal("failed edit/delete changed the in-memory record")
			}
			if got, err := os.ReadFile(foreign); err != nil || !bytes.Equal(got, outside) {
				t.Fatal("failed edit/delete changed the original outside file")
			}
		})
	}
	saved := filepath.Join(t.TempDir(), "saved.json")
	if err := os.Rename(f.path, saved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(f.path, 0700); err != nil {
		t.Fatal(err)
	}
	failed := ordinaryCredentialRequest(f.server, http.MethodDelete, ordinaryCredentialV0+"?name="+ordinaryCredentialFile, nil, true)
	if failed.Code != http.StatusConflict {
		t.Fatalf("replacement directory delete status=%d expected 409", failed.Code)
	}
	if old, err := os.ReadFile(saved); err != nil || !bytes.Equal(old, original) {
		t.Fatal("failed local deletion affected prior file")
	}
	if err := os.Remove(f.path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(saved, f.path); err != nil {
		t.Fatal(err)
	}
	ok := ordinaryCredentialRequest(f.server, http.MethodDelete, ordinaryCredentialV8+"?name="+ordinaryCredentialFile, nil, true)
	if ok.Code != http.StatusOK {
		t.Fatalf("regular local delete retry status=%d", ok.Code)
	}
}
