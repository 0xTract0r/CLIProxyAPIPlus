package management

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

const providerReviewJSON = `[{"name":"fixture","base-url":"https://fixture.invalid/v1","models":[{"name":"fixture-model","alias":"fixture-alias"}]}]`

func providerReviewRouter(t *testing.T) (*Handler, *gin.Engine, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("port: 8317\nauth-dir: "+dir+"\nrequest-retry: 7\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(cfg, path, nil)
	r := gin.New()
	r.PUT("/providers", h.PutOpenAICompat)
	r.PATCH("/providers", h.PatchOpenAICompat)
	r.GET("/scalar", h.GetRequestRetry)
	return h, r, path
}

func TestProviderReviewConcurrentScalarReader(t *testing.T) {
	h, router, _ := providerReviewRouter(t)
	initial := h.cfg
	done := make(chan struct{})
	started := make(chan struct{})
	var readers sync.WaitGroup
	readers.Add(1)
	go func() {
		defer readers.Done()
		close(started)
		for {
			select {
			case <-done:
				return
			default:
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/scalar", nil))
			if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"request-retry":7`) {
				t.Error("neighboring read lost its scalar value")
				return
			}
		}
	}()
	defer func() { close(done); readers.Wait() }()
	<-started
	for range 8 {
		res := httptest.NewRecorder()
		router.ServeHTTP(res, httptest.NewRequest(http.MethodPut, "/providers", strings.NewReader(providerReviewJSON)))
		if res.Code != http.StatusOK {
			t.Fatalf("provider PUT returned %d: %s", res.Code, res.Body.String())
		}
	}
	h.mu.Lock()
	current := h.cfg
	h.mu.Unlock()
	if initial != current {
		t.Error("provider-only save replaced the config pointer used by scalar readers")
	}
}

func TestProviderReviewReloadReceivesCommittedConfigWithoutHoldingHandlerLock(t *testing.T) {
	h, router, path := providerReviewRouter(t)
	callbacks := make(chan *config.Config, 1)
	h.SetConfigReloadHook(func(_ context.Context, cfg *config.Config) {
		_ = h.configSnapshot()
		callbacks <- cfg
	})
	res := httptest.NewRecorder()
	router.ServeHTTP(res, httptest.NewRequest(http.MethodPut, "/providers", strings.NewReader(providerReviewJSON)))
	if res.Code != http.StatusOK {
		t.Fatalf("provider PUT returned %d: %s", res.Code, res.Body.String())
	}
	select {
	case cfg := <-callbacks:
		if len(cfg.OpenAICompatibility) != 1 || cfg.OpenAICompatibility[0].Name != "fixture" ||
			cfg.RequestRetry != 7 || len(cfg.OpenAICompatibility[0].Models) != 1 || cfg.OpenAICompatibility[0].Models[0].Alias != "fixture-alias" {
			t.Fatal("callback lost the committed provider or neighboring scalar")
		}
		written, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.SourceHash != config.ComputeSourceHash(written) {
			t.Error("reload callback source hash does not identify the committed YAML")
		}
		cfg.OpenAICompatibility[0].Models[0].Alias = "only-in-callback"
		if got := h.configSnapshot().OpenAICompatibility[0].Models[0].Alias; got != "fixture-alias" {
			t.Fatal("callback shares the published provider's model aliases")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("reload callback could not reenter the configuration lock")
	}
}

func TestProviderReviewFailedAliasPatchKeepsPublishedSnapshot(t *testing.T) {
	h, router, path := providerReviewRouter(t)
	notified := make(chan *config.Config, 2)
	h.SetConfigReloadHook(func(_ context.Context, cfg *config.Config) { notified <- cfg })
	mutate := func(method, body string, want int) {
		t.Helper()
		res := httptest.NewRecorder()
		router.ServeHTTP(res, httptest.NewRequest(method, "/providers", strings.NewReader(body)))
		if res.Code != want {
			t.Fatalf("%s returned %d, want %d", method, res.Code, want)
		}
	}
	await := func() *config.Config {
		t.Helper()
		select {
		case cfg := <-notified:
			return cfg
		case <-time.After(3 * time.Second):
			t.Fatal("missing reload")
			return nil
		}
	}
	mutate(http.MethodPut, providerReviewJSON, http.StatusOK)
	await()
	if err := os.Rename(path, path+".saved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	const update = `{"index":0,"value":{"models":[{"name":"fixture-model","alias":"new-fixture-alias"}]}}`
	mutate(http.MethodPatch, update, http.StatusInternalServerError)
	if got := h.configSnapshot().OpenAICompatibility[0].Models[0].Alias; got != "fixture-alias" {
		t.Fatalf("failed edit published alias %q", got)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".saved", path); err != nil {
		t.Fatal(err)
	}
	mutate(http.MethodPatch, update, http.StatusOK)
	if got := await().OpenAICompatibility[0].Models[0].Alias; got != "new-fixture-alias" {
		t.Fatalf("notification alias = %q, want latest committed alias", got)
	}
	disk, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := disk.OpenAICompatibility[0].Models[0].Alias; got != "new-fixture-alias" ||
		disk.OpenAICompatibility[0].BaseURL != "https://fixture.invalid/v1" || disk.RequestRetry != 7 {
		t.Fatalf("save failed to retain committed alias and adjacent fields: %q, retry=%d", got, disk.RequestRetry)
	}
}
