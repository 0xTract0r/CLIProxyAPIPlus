package management

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type pendingUploadDeleteStore struct {
	memoryAuthStore
	gate        sync.Mutex
	pending     map[string][]byte
	writes      map[string]int
	deletes     map[string]int
	afterCommit func()
}

func (s *pendingUploadDeleteStore) PersistAuthUpload(_ context.Context, path string, data []byte, install func(func() error) error) error {
	s.gate.Lock()
	defer s.gate.Unlock()
	err := install(func() error {
		if previous := s.pending[path]; previous != nil {
			if !bytes.Equal(previous, data) {
				return coreauth.ErrAuthUploadPayloadConflict
			}
			return nil
		}
		s.pending[path] = bytes.Clone(data)
		s.writes[path]++
		if s.afterCommit != nil {
			s.afterCommit()
			s.afterCommit = nil
		}
		return nil
	})
	if err != nil {
		if s.pending[path] != nil {
			return errors.Join(coreauth.ErrAuthUploadLocalPending, err)
		}
		return err
	}
	delete(s.pending, path)
	return nil
}

func (s *pendingUploadDeleteStore) Delete(ctx context.Context, path string) error {
	return s.DeleteExistingAuthFile(ctx, path)
}

func (s *pendingUploadDeleteStore) DeleteExistingAuthFile(_ context.Context, path string) error {
	s.gate.Lock()
	defer s.gate.Unlock()
	if s.pending[path] != nil {
		return coreauth.ErrAuthUploadPayloadConflict
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	s.deletes[path]++
	return nil
}

func TestPendingUploadBlocksManagementDeletionUntilMirrorRecovers(t *testing.T) {
	for _, endpoint := range []string{"/v0/management/auth-files", "/v8/management/credentials"} {
		t.Run(endpoint, func(t *testing.T) {
			f := setupSIWCFlow(t, false)
			oldDir := filepath.Dir(f.path)
			newDir := t.TempDir()
			state, flow := f.seedPendingRecord(t)
			original := f.readFixtureBytes(t)
			originalAuth, ok := f.manager.GetByID("codex.json")
			if !ok {
				t.Fatal("missing initial fixture registration")
			}
			f.h.tokenStore = &pendingUploadDeleteStore{
				pending: map[string][]byte{},
				writes:  map[string]int{},
				deletes: map[string]int{},
				afterCommit: func() {
					f.h.SetConfig(&config.Config{AuthDir: newDir})
				},
			}
			backend := f.h.tokenStore.(*pendingUploadDeleteStore)
			router := newV8AndV0AuthRouter(f.h)
			body := map[string]any{"type": "codex", "note": "new"}
			upload := siwcRequest(t, router, http.MethodPost, endpoint+"?name=codex.json", body)
			if upload.Code != http.StatusServiceUnavailable || !bytes.Contains(upload.Body.Bytes(), []byte("upload_local_pending")) {
				t.Fatalf("expected remote commit and local pending, got %d %s", upload.Code, upload.Body.String())
			}
			f.h.SetConfig(&config.Config{AuthDir: oldDir})

			checkUnchanged := func() {
				t.Helper()
				current, ok := f.manager.GetByID("codex.json")
				if !ok || current.CredentialVersion != originalAuth.CredentialVersion ||
					!bytes.Equal(f.readFixtureBytes(t), original) || backend.deletes[f.path] != 0 ||
					flow.ctx.Err() != nil || !f.store.IsPending(state, "codex") {
					t.Fatal("pending delete modified old file, active credential, or callback")
				}
			}

			single := siwcRequest(t, router, http.MethodDelete, endpoint+"?name=codex.json", nil)
			if single.Code != http.StatusConflict || !bytes.Contains(single.Body.Bytes(), []byte("auth_upload_pending")) {
				t.Fatalf("single delete status=%d body=%s", single.Code, single.Body.String())
			}
			checkUnchanged()

			other := siwcRequest(t, router, http.MethodPost, endpoint+"?name=other.json", map[string]any{"type": "qwen"})
			if other.Code != http.StatusOK {
				t.Fatalf("seed sibling status=%d body=%s", other.Code, other.Body.String())
			}

			for _, test := range []struct {
				url     string
				input   any
				deleted int
			}{
				{endpoint, []string{"codex.json", "other.json"}, 1},
				{endpoint + "?all=true", nil, 0},
			} {
				response := siwcRequest(t, router, http.MethodDelete, test.url, test.input)
				var result struct {
					Deleted int `json:"deleted"`
					Failed  []struct {
						Name   string `json:"name"`
						Status int    `json:"status"`
						Code   string `json:"code"`
					} `json:"failed"`
				}
				if response.Code != http.StatusMultiStatus ||
					json.Unmarshal(response.Body.Bytes(), &result) != nil || result.Deleted != test.deleted ||
					len(result.Failed) != 1 || result.Failed[0].Name != "codex.json" ||
					result.Failed[0].Status != http.StatusConflict || result.Failed[0].Code != "auth_upload_pending" {
					t.Fatalf("batch delete status=%d body=%s", response.Code, response.Body.String())
				}
				checkUnchanged()
			}
			if _, ok := f.manager.GetByID("other.json"); ok {
				t.Fatal("nonpending sibling remained active")
			}

			retry := siwcRequest(t, router, http.MethodPost, endpoint+"?name=codex.json", body)
			current, ok := f.manager.GetByID("codex.json")
			if retry.Code != http.StatusOK || backend.writes[f.path] != 1 ||
				!ok || current.Metadata["note"] != "new" || !bytes.Contains(f.readFixtureBytes(t), []byte("new")) {
				t.Fatalf("retry did not converge, status=%d body=%s", retry.Code, retry.Body.String())
			}
			deleted := siwcRequest(t, router, http.MethodDelete, endpoint+"?name=codex.json", nil)
			if deleted.Code != http.StatusOK || backend.deletes[f.path] != 1 {
				t.Fatalf("normal delete after retry failed, status=%d body=%s", deleted.Code, deleted.Body.String())
			}
			if _, err := os.Lstat(f.path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("file remains after successful DELETE: %v", err)
			}
			if _, ok := f.manager.GetByID("codex.json"); ok {
				t.Fatal("credential remains active after successful DELETE")
			}
		})
	}
}
