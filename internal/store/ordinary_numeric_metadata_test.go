package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestOrdinaryStoreNumericMetadataRoundTrip(t *testing.T) {
	for _, kind := range []string{"git", "object", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "replica", "auths")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			var store coreauth.Store
			var reader func(string, string) (*coreauth.Auth, error)
			var remotePayload []byte
			switch kind {
			case "git":
				remote := setupGitRemoteRepository(t, root, "master", testBranchSpec{name: "master", contents: "base\n"})
				s := NewGitTokenStore(remote, "", "", "")
				s.SetBaseDir(dir)
				if err := s.EnsureRepository(); err != nil {
					t.Fatal(err)
				}
				store = s
				reader = s.readAuthFile
			case "object":
				s := &ObjectTokenStore{authDir: dir, cfg: ObjectStoreConfig{Bucket: "fixture"}, putObjectFn: func(_ context.Context, _ string, payload []byte, _ string) error {
					remotePayload = append([]byte(nil), payload...)
					return nil
				}}
				store = s
				reader = s.readAuthFile
			case "postgres":
				s := &PostgresStore{authDir: dir, cfg: PostgresStoreConfig{AuthTable: "fixture"}, persistAuthFn: func(_ context.Context, _ string, payload []byte) error {
					remotePayload = append([]byte(nil), payload...)
					return nil
				}}
				store = s
			}
			path := filepath.Join(dir, "ordinary.json")
			raw := []byte(` {"type":"test","disabled":false,"sequence":9007199254740992,"nested":{"other":9007199254740997},"n":1.00} `)
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			var record *coreauth.Auth
			if reader != nil {
				var err error
				record, err = reader(path, dir)
				if err != nil {
					t.Fatal(err)
				}
			} else {
				m, err := coreauth.DecodeCredentialMetadata(raw)
				if err != nil {
					t.Fatal(err)
				}
				record = &coreauth.Auth{ID: "ordinary.json", FileName: "ordinary.json", Provider: "test", Metadata: m}
			}
			record.Metadata["sequence"] = json.Number("9007199254740993")
			if _, err := store.Save(context.Background(), record); err != nil {
				t.Fatal(err)
			}
			want := []byte(`{"type":"test","disabled":false,"sequence":9007199254740993,"nested":{"other":9007199254740997},"n":1}`)
			saved, err := os.ReadFile(path)
			if err != nil || !coreauth.EqualCredentialJSON(saved, want) {
				t.Fatalf("updated bytes %s / %v", saved, err)
			}
			if kind == "git" {
				assertRemoteFileContents(t, filepath.Join(root, "remote.git"), "master", "auths/ordinary.json", string(saved))
			} else if !coreauth.EqualCredentialJSON(remotePayload, want) {
				t.Fatal("remote callback lost exact metadata")
			}
			stamp := time.Unix(1234567000, 0)
			if err := os.Chtimes(path, stamp, stamp); err != nil {
				t.Fatal(err)
			}
			record.Metadata["n"] = json.Number("1e0")
			for i := 0; i < 2; i++ {
				if _, err := store.Save(context.Background(), record); err != nil {
					t.Fatal(err)
				}
			}
			after, err := os.ReadFile(path)
			st, statErr := os.Stat(path)
			if err != nil || statErr != nil || string(after) != string(saved) || !st.ModTime().Equal(stamp) || st.Mode().Perm() != 0600 {
				t.Fatalf("repeated equivalent save rewrote file: %v/%v", err, statErr)
			}
			if reader != nil {
				cold, err := reader(path, dir)
				if err != nil {
					t.Fatal(err)
				}
				b, err := json.Marshal(cold.Metadata)
				if err != nil || !coreauth.EqualCredentialJSON(b, want) {
					t.Fatal("cold reader lost numeric metadata")
				}
				if err := os.WriteFile(path, append(saved, []byte(" {}")...), 0600); err != nil {
					t.Fatal(err)
				}
				if _, err := reader(path, dir); err == nil {
					t.Fatal("reader accepted trailing JSON")
				}
			}
		})
	}
}

func TestOrdinaryRemoteNumericSaveRetainsRemoteFailure(t *testing.T) {
	dir := t.TempDir()
	failure := errors.New("fixture remote unavailable")
	s := &ObjectTokenStore{authDir: dir, cfg: ObjectStoreConfig{Bucket: "fixture"}, putObjectFn: func(context.Context, string, []byte, string) error { return failure }}
	record := &coreauth.Auth{ID: "ordinary.json", FileName: "ordinary.json", Provider: "test", Metadata: map[string]any{"type": "test", "sequence": json.Number("9007199254740993")}}
	for i := 0; i < 2; i++ {
		if _, err := s.Save(context.Background(), record); !errors.Is(err, failure) {
			t.Fatalf("remote failure hidden: %v", err)
		}
	}
}
