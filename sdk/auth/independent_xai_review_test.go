package auth

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/xai"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type reviewXAILogin struct{ record *coreauth.Auth }

func (a reviewXAILogin) Provider() string            { return "xai" }
func (a reviewXAILogin) RefreshLead() *time.Duration { return nil }
func (a reviewXAILogin) Login(context.Context, *config.Config, *LoginOptions) (*coreauth.Auth, error) {
	return a.record, nil
}

func reviewXAIRecord(email, subject string) *coreauth.Auth {
	name := xai.CredentialFileName(email, subject)
	return &coreauth.Auth{Provider: "xai", ID: name, FileName: name,
		Storage:  &xai.TokenStorage{Email: email, Subject: subject, AccessToken: "synthetic"},
		Metadata: map[string]any{"type": "xai", "email": email, "sub": subject, "access_token": "synthetic"}}
}

func TestIndependentXAICanonicalDestinationKeepsDifferentSubject(t *testing.T) {
	{
		email := "fixture@example.invalid"
		first, second := reviewXAIRecord(email, "fixture-A"), reviewXAIRecord(email, "fixture-B")
		if first.FileName != second.FileName {
			t.Fatal("fixture needs the same canonical file")
		}
		dir := t.TempDir()
		store := NewFileTokenStore()
		store.SetBaseDir(dir)
		if _, _, err := NewManager(store, reviewXAILogin{first}).Login(context.Background(), "xai", &config.Config{AuthDir: dir}, nil); err != nil {
			t.Fatal(err)
		}
		_, _, denied := NewManager(store, reviewXAILogin{second}).Login(context.Background(), "xai", &config.Config{AuthDir: dir}, nil)
		path := filepath.Join(dir, first.FileName)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var saved map[string]any
		if err := json.Unmarshal(data, &saved); err != nil {
			t.Fatal(err)
		}
		if denied == nil || saved["sub"] != "fixture-A" {
			t.Fatalf("second login error = %v; same-name record now has subject %q, want fixture-A preserved", denied, saved["sub"])
		}
	}
}
