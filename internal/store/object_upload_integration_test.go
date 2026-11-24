package store

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/minio/minio-go/v7"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestObjectPreparedAuthUploadSeaweedFS(t *testing.T) {
	if os.Getenv("CLIPROXY_TEST_SEAWEEDFS") != "1" {
		t.Skip("requires the isolated SeaweedFS integration job")
	}

	ctx := t.Context()
	const bucket = "cpa-auth-fixture"
	const name = "ordinary.json"
	open := func() *ObjectTokenStore {
		t.Helper()
		s, err := NewObjectTokenStore(ObjectStoreConfig{
			Endpoint: "127.0.0.1:48333", Bucket: bucket, Region: "us-east-1",
			AccessKey: "fixture-access", SecretKey: "fixture-secret-only", PathStyle: true,
			LocalRoot: t.TempDir(), OptionalConfig: true,
		})
		if err != nil {
			t.Fatal("cannot create isolated object store")
		}
		if err := s.Bootstrap(ctx, ""); err != nil {
			t.Fatalf("object store bootstrap: %v", err)
		}
		return s
	}
	s := open()
	path := filepath.Join(s.AuthDir(), name)
	payload := []byte("{ \"type\": \"ordinary\", \"note\": \"fixture\", \"value\": 9007199254740993 }\n")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	install := func(commit func() error) error {
		if err := commit(); err != nil {
			return err
		}
		return os.WriteFile(path, payload, 0600)
	}
	if err := s.PersistAuthUpload(ctx, path, payload, install); !errors.Is(err, coreauth.ErrAuthUploadLocalPending) {
		t.Fatalf("object upload did not report pending after the local write failure: %v", err)
	}
	verifyObject := func() {
		t.Helper()
		obj, err := s.client.GetObject(ctx, bucket, "auths/"+name, minio.GetObjectOptions{})
		if err != nil {
			t.Fatal("cannot open uploaded object")
		}
		defer obj.Close()
		got, err := io.ReadAll(io.LimitReader(obj, int64(len(payload)+1)))
		if err != nil || !bytes.Equal(got, payload) {
			t.Fatal("remote object differs from prepared bytes")
		}
	}
	verifyObject()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	prior := []byte(`{"type":"ordinary","note":"prior-mirror"}`)
	if err := os.WriteFile(path, prior, 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteExistingAuthFile(ctx, path); !errors.Is(err, coreauth.ErrAuthUploadPayloadConflict) {
		t.Fatalf("pending delete was not rejected: %v", err)
	}
	if err := s.PersistAuthUpload(ctx, path, []byte(`{"type":"ordinary","note":"different"}`), func(func() error) error {
		t.Fatal("different payload reached the installer")
		return nil
	}); !errors.Is(err, coreauth.ErrAuthUploadPayloadConflict) {
		t.Fatalf("conflicting upload was not rejected: %v", err)
	}
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, prior) {
		t.Fatal("pending delete or upload changed the prior mirror")
	}
	verifyObject()
	if err := s.PersistAuthUpload(ctx, path, payload, install); err != nil {
		t.Fatalf("same-payload local recovery: %v", err)
	}
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, payload) {
		t.Fatal("recovered mirror differs from the object")
	}
	verifyObject()
	cold := open()
	if got, err := os.ReadFile(filepath.Join(cold.AuthDir(), name)); err != nil || !bytes.Equal(got, payload) {
		t.Fatal("fresh bootstrap did not reproduce exact bytes")
	}
	if got, err := cold.List(ctx); err != nil || len(got) != 1 || got[0].ID != name {
		t.Fatal("fresh store did not list the single confirmed credential")
	}
}
