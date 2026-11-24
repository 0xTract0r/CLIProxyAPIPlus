package store

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codex"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type preparedAuthUploadStore interface {
	PersistAuthUpload(context.Context, string, []byte, func(func() error) error) error
	Save(context.Context, *coreauth.Auth) (string, error)
	Delete(context.Context, string) error
	DeleteExistingAuthFile(context.Context, string) error
	PersistAuthFiles(context.Context, string, ...string) error
}

func authUploadTestStores(dir string, persist func([]byte) error) map[string]struct {
	backend preparedAuthUploadStore
	mu      *sync.Mutex
} {
	pg := &PostgresStore{authDir: dir, persistAuthFn: func(_ context.Context, _ string, data []byte) error {
		return persist(data)
	}}
	obj := &ObjectTokenStore{authDir: dir, putObjectFn: func(_ context.Context, _ string, data []byte, _ string) error {
		return persist(data)
	}}
	return map[string]struct {
		backend preparedAuthUploadStore
		mu      *sync.Mutex
	}{"postgres": {pg, &pg.mu}, "object": {obj, &obj.mu}}
}

func TestScopedAuthUploadStoresMirrorValidatedBytes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ordinary.json")
	payload := []byte(`{ "type":"ordinary", "value":9007199254740993 }`)
	refused := errors.New("fixture refused storage")
	writeErr := refused
	calls := 0
	for name, test := range authUploadTestStores(dir, func(data []byte) error {
		calls++
		if !bytes.Equal(data, payload) {
			return errors.New("unexpected upload bytes")
		}
		return writeErr
	}) {
		t.Run(name, func(t *testing.T) {
			before := calls
			if err := test.backend.PersistAuthUpload(t.Context(), path, payload, func(commit func() error) error {
				return commit()
			}); !errors.Is(err, coreauth.ErrAuthUploadRemoteUnconfirmed) || !errors.Is(err, refused) {
				t.Fatalf("unconfirmed result = %v", err)
			}
			writeErr = nil
			errLocal := errors.New("local installation failed")
			if err := test.backend.PersistAuthUpload(t.Context(), path, payload, func(commit func() error) error {
				if err := commit(); err != nil {
					return err
				}
				return errLocal
			}); !errors.Is(err, coreauth.ErrAuthUploadLocalPending) || !errors.Is(err, errLocal) {
				t.Fatalf("pending result = %v", err)
			}
			if _, err := test.backend.Save(t.Context(), &coreauth.Auth{ID: "ordinary.json", Metadata: map[string]any{"type": "ordinary"}}); !errors.Is(err, coreauth.ErrAuthUploadPayloadConflict) {
				t.Fatalf("competing Save = %v", err)
			}
			if err := test.backend.Delete(t.Context(), path); !errors.Is(err, coreauth.ErrAuthUploadPayloadConflict) {
				t.Fatalf("competing Delete = %v", err)
			}
			oldMirror := []byte(`{"type":"ordinary","note":"previous"}`)
			if err := os.WriteFile(path, oldMirror, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := test.backend.DeleteExistingAuthFile(t.Context(), path); !errors.Is(err, coreauth.ErrAuthUploadPayloadConflict) {
				t.Fatalf("competing management DELETE = %v", err)
			}
			if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, oldMirror) {
				t.Fatalf("pending management DELETE changed old mirror: %v", err)
			}
			if err := test.backend.PersistAuthFiles(t.Context(), "watcher", path); !errors.Is(err, coreauth.ErrAuthUploadPayloadConflict) {
				t.Fatalf("competing mirror = %v", err)
			}
			if err := test.backend.PersistAuthUpload(t.Context(), path, []byte("{}"), func(commit func() error) error {
				t.Fatal("different bytes reached the installer")
				return commit()
			}); !errors.Is(err, coreauth.ErrAuthUploadPayloadConflict) {
				t.Fatalf("different upload = %v", err)
			}
			if err := test.backend.PersistAuthUpload(t.Context(), path, payload, func(commit func() error) error {
				if err := commit(); err != nil {
					return err
				}
				return os.WriteFile(path, payload, 0o600)
			}); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(got, payload) || calls != before+2 {
				t.Fatalf("did not converge with exactly one confirmed write: calls=%d error=%v", calls-before, err)
			}
			writeErr = refused
		})
	}
}

func TestScopedAuthUploadAndSaveUseStoreThenCredentialLock(t *testing.T) {
	for _, backendName := range []string{"postgres", "object"} {
		t.Run(backendName, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "ordinary.json")
			payload := []byte(`{"type":"codex","note":"uploaded"}`)
			if err := os.WriteFile(path, []byte(`{"type":"codex","note":"old"}`), 0o600); err != nil {
				t.Fatal(err)
			}
			test := authUploadTestStores(dir, func([]byte) error { return nil })[backendName]
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			locked := make(chan struct{})
			release := make(chan struct{})
			lockResult := make(chan error, 1)
			var releaseOnce sync.Once
			unlock := func() { releaseOnce.Do(func() { close(release) }) }
			defer unlock()
			go func() {
				lockResult <- codex.WithFileLock(path, func() error { close(locked); <-release; return nil })
			}()
			select {
			case <-locked:
			case err := <-lockResult:
				t.Fatal(err)
			case <-time.After(3 * time.Second):
				t.Fatal("initial local lock timed out")
			}

			saveResult := make(chan error, 1)
			go func() {
				_, err := test.backend.Save(t.Context(), &coreauth.Auth{
					ID: "ordinary.json", Provider: "codex", Metadata: map[string]any{"type": "codex", "note": "saved"},
				})
				saveResult <- err
			}()
			deadline := time.Now().Add(3 * time.Second)
			for test.mu.TryLock() {
				test.mu.Unlock()
				if time.Now().After(deadline) {
					t.Fatal("Save did not take the store lock before waiting for the credential")
				}
				time.Sleep(time.Millisecond)
			}
			uploadResult := make(chan error, 1)
			go func() {
				uploadResult <- test.backend.PersistAuthUpload(t.Context(), path, payload, func(commit func() error) error {
					return codex.WithFileLockInRoot(root, "ordinary.json", func() error {
						if err := commit(); err != nil {
							return err
						}
						return root.WriteFile("ordinary.json", payload, 0o600)
					})
				})
			}()
			unlock()
			for _, result := range []<-chan error{lockResult, saveResult, uploadResult} {
				select {
				case err := <-result:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("Save and scoped upload did not complete")
				}
			}
			got, err := root.ReadFile("ordinary.json")
			if err != nil || !bytes.Equal(got, payload) {
				t.Fatalf("serialized upload did not publish last: %v", err)
			}
		})
	}
}
