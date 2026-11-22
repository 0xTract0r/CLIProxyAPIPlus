package management

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	sdkauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type ordinarySaveOnlyStore struct {
	saved          *coreauth.Auth
	lists, deletes int
}

func (s *ordinarySaveOnlyStore) List(context.Context) ([]*coreauth.Auth, error) {
	s.lists++
	return nil, errors.New("fixture has no list permission")
}
func (s *ordinarySaveOnlyStore) Delete(context.Context, string) error {
	s.deletes++
	return errors.New("fixture has no delete permission")
}
func (s *ordinarySaveOnlyStore) Save(_ context.Context, record *coreauth.Auth) (string, error) {
	s.saved = record.Clone()
	return "opaque-store-receipt", nil
}

func TestOrdinaryXAICustomStoreSameFileMetadata(t *testing.T) {
	for _, entry := range []string{"sdk", "management"} {
		for _, scenario := range []string{"same subject", "different subject", "external target", "incoming note"} {
			t.Run(entry+"/"+scenario, func(t *testing.T) {
				dir := t.TempDir()
				cfg := &config.Config{AuthDir: dir}
				store := &ordinarySaveOnlyStore{}
				record := syntheticXAIRecord("fixture@example.invalid", "synthetic-subject", "synthetic")
				target := filepath.Join(dir, record.FileName)
				subject := "synthetic-subject"
				if scenario == "different subject" {
					subject = "unrelated-synthetic"
				}
				if scenario == "external target" {
					if record.Attributes == nil {
						record.Attributes = map[string]string{}
					}
					record.Attributes[coreauth.AttributePath] = filepath.Join(t.TempDir(), record.FileName)
				}
				if scenario == "incoming note" {
					record.Metadata["note"] = "operator updated"
				}
				prior := writeXAIStorageFixture(t, target, "fixture@example.invalid", subject)
				var saved string
				var err error
				if entry == "sdk" {
					_, saved, err = sdkauth.NewManager(store, xaiSyntheticLogin{record}).Login(context.Background(), "xai", cfg, nil)
				} else {
					handler := NewHandlerWithoutConfigFilePath(cfg, nil)
					handler.tokenStore = store
					saved, err = handler.saveTokenRecord(context.Background(), record)
				}
				if err != nil || saved != "opaque-store-receipt" || store.saved == nil || store.lists != 0 || store.deletes != 0 {
					t.Fatalf("external save=%q err=%v list=%d delete=%d", saved, err, store.lists, store.deletes)
				}
				m := store.saved.Metadata
				if scenario == "same subject" && (m["note"] != "keep note" || m["disabled"] != true) {
					t.Error("same record metadata was lost")
				}
				if scenario == "incoming note" && (m["note"] != "operator updated" || m["disabled"] != true) {
					t.Error("explicit note or prior disabled state was lost")
				}
				if (scenario == "different subject" || scenario == "external target") && (m["note"] != nil || m["disabled"] == true) {
					t.Error("unrelated metadata was copied")
				}
				got, err := os.ReadFile(target)
				if err != nil || string(got) != string(prior) {
					t.Fatal("fixture file was modified")
				}
			})
		}
	}
}
