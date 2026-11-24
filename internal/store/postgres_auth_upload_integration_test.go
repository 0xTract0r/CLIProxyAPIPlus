package store

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestPostgresPreparedAuthUploadRealDatabase(t *testing.T) {
	db := startIntegrationPostgres(t)
	ctx := t.Context()
	var socketDir, addresses string
	if err := db.QueryRowContext(ctx, "SELECT current_setting('unix_socket_directories'), current_setting('listen_addresses')").Scan(&socketDir, &addresses); err != nil || socketDir == "" || addresses != "" {
		t.Fatal("PostgreSQL is not using only a private Unix socket")
	}
	cfg := PostgresStoreConfig{DSN: "host=" + socketDir + " user=refresh_test dbname=postgres sslmode=disable", SpoolDir: t.TempDir(), OptionalConfig: true}
	open := func(cfg PostgresStoreConfig) *PostgresStore {
		t.Helper()
		s, err := NewPostgresStore(ctx, cfg)
		if err != nil {
			t.Fatal("cannot open isolated Postgres store")
		}
		t.Cleanup(func() { _ = s.Close() })
		if err := s.Bootstrap(ctx, ""); err != nil {
			t.Fatal("cannot bootstrap isolated Postgres store")
		}
		return s
	}
	s := open(cfg)
	const name = "ordinary.json"
	target := filepath.Join(s.AuthDir(), name)
	payload := []byte("{ \"type\": \"ordinary\", \"note\": \"fixture\", \"value\": 9007199254740993 }\n")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	install := func(commit func() error) error {
		if err := commit(); err != nil {
			return err
		}
		return os.WriteFile(target, payload, 0600)
	}
	if err := s.PersistAuthUpload(ctx, target, payload, install); !errors.Is(err, coreauth.ErrAuthUploadLocalPending) {
		t.Fatal("database commit followed by a real local write error should be pending")
	}
	readDatabase := func() (string, []byte) {
		t.Helper()
		var version, number string
		var content []byte
		var equal bool
		err := db.QueryRowContext(ctx, "SELECT xmin::text, content::text, content->>'value', content = $2::jsonb FROM auth_store WHERE id=$1", name, string(payload)).Scan(&version, &content, &number, &equal)
		if err != nil || !equal || number != "9007199254740993" {
			t.Fatal("confirmed JSONB did not preserve exact JSON values")
		}
		return version, content
	}
	version, canonical := readDatabase()
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	previous := []byte(`{"type":"ordinary","note":"previous mirror"}`)
	if err := os.WriteFile(target, previous, 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteExistingAuthFile(ctx, target); !errors.Is(err, coreauth.ErrAuthUploadPayloadConflict) {
		t.Fatal("pending delete did not reject")
	}
	if got, err := os.ReadFile(target); err != nil || !bytes.Equal(got, previous) {
		t.Fatal("pending delete changed the prior mirror")
	}
	called := false
	err := s.PersistAuthUpload(ctx, target, []byte(`{"type":"ordinary","note":"different"}`), func(func() error) error {
		called = true
		return nil
	})
	if !errors.Is(err, coreauth.ErrAuthUploadPayloadConflict) || called {
		t.Fatal("different payload reached the installer")
	}
	if err := s.PersistAuthUpload(ctx, target, payload, install); err != nil {
		t.Fatal("same-payload local completion did not recover")
	}
	if got, err := os.ReadFile(target); err != nil || !bytes.Equal(got, payload) {
		t.Fatal("local mirror did not retain exact uploaded bytes")
	}
	if got, _ := readDatabase(); got != version {
		t.Fatal("local recovery rewrote the confirmed database row")
	}
	cfg.SpoolDir = t.TempDir()
	cold := open(cfg)
	if got, err := os.ReadFile(filepath.Join(cold.AuthDir(), name)); err != nil || !bytes.Equal(got, canonical) {
		t.Fatal("cold store mirror did not reflect PostgreSQL JSONB representation")
	}
	records, err := cold.List(ctx)
	if err != nil || len(records) != 1 || records[0].ID != name {
		t.Fatal("cold store could not list the single confirmed credential")
	}
}
