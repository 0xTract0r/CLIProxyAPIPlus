package auth

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestIndependentXAISameSubjectLoginKeepsLatestNote(t *testing.T) {
	dir := t.TempDir()
	store := NewFileTokenStore()
	store.SetBaseDir(dir)
	ctx := coreauth.WithAuthCreationIntent(context.Background())
	old := reviewXAIRecord("fixture@example.invalid", "fixture-A")
	old.Metadata["note"] = "before"
	path, err := store.Save(ctx, old)
	if err != nil {
		t.Fatal(err)
	}
	login := reviewXAIRecord("fixture@example.invalid", "fixture-A")
	plan, err := PrepareXAILogin(ctx, store, login)
	if err != nil {
		t.Fatal(err)
	}
	coreauth.MergeExistingAuthMetadata(login, plan.PriorMetadata)

	// Simulate an ordinary concurrent save that finishes in the window
	// between the login's prepare and its final destination lock.
	old.Metadata["note"] = "updated while login prepared"
	old.Disabled = true
	if _, err := store.Save(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	if _, err := plan.Save(ctx, store, login); !errors.Is(err, ErrXAILoginFileChanged) {
		t.Fatalf("stale login error = %v", err)
	}
	retry := reviewXAIRecord("fixture@example.invalid", "fixture-A")
	if _, _, err := NewManager(store, reviewXAILogin{retry}).Login(context.Background(), "xai", &config.Config{AuthDir: dir}, nil); err != nil {
		t.Fatalf("retry: %v", err)
	}
	file, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatal(err)
	}
	var data map[string]any
	if err := json.Unmarshal(file, &data); err != nil {
		t.Fatal(err)
	}
	if data["note"] != "updated while login prepared" || data["disabled"] != true {
		t.Fatalf("ordinary committed note lost; final note/disabled = %q/%v", data["note"], data["disabled"])
	}
}

type reviewCustomXAIStore struct {
	saved bool
}

var errCustomListUnavailable = errors.New("synthetic custom store has write scope only")

func (s *reviewCustomXAIStore) List(context.Context) ([]*coreauth.Auth, error) {
	return nil, errCustomListUnavailable
}
func (s *reviewCustomXAIStore) Save(context.Context, *coreauth.Auth) (string, error) {
	s.saved = true
	return "ordinary-opaque-store-receipt", nil
}
func (s *reviewCustomXAIStore) Delete(context.Context, string) error {
	return errors.New("unexpected delete")
}

func TestIndependentXAICustomStoreSaveContract(t *testing.T) {
	store := &reviewCustomXAIStore{}
	login := reviewXAIRecord("fixture@example.invalid", "fixture-A")
	_, _, err := NewManager(store, reviewXAILogin{login}).Login(context.Background(), "xai", &config.Config{}, nil)
	if err != nil || !store.saved {
		t.Fatalf("custom store save = %v; login error = %v", store.saved, err)
	}
}
