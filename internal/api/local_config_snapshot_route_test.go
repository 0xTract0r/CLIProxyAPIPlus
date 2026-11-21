package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func TestOrdinaryManagementConfigReadsDuringRuntimeReload(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "synthetic-config-snapshot")
	server := newTestServer(t)
	cfg := server.cfg.CloneForRuntime()
	versions := []int{701, 702}
	cfg.RequestRetry = versions[0]
	server.mgmt.SetConfig(cfg)
	var workers sync.WaitGroup
	start := make(chan struct{})
	workers.Go(func() {
		<-start
		for i := range 32 {
			next := cfg.CloneForRuntime()
			next.RequestRetry = versions[i%2]
			server.mgmt.SetConfig(next)
		}
	})
	for range 2 {
		workers.Go(func() {
			<-start
			for range 16 {
				req := httptest.NewRequest(http.MethodGet, "/v0/management/config", nil)
				req.RemoteAddr = "127.0.0.1:2345"
				req.Header.Set("X-Management-Key", "synthetic-config-snapshot")
				res := httptest.NewRecorder()
				server.Handler().ServeHTTP(res, req)
				var body map[string]any
				err := json.Unmarshal(res.Body.Bytes(), &body)
				retry, ok := body["request-retry"].(float64)
				if res.Code != http.StatusOK || err != nil || !ok || retry != 701 && retry != 702 {
					t.Errorf("config response status=%d, request-retry=%v, decode=%v", res.Code, body["request-retry"], err)
					return
				}
			}
		})
	}
	close(start)
	workers.Wait()
}
