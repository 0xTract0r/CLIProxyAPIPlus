package management

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/xai"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	sdkauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type xaiSyntheticLogin struct{ record *coreauth.Auth }

func (a xaiSyntheticLogin) Provider() string            { return "xai" }
func (a xaiSyntheticLogin) RefreshLead() *time.Duration { return nil }
func (a xaiSyntheticLogin) Login(context.Context, *config.Config, *sdkauth.LoginOptions) (*coreauth.Auth, error) {
	return a.record, nil
}

func syntheticXAIRecord(email, subject, token string) *coreauth.Auth {
	name := xai.CredentialFileName(email, subject)
	return &coreauth.Auth{
		Provider: "xai", ID: name, FileName: name,
		Storage: &xai.TokenStorage{Email: email, Subject: subject, AccessToken: token, RefreshToken: token + "-refresh"},
		Metadata: map[string]any{"type": "xai", "email": email, "sub": subject,
			"access_token": token, "refresh_token": token + "-refresh"},
	}
}

func writeXAIStorageFixture(t *testing.T, path, email, subject string) []byte {
	t.Helper()
	data, err := json.Marshal(map[string]any{"type": "xai", "email": email, "sub": subject,
		"access_token": "old", "refresh_token": "old-refresh", "note": "keep note", "disabled": true})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return data
}

func assertXAIStorageBytes(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("credential fixture changed: %s: %v", filepath.Base(path), err)
	}
}

func TestXAILocalLoginPersistenceEntrypoints(t *testing.T) {
	type saveFunc func(*config.Config, *coreauth.Auth) (string, error)
	login := func(store *sdkauth.FileTokenStore) saveFunc {
		return func(cfg *config.Config, record *coreauth.Auth) (string, error) {
			_, path, err := sdkauth.NewManager(store, xaiSyntheticLogin{record}).Login(context.Background(), "xai", cfg, nil)
			return path, err
		}
	}
	management := func(store *sdkauth.FileTokenStore) saveFunc {
		return func(cfg *config.Config, record *coreauth.Auth) (string, error) {
			h := NewHandler(cfg, "", nil)
			h.tokenStore = store
			return h.saveTokenRecordWithConfig(context.Background(), record, cfg)
		}
	}
	for _, entry := range []struct {
		name   string
		create func(*sdkauth.FileTokenStore) saveFunc
	}{
		{"sdk-login", login}, {"management-save", management},
	} {
		t.Run(entry.name, func(t *testing.T) {
			for _, scenario := range []string{"migrate", "different-subject", "unknown-predecessor", "failed-write", "coexisting", "richer-name", "symlink-destination"} {
				t.Run(scenario, func(t *testing.T) {
					dir := t.TempDir()
					cfg := &config.Config{AuthDir: dir}
					store := sdkauth.NewFileTokenStore()
					store.SetBaseDir(t.TempDir()) // Both entry points must use the accepted AuthDir.
					save := entry.create(store)
					const email, subject = "a+b@example.invalid", "synthetic-subject"
					record := syntheticXAIRecord(email, subject, "new")
					dest := filepath.Join(dir, record.FileName)
					legacy := filepath.Join(dir, "xai-a-b@example.invalid.json")
					old := writeXAIStorageFixture(t, legacy, email, subject)
					switch scenario {
					case "different-subject":
						if record.FileName != xai.CredentialFileName(email, "different") {
							t.Fatal("fixture must reach the same named destination")
						}
						prior := writeXAIStorageFixture(t, dest, email, "different")
						if _, err := save(cfg, record); err == nil {
							t.Fatal("overwrote a different subject")
						}
						assertXAIStorageBytes(t, dest, prior)
						assertXAIStorageBytes(t, legacy, old)
						return
					case "symlink-destination":
						if err := os.Symlink(legacy, dest); err != nil {
							t.Fatal(err)
						}
						if _, err := save(cfg, record); err == nil {
							t.Fatal("accepted an aliased destination")
						}
						if link, err := os.Readlink(dest); err != nil || link != legacy {
							t.Fatalf("destination link changed: %v", err)
						}
						assertXAIStorageBytes(t, legacy, old)
						return
					case "unknown-predecessor":
						old = writeXAIStorageFixture(t, legacy, email, "")
					case "failed-write":
						record.Metadata["unserializable"] = make(chan int)
						if _, err := save(cfg, record); err == nil {
							t.Fatal("expected storage write failure")
						}
						assertXAIStorageBytes(t, legacy, old)
						if _, err := os.Stat(dest); !os.IsNotExist(err) {
							t.Fatalf("failed write left destination: %v", err)
						}
						return
					case "coexisting":
						writeXAIStorageFixture(t, dest, email, subject)
					case "richer-name":
						if err := os.Rename(legacy, dest); err != nil {
							t.Fatal(err)
						}
						record = syntheticXAIRecord("", subject, "new")
					}
					saved, err := save(cfg, record)
					if err != nil || filepath.Clean(saved) != filepath.Clean(dest) {
						t.Fatalf("save = %q, %v", saved, err)
					}
					raw, err := os.ReadFile(dest)
					if err != nil {
						t.Fatal(err)
					}
					var current map[string]any
					if err := json.Unmarshal(raw, &current); err != nil ||
						current["access_token"] != "new" || current["email"] != email || current["sub"] != subject {
						t.Fatalf("fresh credential missing: %v", err)
					}
					if scenario == "unknown-predecessor" {
						if current["note"] != nil {
							t.Fatal("unverified metadata was inherited")
						}
					} else if current["note"] != "keep note" || current["disabled"] != true {
						t.Fatal("operator metadata was lost")
					}
					if scenario == "migrate" || scenario == "richer-name" {
						if _, err := os.Stat(legacy); !os.IsNotExist(err) {
							t.Fatalf("verified migration left old filename: %v", err)
						}
						other := syntheticXAIRecord("a-b@example.invalid", "other", "separate")
						if other.FileName == record.FileName {
							t.Fatal("sanitized email collision")
						}
						if _, err := save(cfg, other); err != nil {
							t.Fatal(err)
						}
						assertXAIStorageBytes(t, dest, raw)
					} else {
						assertXAIStorageBytes(t, legacy, old)
					}
				})
			}
		})
	}
}

