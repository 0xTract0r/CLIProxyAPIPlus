package management

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	sdkauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type gatePausedFileList struct {
	*sdkauth.FileTokenStore
	snapshotTaken chan struct{}
	resumeLoad    chan struct{}
}

func (s *gatePausedFileList) List(ctx context.Context) ([]*coreauth.Auth, error) {
	items, err := s.FileTokenStore.List(ctx)
	if len(items) != 1 || err != nil {
		return nil, errors.New("fixture must load exactly one synthetic credential")
	}
	close(s.snapshotTaken)
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.resumeLoad:
		return items, nil
	}
}

// removalWaitContext signals when the queued delete consults cancellation at
// the reload barrier. On a broken build the handler may respond before this.
type removalWaitContext struct {
	context.Context
	reached chan struct{}
	once    sync.Once
}

func (ctx *removalWaitContext) Done() <-chan struct{} {
	ctx.once.Do(func() { close(ctx.reached) })
	return ctx.Context.Done()
}

func TestLocalGinDeleteWaitsForSDKManagerLoad(t *testing.T) {
	for _, route := range []string{"/v0/management/auth-files", "/v8/management/credentials"} {
		t.Run(route, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			dir := t.TempDir()
			path := filepath.Join(dir, "selected.json")
			data := []byte(`{"type":"fixture","note":"synthetic"}`)
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			before, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			store := sdkauth.NewFileTokenStore()
			store.SetBaseDir(dir)
			paused := &gatePausedFileList{FileTokenStore: store, snapshotTaken: make(chan struct{}), resumeLoad: make(chan struct{})}
			m := coreauth.NewManager(paused, nil, nil)
			h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: dir}, m)
			h.tokenStore = store
			router := gin.New()
			router.DELETE("/v0/management/auth-files", h.DeleteAuthFile)
			router.DELETE("/v8/management/credentials", h.DeleteAuthFileV8)

			resume := sync.OnceFunc(func() { close(paused.resumeLoad) })
			defer resume()
			loaded := make(chan error, 1)
			go func() { loaded <- m.Load(ctx) }()
			select {
			case <-paused.snapshotTaken:
			case err := <-loaded:
				t.Fatalf("SDK load quit before finding the fixture: %v", err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if len(m.List()) != 0 {
				t.Fatal("fixture starts with zero runtime registrations")
			}

			serve := func(callCtx context.Context) *httptest.ResponseRecorder {
				out := httptest.NewRecorder()
				router.ServeHTTP(out, httptest.NewRequestWithContext(callCtx, http.MethodDelete, route+"?name=selected.json", nil))
				return out
			}
			requestCtx := &removalWaitContext{Context: ctx, reached: make(chan struct{})}
			responses := make(chan *httptest.ResponseRecorder, 1)
			go func() { responses <- serve(requestCtx) }()
			var response *httptest.ResponseRecorder
			select {
			case response = <-responses:
			case <-requestCtx.reached:
				// Let the already-read store snapshot publish before deletion can commit.
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			resume()
			if err := <-loaded; err != nil {
				t.Fatal(err)
			}
			if response == nil {
				select {
				case response = <-responses:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
			after, statErr := os.Stat(path)
			got, readErr := os.ReadFile(path)
			_, runtimeExists := m.GetByID("selected.json")
			if response.Code != http.StatusConflict || statErr != nil || readErr != nil ||
				!runtimeExists || !os.SameFile(before, after) || !bytes.Equal(got, data) {
				t.Fatalf("after Load: status=%d stat=%v read=%v registered=%v body=%s",
					response.Code, statErr, readErr, runtimeExists, response.Body)
			}
			if out := serve(ctx); out.Code != http.StatusOK {
				t.Fatalf("retry: status=%d body=%s", out.Code, out.Body)
			}
			_, runtimeExists = m.GetByID("selected.json")
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) || runtimeExists {
				t.Fatalf("retry failed to remove disk and runtime entry: stat=%v registered=%v", err, runtimeExists)
			}
		})
	}
}
