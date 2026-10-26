package auth

import (
	"context"
	"encoding/json"
	"fmt"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// 同一 Store 及不同 Store 同时写同一路径/独立路径，最终文件必须完整。
func TestFileTokenStore467ConcurrentPaths(t *testing.T) {
	for _, sharedStore := range []bool{true, false} {
		t.Run(fmt.Sprintf("shared_store_%t", sharedStore), func(t *testing.T) {
			dir := t.TempDir()
			shared := NewFileTokenStore()
			shared.SetBaseDir(dir)
			var wg sync.WaitGroup
			start := make(chan struct{})
			errs := make(chan error, 24)
			for i := 0; i < 24; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					<-start
					store := shared
					if !sharedStore {
						store = NewFileTokenStore()
						store.SetBaseDir(dir)
					}
					name := "same.json"
					if i%2 == 0 {
						name = fmt.Sprintf("distinct-%d.json", i)
					}
					a := &coreauth.Auth{ID: name, FileName: name, Provider: "claude", Metadata: map[string]any{"type": "claude", "access_token": fmt.Sprintf("fresh-%d", i)}}
					_, err := store.Save(context.Background(), a)
					errs <- err
				}(i)
			}
			close(start)
			wg.Wait()
			close(errs)
			for err := range errs {
				if err != nil {
					t.Fatal(err)
				}
			}
			names := []string{"same.json"}
			for i := 0; i < 24; i += 2 {
				names = append(names, fmt.Sprintf("distinct-%d.json", i))
			}
			for _, name := range names {
				raw, err := os.ReadFile(filepath.Join(dir, name))
				if err != nil {
					t.Fatal(err)
				}
				var m map[string]any
				if err = json.Unmarshal(raw, &m); err != nil {
					t.Fatal(err)
				}
				if m["type"] != "claude" || m["access_token"] == "" {
					t.Fatalf("invalid final record: %s", raw)
				}
			}
		})
	}
}
