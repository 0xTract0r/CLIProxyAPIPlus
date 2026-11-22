package api

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestOrdinaryCredentialEditPreservesLinkedFile(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	for _, route := range []string{ordinaryCredentialV0, ordinaryCredentialV8} {
		t.Run(route, func(t *testing.T) {
			f := newOrdinaryCredentialFixture(t, route)
			alias := filepath.Join(t.TempDir(), "saved-link")
			if err := os.Link(f.path, alias); err != nil {
				t.Fatal(err)
			}
			old, err := os.ReadFile(alias)
			if err != nil {
				t.Fatal(err)
			}
			body := []byte(`{"name":"synthetic-note.json","note":"changed"}`)
			result := ordinaryCredentialRequest(f.server, http.MethodPatch, route+"/fields", body, true)
			if result.Code != http.StatusConflict {
				t.Fatalf("linked credential edit returned %d", result.Code)
			}
			for _, path := range []string{alias, f.path} {
				got, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(got, old) {
					t.Fatal("linked file changed after rejected edit")
				}
			}
			if err := os.Remove(alias); err != nil {
				t.Fatal(err)
			}
			result = ordinaryCredentialRequest(f.server, http.MethodPatch, route+"/fields", body, true)
			if result.Code != http.StatusOK {
				t.Fatalf("single-link retry returned %d", result.Code)
			}
		})
	}
}