func TestXAILocalLoginConcurrentDestinationClaims(t *testing.T) {
	cfg := &config.Config{AuthDir: t.TempDir()}
	const email = "owner@example.invalid"
	start := make(chan struct{})
	results := make(chan error, 2)
	var group sync.WaitGroup
	for _, subject := range []string{"one", "two"} {
		group.Go(func() {
			<-start
			record := syntheticXAIRecord(email, subject, subject)
			_, _, err := sdkauth.NewManager(sdkauth.NewFileTokenStore(), xaiSyntheticLogin{record}).Login(context.Background(), "xai", cfg, nil)
			results <- err
		})
	}
	close(start)
	group.Wait()
	if first, second := <-results, <-results; (first == nil) == (second == nil) {
		t.Fatalf("wanted exactly one destination claim: %v, %v", first, second)
	}
}

func TestXAILocalLoginKeepsReplacedPredecessor(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{AuthDir: dir}
	const email, subject = "old@example.invalid", "known"
	legacy := filepath.Join(dir, "xai-"+email+".json")
	writeXAIStorageFixture(t, legacy, email, subject)
	h := NewHandler(cfg, "", nil)
	h.tokenStore = sdkauth.NewFileTokenStore()
	var replacement []byte
	h.postAuthHook = func(context.Context, *coreauth.Auth) error {
		if err := os.Remove(legacy); err != nil {
			return err
		}
		replacement = writeXAIStorageFixture(t, legacy, email, "different")
		return nil
	}
	record := syntheticXAIRecord(email, subject, "new")
	path, err := h.saveTokenRecordWithConfig(context.Background(), record, cfg)
	if err == nil || path != filepath.Join(dir, record.FileName) {
		t.Fatalf("expected saved destination and cleanup conflict: %q, %v", path, err)
	}
	assertXAIStorageBytes(t, legacy, replacement)
	data, err := os.ReadFile(path)
	if err != nil || !bytes.Contains(data, []byte(`"access_token": "new"`)) {
		t.Fatalf("saved destination lost: %v", err)
	}
}
