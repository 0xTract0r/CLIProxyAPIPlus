package executor

import (
	"net/http"
	"sync"
	"testing"

	claudeauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/claude"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestOrdinaryClaudeSDKMetadataRace(t *testing.T) {
	e := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Metadata: map[string]any{"access_token": "race-fixture"}}
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			claudeauth.StoreMetadataString(&auth.Metadata, "access_token", "race-fixture")
		}()
		go func() {
			defer wg.Done()
			req, err := http.NewRequest(http.MethodGet, "https://api.anthropic.com/v1/messages", nil)
			if err != nil {
				t.Error(err)
				return
			}
			if err = e.PrepareRequest(req, auth); err != nil || req.Header.Get("Authorization") != "Bearer race-fixture" {
				t.Errorf("prepare error=%v", err)
			}
		}()
	}
	wg.Wait()
}
