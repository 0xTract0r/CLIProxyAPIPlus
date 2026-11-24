package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usage"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v8/sdk/access"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	exec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdkusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

const runtimeFixtureModel = "local-usage-fixture"

type usageFixtureExecutor struct {
	auth.ProviderExecutor
	record   sdkusage.Record
	sequence atomic.Uint64
}

func (e *usageFixtureExecutor) Identifier() string { return "usage-fixture" }
func (e *usageFixtureExecutor) Execute(ctx context.Context, _ *auth.Auth, req exec.Request, _ exec.Options) (exec.Response, error) {
	r := e.record
	r.Model = req.Model
	r.RequestID = fmt.Sprintf("%s-%d", r.RequestID, e.sequence.Add(1))
	r.RequestedAt = time.Now()
	sdkusage.PublishRecord(ctx, r)
	if r.Failed {
		return exec.Response{}, errors.New("synthetic model service failure")
	}
	return exec.Response{Payload: []byte(`{"id":"fixture-chat","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"fixture reply"}}]}`)}, nil
}

type usageFixtureObserver func(sdkusage.Record)

func (f usageFixtureObserver) HandleUsage(_ context.Context, r sdkusage.Record) { f(r) }

func TestUsageSDKChatAttributionEndToEnd(t *testing.T) {
	const admin = "synthetic-runtime-admin"
	const inA = "synthetic-runtime-inbound-a"
	const inB = "synthetic-runtime-inbound-b"
	const upstream = "synthetic-runtime-upstream"
	const header = "synthetic-runtime-header"
	const raw = "synthetic-runtime-raw-prompt"
	const detached = "synthetic-runtime-detached"
	t.Setenv("MANAGEMENT_PASSWORD", admin)
	wasEnabled := usage.StatisticsEnabled()
	usage.SetStatisticsEnabled(true)
	t.Cleanup(func() { usage.SetStatisticsEnabled(wasEnabled) })
	delivered := make(chan string, 16)
	sdkusage.RegisterNamedPlugin("runtime-e2e-observer", usageFixtureObserver(func(r sdkusage.Record) {
		if strings.HasPrefix(r.RequestID, "synthetic-runtime-") {
			delivered <- r.RequestID
		}
	}))
	t.Cleanup(func() {
		sdkusage.RegisterNamedPlugin("runtime-e2e-observer", usageFixtureObserver(func(sdkusage.Record) {}))
	})
	start := func(id, key string, failed bool, input, output, cached int64) (*Server, string) {
		t.Helper()
		dir := t.TempDir()
		path := filepath.Join(dir, "config.yaml")
		if err := os.WriteFile(path, []byte("port: 8317\n"), 0600); err != nil {
			t.Fatal(err)
		}
		cfg := &config.Config{SDKConfig: sdkconfig.SDKConfig{APIKeys: []string{key}}, AuthDir: dir, UsageStatisticsEnabled: true}
		cfg.RemoteManagement.DisableControlPanel = true
		manager := auth.NewManager(nil, nil, nil)
		server := NewServer(cfg, manager, sdkaccess.NewManager(), path, WithIsolatedUsage())
		record := sdkusage.Record{RequestID: "synthetic-runtime-event-" + id, Provider: "usage-fixture", APIKey: key,
			AuthID: "ordinary-fixture-" + id, Source: upstream, Failed: failed, RequestedAt: time.Now(),
			ResponseHeaders: http.Header{"Authorization": []string{upstream}, "X-Fixture": []string{header}},
			Detail:          sdkusage.Detail{InputTokens: input, OutputTokens: output, CachedTokens: cached, CacheReadTokens: cached, TotalTokens: input + output}}
		manager.RegisterExecutor(&usageFixtureExecutor{record: record})
		credential := &auth.Auth{ID: record.AuthID, Provider: "usage-fixture", Status: auth.StatusActive, Metadata: map[string]any{"access_token": upstream}}
		if _, err := manager.Register(context.Background(), credential); err != nil {
			t.Fatal(err)
		}
		reg := registry.GetGlobalRegistry()
		reg.RegisterClient(credential.ID, credential.Provider, []*registry.ModelInfo{{ID: runtimeFixtureModel}})
		t.Cleanup(func() { reg.UnregisterClient(credential.ID) })
		t.Cleanup(func() { _ = server.Stop(context.Background()) })
		return server, path
	}
	a, aPath := start("a", inA, false, 12, 5, 3)
	b, bPath := start("b", inB, true, 21, 0, 7)
	chat := func(s *Server, key string, success bool) {
		t.Helper()
		body := `{"model":"` + runtimeFixtureModel + `","messages":[{"role":"user","content":"` + raw + `"}]}`
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Fixture", header)
		res := httptest.NewRecorder()
		s.Handler().ServeHTTP(res, req)
		if (res.Code == 200) != success || (!success && res.Code < 500) {
			t.Fatalf("unexpected chat status %d", res.Code)
		}
	}
	scan := func(data []byte) {
		t.Helper()
		for _, secret := range []string{inA, inB, admin, upstream, header, raw, detached} {
			if bytes.Contains(data, []byte(secret)) {
				t.Error("confidential fixture marker escaped into usage")
			}
		}
	}
	read := func(s *Server) usage.StatisticsSnapshot {
		t.Helper()
		req := httptest.NewRequest("GET", "/v0/management/usage", nil)
		req.RemoteAddr = "127.0.0.1:4321"
		req.Header.Set("X-Management-Key", admin)
		res := httptest.NewRecorder()
		s.Handler().ServeHTTP(res, req)
		if res.Code != 200 {
			t.Fatalf("management usage status %d", res.Code)
		}
		scan(res.Body.Bytes())
		var result struct{ Usage usage.StatisticsSnapshot }
		if err := json.Unmarshal(res.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result.Usage
	}
	awaitBus := func(n int) {
		t.Helper()
		timer := time.NewTimer(3 * time.Second)
		defer timer.Stop()
		for range n {
			select {
			case <-delivered:
			case <-timer.C:
				t.Fatal("real SDK observer did not receive published event")
			}
		}
	}
	chat(a, inA, true)
	chat(b, inB, false)
	sdkusage.PublishRecord(context.Background(), sdkusage.Record{RequestID: "synthetic-runtime-unowned", Provider: "usage-fixture", Model: runtimeFixtureModel, APIKey: detached, Detail: sdkusage.Detail{TotalTokens: 99}})
	awaitBus(3)
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		if read(a).TotalRequests != 0 && read(b).TotalRequests != 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	assert := func(s *Server, success, failed, total, cache int64) {
		t.Helper()
		snapshot := read(s)
		var gotCache int64
		var detailCount int
		for _, api := range snapshot.APIs {
			for model, v := range api.Models {
				if model != runtimeFixtureModel {
					t.Errorf("unexpected model %q", model)
				}
				for _, d := range v.Details {
					gotCache += d.Tokens.CacheReadTokens
					detailCount++
				}
			}
		}
		if snapshot.TotalRequests != 1 || detailCount != 1 || snapshot.SuccessCount != success || snapshot.FailureCount != failed || snapshot.TotalTokens != total || gotCache != cache {
			t.Errorf("actual /v1 SDK-to-instance totals: requests=%d success=%d failure=%d tokens=%d cache=%d details=%d", snapshot.TotalRequests, snapshot.SuccessCount, snapshot.FailureCount, snapshot.TotalTokens, gotCache, detailCount)
		}
	}
	assert(a, 1, 0, 17, 3)
	assert(b, 0, 1, 21, 7)
	for _, s := range []*Server{a, b} {
		next := s.getConfig().CloneForRuntime()
		next.UsageStatisticsEnabled = false
		s.UpdateClients(next)
	}
	chat(a, inA, true)
	awaitBus(1)
	assert(a, 1, 0, 17, 3)
	assert(b, 0, 1, 21, 7)
	for s, p := range map[*Server]string{a: aPath, b: bPath} {
		if err := s.Stop(context.Background()); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(filepath.Dir(p), ".usage-statistics.json"))
		if err != nil {
			t.Fatal(err)
		}
		scan(data)
		var persisted struct{ Usage usage.StatisticsSnapshot }
		if err := json.Unmarshal(data, &persisted); err != nil || persisted.Usage.TotalRequests != 1 {
			t.Errorf("persisted request count=%d err=%v", persisted.Usage.TotalRequests, err)
		}
	}
}
