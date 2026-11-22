package management

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

func TestCatalogRefreshDoesNotEchoPrivateURL(t *testing.T) {
	const tag = "fixture-only-signature"
	cfg := &config.Config{Models: registry.CatalogSources{Catalog: "https://fixture-user:fixture-pass@example.invalid/catalog?signature=" + tag + "#fixture-fragment", CodexCatalog: "https://example.invalid/c", DevinCatalog: "https://example.invalid/d"}}
	h := NewHandlerWithoutConfigFilePath(cfg, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest("POST", "/v8/management/models/refresh", nil).WithContext(ctx)
	h.RefreshModelDefinitions(c)
	if rec.Code != 502 {
		t.Fatalf("wrong fixture response: %d", rec.Code)
	}
	var report map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report["catalog"] != "https://example.invalid/catalog" {
		t.Fatal("public location missing")
	}
	for _, s := range []string{tag, "fixture-user", "fixture-pass", "fixture-fragment"} {
		if strings.Contains(rec.Body.String(), s) {
			t.Fatal("refresh diagnostic contains private URL material")
		}
	}
	const rawURL = "https://fixture-user:fixture-pass@example.invalid/path?token=" + tag + "#fixture-fragment"
	out := publicCatalogRefreshResult(registry.CatalogRefreshResult{Catalog: rawURL, Catalogs: map[string]registry.CatalogSourceStatus{"catalog": {Source: rawURL, Error: "Get " + rawURL + ": denied"}}})
	if got := out.Catalogs["catalog"]; got.Source != "https://example.invalid/path" || got.Error == "" || strings.Contains(got.Error, rawURL) || strings.Contains(got.Error, tag) {
		t.Fatalf("unsafe status: %q", got.Source)
	}
}
