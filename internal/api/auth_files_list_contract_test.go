package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestOrdinaryAuthFileListScopeAndLifecycle(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	f := newOrdinaryCredentialFixture(t, ordinaryCredentialV0)
	const newName = "aa-ordinary.json"
	const sentinel = "synthetic-never-list-secret"
	uploadRoute := ordinaryCredentialV8 + "?name=" + newName
	body := []byte(`{"type":"synthetic","note":"first","priority":11,"disabled":true,"oauth_provider":"synthetic-alias",
	"proxy_url":"socks5://proxy.example.invalid:1080","headers":{"Accept":"application/json","Authorization":"` + sentinel + `"},
	"access_token":"` + sentinel + `","refresh_disabled_at":"2026-11-24T00:00:00Z"}`)
	request := func(method, route string, data []byte, code int) []byte {
		t.Helper()
		response := ordinaryCredentialRequest(f.server, method, route, data, true)
		if response.Code != code {
			t.Fatalf("%s %s status=%d want=%d; body=%s", method, route, response.Code, code, response.Body)
		}
		return response.Body.Bytes()
	}
	request(http.MethodPost, uploadRoute, body, http.StatusOK)
	path := filepath.Join(f.cfg.AuthDir, newName)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	private := func() {
		t.Helper()
		if info, err := os.Stat(path); err != nil || info.Mode().Perm()&0077 != 0 {
			t.Fatalf("credential file missing or not private: %v", err)
		}
	}
	private()
	request(http.MethodPost, uploadRoute, []byte(`{"broken"`), http.StatusBadRequest)
	if after, err := os.ReadFile(path); err != nil || !bytes.Equal(after, before) {
		t.Fatal("invalid upload changed the original data")
	}

	foreignDir := t.TempDir()
	foreignPath := filepath.Join(foreignDir, "foreign.json")
	if err := os.WriteFile(foreignPath, []byte(`{"type":"synthetic","note":"`+sentinel+`"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(foreignPath, filepath.Join(f.cfg.AuthDir, "shadow.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.manager.Register(coreauth.WithSkipPersist(context.Background()), &coreauth.Auth{
		ID: "outside-id", FileName: "foreign.json", Provider: "synthetic",
		Attributes: map[string]string{"path": foreignPath}, Metadata: map[string]any{"note": sentinel},
	}); err != nil {
		t.Fatal(err)
	}
	type page struct {
		Files []map[string]any `json:"files"`
		Total int              `json:"total"`
		More  bool             `json:"has_more"`
	}
	read := func(server *Server, route string) page {
		t.Helper()
		resp := ordinaryCredentialRequest(server, http.MethodGet, route, nil, true)
		var payload page
		if err := json.Unmarshal(resp.Body.Bytes(), &payload); err != nil || resp.Code != http.StatusOK ||
			strings.Contains(resp.Body.String(), sentinel) || strings.Contains(resp.Body.String(), "foreign.json") ||
			strings.Contains(resp.Body.String(), "shadow.json") {
			t.Fatalf("unsafe or invalid list payload, status=%d error=%v", resp.Code, err)
		}
		return payload
	}
	for _, current := range []*Server{f.server, f.newServer(nil)} {
		p1 := read(current, ordinaryCredentialV0+"?page=1&page_size=1")
		p2 := read(current, ordinaryCredentialV0+"?page=2&page_size=1")
		if p1.Total != 2 || p2.Total != 2 || !p1.More || p2.More || len(p1.Files) != 1 || len(p2.Files) != 1 ||
			p1.Files[0]["name"] != newName || p2.Files[0]["name"] != ordinaryCredentialFile {
			t.Fatalf("unstable rooted pagination: first=%+v second=%+v", p1, p2)
		}
		row := p1.Files[0]
		headers, _ := row["headers"].(map[string]any)
		if row["note"] != "first" || row["priority"] != float64(11) || row["disabled"] != true ||
			row["proxy_url"] != "socks5://proxy.example.invalid:1080" || len(headers) != 1 ||
			headers["Accept"] != "application/json" || row["refresh_disabled_at"] != "2026-11-24T00:00:00Z" {
			t.Fatalf("ordinary metadata mismatch: %+v", row)
		}
	}
	v8 := read(f.server, ordinaryCredentialV8+"?name="+newName+"&page=1")
	if len(v8.Files) != 1 || v8.Files[0]["oauth_provider"] != "synthetic-alias" {
		t.Fatalf("v8 OAuth alias metadata lost: %+v", v8)
	}
	request(http.MethodPatch, ordinaryCredentialV0+"/fields", []byte(`{"name":"`+newName+`","note":"edited","priority":8}`), http.StatusOK)
	request(http.MethodPatch, ordinaryCredentialV8+"/status", []byte(`{"name":"`+newName+`","disabled":false}`), http.StatusOK)
	private()
	reopened := f.newManager()
	if err := reopened.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	row := read(f.newServer(reopened), ordinaryCredentialV8+"?name="+newName)
	if len(row.Files) != 1 || row.Files[0]["note"] != "edited" || row.Files[0]["priority"] != float64(8) || row.Files[0]["disabled"] != false {
		t.Fatalf("cold load lost the ordinary edits")
	}
	request(http.MethodDelete, ordinaryCredentialV8+"?name="+newName, nil, http.StatusOK)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("deleted file remains on disk")
	}
	remaining := read(f.server, ordinaryCredentialV0+"?page=1")
	if remaining.Total != 1 || len(remaining.Files) != 1 || remaining.Files[0]["name"] != ordinaryCredentialFile {
		t.Fatal("list retained the deleted credential")
	}
	if intact, err := os.ReadFile(foreignPath); err != nil || !strings.Contains(string(intact), sentinel) {
		t.Fatal("foreign fixture changed")
	}
}
