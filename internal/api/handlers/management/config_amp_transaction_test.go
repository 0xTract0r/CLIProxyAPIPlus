package management

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestOrdinaryAmpConfigTransaction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	const old = "ampcode:\n  model-mappings:\n    - from: saved\n      to: model\n"
	if err := os.WriteFile(path, []byte(old), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(cfg, path, nil)
	router := gin.New()
	started := make(chan struct{}, 2)
	router.GET("/ampcode/model-mappings", h.GetAmpModelMappings)
	router.PATCH("/ampcode/model-mappings", func(c *gin.Context) {
		if c.GetHeader("X-Test-Overlap") != "" {
			started <- struct{}{}
		}
		h.PatchAmpModelMappings(c)
	})
	call := func(method, body string, overlap bool) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(method, "/ampcode/model-mappings", strings.NewReader(body))
		if overlap {
			req.Header.Set("X-Test-Overlap", "1")
		}
		router.ServeHTTP(w, req)
		return w
	}
	saved := path + ".saved"
	if err := os.Rename(path, saved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if got := call(http.MethodPatch, `{"value":[{"from":"saved","to":"failed"}]}`, false); got.Code != http.StatusInternalServerError {
		t.Fatalf("failed save: HTTP %d", got.Code)
	}
	if got := call(http.MethodGet, "", false); strings.Contains(got.Body.String(), "failed") || !strings.Contains(got.Body.String(), "saved") {
		t.Fatal("failed mutation changed visible state")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(saved, path); err != nil {
		t.Fatal(err)
	}
	reopened, err := config.LoadConfig(path)
	if err != nil || len(reopened.AmpCode.ModelMappings) != 1 || reopened.AmpCode.ModelMappings[0].To != "model" {
		t.Fatalf("reopen after failure: %v", err)
	}
	results := make(chan int, 2)
	h.mu.Lock()
	for _, name := range []string{"first", "second"} {
		go func() {
			results <- call(http.MethodPatch, `{"value":[{"from":"`+name+`","to":"model"}]}`, true).Code
		}()
	}
	<-started
	<-started
	h.mu.Unlock()
	for range 2 {
		if code := <-results; code != http.StatusOK {
			t.Fatalf("concurrent update: HTTP %d", code)
		}
	}
	reopened, err = config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]string{}
	for _, mapping := range reopened.AmpCode.ModelMappings {
		have[mapping.From] = mapping.To
	}
	if len(have) != 3 || have["saved"] != "model" || have["first"] != "model" || have["second"] != "model" {
		t.Fatalf("cold mappings after overlapping updates: %v", have)
	}
}
