//go:build darwin || linux

package api

import (
	"bytes"
	"net/http"
	"os"
	"strings"
	"testing"
)

func TestOrdinaryCredentialOperationsIsolateSameName(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	for _, route := range []string{ordinaryCredentialV0, ordinaryCredentialV8} {
		t.Run(strings.Split(route, "/")[1], func(t *testing.T) {
			a := newOrdinaryCredentialFixture(t, route)
			b := newOrdinaryCredentialFixture(t, route)
			before, err := os.ReadFile(b.path)
			if err != nil {
				t.Fatal(err)
			}
			otherRecords := b.manager.List()
			if len(otherRecords) != 1 {
				t.Fatalf("second fixture has %d records, want one", len(otherRecords))
			}
			otherID := otherRecords[0].ID
			assertOtherStore := func() {
				t.Helper()
				after, err := os.ReadFile(b.path)
				if err != nil || !bytes.Equal(before, after) {
					t.Fatal("second instance changed")
				}
				if _, ok := b.manager.GetByID(otherID); !ok {
					t.Fatal("second instance lost its registration")
				}
			}
			callOK := func(method, path, raw string) {
				t.Helper()
				if got := ordinaryCredentialRequest(a.server, method, path, []byte(raw), true); got.Code != http.StatusOK {
					t.Fatalf("%s %s returned %d", method, path, got.Code)
				}
				assertOtherStore()
			}
			callOK(http.MethodPost, route+"?name="+ordinaryCredentialFile, `{"type":"synthetic","note":"replaced","marker":"replacement"}`)
			callOK(http.MethodPatch, route+"/fields", `{"name":"`+ordinaryCredentialFile+`","note":"patched"}`)
			if data, err := os.ReadFile(a.path); err != nil || !bytes.Contains(data, []byte("patched")) || !bytes.Contains(data, []byte("replacement")) {
				t.Fatal("edited instance did not persist both changes")
			}
			callOK(http.MethodDelete, route+"?name="+ordinaryCredentialFile, "")
			if _, err := os.Stat(a.path); !os.IsNotExist(err) {
				t.Fatalf("deleted source still exists: %v", err)
			}
		})
	}
}
