package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/usage"
)

func TestCombinedOrdinaryUsageRegisteredImportAndTiePaging(t *testing.T) {
	const admin = "synthetic-combined-usage-admin"
	t.Setenv("MANAGEMENT_PASSWORD", admin)
	server := newTestServer(t)
	call := func(method, path, body string, authorized bool, status int) []byte {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("Content-Type", "application/json")
		if authorized {
			req.Header.Set("X-Management-Key", admin)
		}
		res := httptest.NewRecorder()
		server.Handler().ServeHTTP(res, req)
		if res.Code != status {
			t.Fatalf("%s %s status=%d want=%d: %s", method, path, res.Code, status, res.Body)
		}
		return res.Body.Bytes()
	}
	const mgmt = "/v0/management"
	const tie = "2026-11-20T12:00:00.123456789Z"
	const nextTime = "2026-11-20T12:00:01.123456789Z"
	raw := `{"version":2,"usage":{"apis":{
		"synthetic-one":{"models":{"model-a":{"details":[{"timestamp":"` + tie + `","tokens":{"input_tokens":10}}]}}},
		"synthetic-two":{"models":{"model-b":{"details":[{"timestamp":"` + tie + `","failed":true,"tokens":{"input_tokens":20}},{"timestamp":"` + nextTime + `","tokens":{"input_tokens":30}}]}}}
	}}}`
	call(http.MethodPost, mgmt+"/usage/import", raw, false, http.StatusUnauthorized)
	call(http.MethodPost, mgmt+"/usage/import", strings.Replace(raw, `"version":2`, `"version":3`, 1), true, http.StatusBadRequest)
	for i, want := range []map[string]int{
		{"added": 3, "skipped": 0, "total_requests": 3, "failed_requests": 1},
		{"added": 0, "skipped": 3, "total_requests": 3, "failed_requests": 1},
	} {
		data := call(http.MethodPost, mgmt+"/usage/import", raw, true, http.StatusOK)
		var got map[string]int
		if err := json.Unmarshal(data, &got); err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("import %d=%v err=%v; want %v", i+1, got, err, want)
		}
	}
	read := call(http.MethodGet, mgmt+"/usage?include_details=false", "", true, http.StatusOK)
	var summary struct {
		Usage  usage.StatisticsSnapshot `json:"usage"`
		Failed int                      `json:"failed_requests"`
	}
	if err := json.Unmarshal(read, &summary); err != nil || summary.Usage.TotalRequests != 3 || summary.Failed != 1 || summary.Usage.TotalTokens != 60 {
		t.Fatalf("unexpected summary totals/failed/tokens: %d/%d/%d; %v", summary.Usage.TotalRequests, summary.Failed, summary.Usage.TotalTokens, err)
	}
	type page struct {
		Usage     usage.StatisticsSnapshot `json:"usage"`
		HasMore   bool                     `json:"has_more"`
		NextSince string                   `json:"next_since"`
	}
	getPage := func(query string) (page, []string) {
		t.Helper()
		var pg page
		data := call(http.MethodGet, mgmt+"/usage/export?"+query, "", true, http.StatusOK)
		if err := json.Unmarshal(data, &pg); err != nil {
			t.Fatal(err)
		}
		keys := []string{}
		for key, api := range pg.Usage.APIs {
			for model, stat := range api.Models {
				for _, d := range stat.Details {
					keys = append(keys, key+"/"+model+"/"+d.Timestamp.UTC().Format(time.RFC3339Nano))
				}
			}
		}
		sort.Strings(keys)
		return pg, keys
	}
	first, keys1 := getPage("limit=1")
	if !first.HasMore || first.NextSince != tie || !reflect.DeepEqual(keys1, []string{"synthetic-one/model-a/" + tie, "synthetic-two/model-b/" + tie}) {
		t.Fatalf("page 1 has_more=%t next=%q records=%v", first.HasMore, first.NextSince, keys1)
	}
	second, keys2 := getPage("limit=1&since=" + url.QueryEscape(first.NextSince))
	if second.HasMore || second.NextSince != "" || !reflect.DeepEqual(keys2, []string{"synthetic-two/model-b/" + nextTime}) {
		t.Fatalf("page 2 has_more=%t next=%q records=%v", second.HasMore, second.NextSince, keys2)
	}
	whole, all := getPage("limit=0")
	if whole.HasMore || !reflect.DeepEqual(all, append(keys1, keys2...)) {
		t.Fatalf("unwindowed records %v", all)
	}
	server.mgmt.SetUsageStatistics(nil)
	call(http.MethodPost, mgmt+"/usage/import", raw, true, http.StatusBadRequest)
	t.Logf("import added=3 then skipped=3; totals 3/60 tokens/1 failure; tie pages 2+1 with precise cursor %s", first.NextSince)
}
