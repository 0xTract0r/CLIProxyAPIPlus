package management

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type perFileDeleteStatusStore struct {
	memoryAuthStore
	failures map[string]error
}

func (s *perFileDeleteStatusStore) DeleteExistingAuthFile(_ context.Context, path string) error {
	if err := s.failures[filepath.Base(path)]; err != nil {
		return err
	}
	return os.Remove(path)
}

func TestManagementDeletePreservesPerFileRecoveryStatuses(t *testing.T) {
	for _, endpoint := range []string{"/v0/management/auth-files", "/v8/management/credentials"} {
		for _, deletion := range []string{"all", "named"} {
			t.Run(endpoint+"/"+deletion, func(t *testing.T) {
				dir := t.TempDir()
				backend := &perFileDeleteStatusStore{failures: map[string]error{
					"a-recovering.json":  fmt.Errorf("store recovery: %w", errors.Join(coreauth.ErrAuthUploadLocalPending, os.ErrNotExist)),
					"b-pending.json":     coreauth.ErrAuthUploadPayloadConflict,
					"d-other-error.json": errors.New("fixture store could not delete this file"),
				}}
				h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: dir}, coreauth.NewManager(nil, nil, nil))
				h.tokenStore = backend
				router := newV8AndV0AuthRouter(h)
				names := []string{"a-recovering.json", "b-pending.json", "c-delete.json", "d-other-error.json"}
				for _, name := range names {
					if err := os.WriteFile(filepath.Join(dir, name), []byte(`{"type":"qwen"}`), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				type failure struct {
					Name   string `json:"name"`
					Status int    `json:"status"`
					Code   string `json:"code"`
				}
				for _, want := range []failure{
					{"a-recovering.json", http.StatusServiceUnavailable, "upload_local_pending"},
					{"b-pending.json", http.StatusConflict, "auth_upload_pending"},
				} {
					rec := siwcRequest(t, router, http.MethodDelete, endpoint+"?name="+want.Name, nil)
					var one failure
					if rec.Code != want.Status || json.Unmarshal(rec.Body.Bytes(), &one) != nil || one.Code != want.Code {
						t.Fatalf("single %s status=%d body=%s", want.Name, rec.Code, rec.Body.String())
					}
				}
				var req any = names
				url := endpoint
				if deletion == "all" {
					req = nil
					url += "?all=true"
				}
				rec := siwcRequest(t, router, http.MethodDelete, url, req)
				var result struct {
					Deleted int       `json:"deleted"`
					Failed  []failure `json:"failed"`
				}
				want := []failure{
					{"a-recovering.json", http.StatusServiceUnavailable, "upload_local_pending"},
					{"b-pending.json", http.StatusConflict, "auth_upload_pending"},
					{"d-other-error.json", http.StatusInternalServerError, ""},
				}
				if rec.Code != http.StatusMultiStatus || json.Unmarshal(rec.Body.Bytes(), &result) != nil ||
					result.Deleted != 1 || !reflect.DeepEqual(result.Failed, want) {
					t.Fatalf("batch status=%d body=%s", rec.Code, rec.Body.String())
				}
				for _, failed := range want {
					if _, err := os.Stat(filepath.Join(dir, failed.Name)); err != nil {
						t.Fatalf("failed delete lost %s: %v", failed.Name, err)
					}
				}
				if _, err := os.Stat(filepath.Join(dir, "c-delete.json")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("successful delete left its file: %v", err)
				}
			})
		}
	}
}
