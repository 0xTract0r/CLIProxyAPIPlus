package management

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/usage"
)

func TestUsagePricingOverrideLifecycle(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)
	stats := usage.NewRequestStatisticsWithCatalog(usage.NewPricingCatalogManager())
	handler := &Handler{usageStats: stats}

	router := gin.New()
	router.GET("/v0/management/usage/pricing", handler.GetUsagePricing)
	router.PUT("/v0/management/usage/pricing/overrides/:model", handler.PutUsagePricingOverride)
	router.DELETE("/v0/management/usage/pricing/overrides/:model", handler.DeleteUsagePricingOverride)

	body := map[string]any{
		"display_name":              "gpt-5.4 manual",
		"input_usd_per_mtok":        4.0,
		"cached_input_usd_per_mtok": 0.4,
		"output_usd_per_mtok":       16.0,
		"cache_write_usd_per_mtok":  0.0,
	}
	payload, _ := json.Marshal(body)

	putReq := httptest.NewRequest(http.MethodPut, "/v0/management/usage/pricing/overrides/gpt-5.4", bytes.NewReader(payload))
	putReq.Header.Set("Content-Type", "application/json")
	putRec := httptest.NewRecorder()
	router.ServeHTTP(putRec, putReq)
	if putRec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200 body=%s", putRec.Code, putRec.Body.String())
	}

	getRec := httptest.NewRecorder()
	router.ServeHTTP(getRec, httptest.NewRequest(http.MethodGet, "/v0/management/usage/pricing", nil))
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200 body=%s", getRec.Code, getRec.Body.String())
	}

	var response struct {
		Pricing usage.PricingSnapshot `json:"pricing"`
	}
	if err := json.Unmarshal(getRec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode GET response: %v", err)
	}
	override := response.Pricing.Overrides["gpt-5.4"]
	if override.DisplayName != "gpt-5.4 manual" {
		t.Fatalf("override display name = %q, want gpt-5.4 manual", override.DisplayName)
	}
	if response.Pricing.Models["gpt-5.4"].Source != "override" {
		t.Fatalf("effective source = %q, want override", response.Pricing.Models["gpt-5.4"].Source)
	}

	deleteRec := httptest.NewRecorder()
	router.ServeHTTP(deleteRec, httptest.NewRequest(http.MethodDelete, "/v0/management/usage/pricing/overrides/gpt-5.4", nil))
	if deleteRec.Code != http.StatusOK {
		t.Fatalf("DELETE status = %d, want 200 body=%s", deleteRec.Code, deleteRec.Body.String())
	}
}
