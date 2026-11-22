package api

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/usage"
)

func TestRegisteredPricingWriteFailureLeavesOverride(t *testing.T) {
	const admin = "pricing-fixture-admin"
	t.Setenv("MANAGEMENT_PASSWORD", admin)
	server := newTestServer(t)
	dir := filepath.Join(t.TempDir(), "pricing")
	path := filepath.Join(dir, "catalog.json")
	catalog := usage.NewPricingCatalogManager()
	if err := catalog.SetPersistencePath(path); err != nil {
		t.Fatal(err)
	}
	server.mgmt.SetUsageStatistics(usage.NewRequestStatisticsWithCatalog(catalog))
	const base = "/v0/management/usage/pricing"
	const override = base + "/overrides/fixture-model"
	const payload = `{"input_usd_per_mtok":1,"output_usd_per_mtok":3}`
	call := func(method, url, body string, want int) []byte {
		t.Helper()
		req := httptest.NewRequest(method, url, strings.NewReader(body))
		req.RemoteAddr = "127.0.0.1:1234"
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Management-Key", admin)
		res := httptest.NewRecorder()
		server.Handler().ServeHTTP(res, req)
		if res.Code != want {
			t.Errorf("%s %s=%d want %d", method, url, res.Code, want)
		}
		return res.Body.Bytes()
	}
	assertCurrent := func() {
		t.Helper()
		var got struct{ Pricing usage.PricingSnapshot }
		if err := json.Unmarshal(call("GET", base, "", 200), &got); err != nil || got.Pricing.Overrides[usage.NormalizeCanonicalModelID("fixture-model")].InputUSDPerMTok != 1 {
			t.Error("current override changed after failure")
		}
	}
	call("PUT", override, payload, 200)
	assertCurrent()
	saved := dir + ".saved"
	if err := os.Rename(dir, saved); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, nil, 0600); err != nil {
		t.Fatal(err)
	}
	call("PUT", override, `{"input_usd_per_mtok":4,"output_usd_per_mtok":5}`, 500)
	assertCurrent()
	call("DELETE", override, "", 500)
	assertCurrent()
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(saved, dir); err != nil {
		t.Fatal(err)
	}
	reopened := usage.NewPricingCatalogManager()
	if err := reopened.SetPersistencePath(path); err != nil {
		t.Fatal(err)
	}
	if reopened.Snapshot(nil).Overrides[usage.NormalizeCanonicalModelID("fixture-model")].InputUSDPerMTok != 1 {
		t.Fatal("reopened override differs")
	}
	call("DELETE", override, "", 200)
	call("DELETE", override, "", 404)
	if err := reopened.LoadFromPersistence(); err != nil {
		t.Fatal(err)
	}
	if len(reopened.Snapshot(nil).Overrides) != 0 {
		t.Fatal("successful delete did not persist")
	}
}
