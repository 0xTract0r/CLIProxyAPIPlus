package management

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/MoeForge/moego/json"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestGetInFlightRequestsReturnsSnapshot(t *testing.T) {
	gin.SetMode(gin.TestMode)
	manager := coreauth.NewManager(nil, nil, nil)
	h := &Handler{authManager: manager}

	rec := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(rec)
	ginCtx.Request = httptest.NewRequest(http.MethodGet, "/v8/management/observability/requests/in-flight", nil)

	h.GetInFlightRequests(ginCtx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var payload struct {
		InFlight        int64            `json:"in_flight"`
		OldestStartedAt string           `json:"oldest_started_at"`
		ByProvider      map[string]int64 `json:"by_provider"`
	}
	if errUnmarshal := json.Unmarshal(rec.Body.Bytes(), &payload); errUnmarshal != nil {
		t.Fatalf("unmarshal response: %v", errUnmarshal)
	}
	if payload.InFlight != 0 {
		t.Fatalf("in_flight = %d, want 0", payload.InFlight)
	}
}

func TestGetInFlightRequestsNilManager(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &Handler{}

	rec := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(rec)
	ginCtx.Request = httptest.NewRequest(http.MethodGet, "/v8/management/observability/requests/in-flight", nil)

	h.GetInFlightRequests(ginCtx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var payload struct {
		InFlight int64 `json:"in_flight"`
	}
	if errUnmarshal := json.Unmarshal(rec.Body.Bytes(), &payload); errUnmarshal != nil {
		t.Fatalf("unmarshal response: %v", errUnmarshal)
	}
	if payload.InFlight != 0 {
		t.Fatalf("in_flight = %d, want 0", payload.InFlight)
	}
}

type inFlightManagementExecutor struct {
	coreauth.ProviderExecutor
	entered chan struct{}
}

func (*inFlightManagementExecutor) Identifier() string { return "in-flight-management-fixture" }

func (e *inFlightManagementExecutor) Execute(ctx context.Context, _ *coreauth.Auth, _ executor.Request, _ executor.Options) (executor.Response, error) {
	close(e.entered)
	<-ctx.Done()
	return executor.Response{}, ctx.Err()
}

func TestGetInFlightRequestsDuringManagerReplacement(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const (
		path    = "/v8/management/observability/requests/in-flight"
		grantID = "in-flight-management-fixture-grant"
		model   = "in-flight-management-fixture-model"
	)
	manager := coreauth.NewManager(nil, nil, nil)
	exec := &inFlightManagementExecutor{entered: make(chan struct{})}
	manager.RegisterExecutor(exec)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{ID: grantID, Provider: exec.Identifier(), Status: coreauth.StatusActive}); err != nil {
		t.Fatalf("register fixture: %v", err)
	}
	models := registry.GetGlobalRegistry()
	models.RegisterClient(grantID, exec.Identifier(), []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { models.UnregisterClient(grantID) })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	executionDone := make(chan struct{})
	go func() {
		defer close(executionDone)
		_, _ = manager.Execute(ctx, []string{exec.Identifier()}, executor.Request{Model: model}, executor.Options{})
	}()
	t.Cleanup(func() {
		cancel()
		<-executionDone
	})
	select {
	case <-exec.entered:
	case <-executionDone:
		t.Fatal("the fixture never entered its executor")
	case <-ctx.Done():
		t.Fatal("timed out waiting for the fixture to enter its executor")
	}

	h := &Handler{}
	router := gin.New()
	router.GET(path, h.GetInFlightRequests)
	check := func(want int64) {
		t.Helper()
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		var got inFlightResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &got); rec.Code != http.StatusOK || err != nil {
			t.Fatalf("in-flight response status=%d error=%v", rec.Code, err)
		}
		if got.ByProvider == nil || (want >= 0 && got.InFlight != want) {
			t.Fatalf("in-flight response = %+v, expected count %d", got, want)
		}
		switch got.InFlight {
		case 0:
			if len(got.ByProvider) != 0 || got.OldestStartedAt != nil {
				t.Fatalf("empty snapshot has stale fields: %+v", got)
			}
		case 1:
			if len(got.ByProvider) != 1 || got.ByProvider[exec.Identifier()] != 1 || got.OldestStartedAt == nil {
				t.Fatalf("active snapshot is incomplete: %+v", got)
			}
		default:
			t.Fatalf("unexpected in-flight response: %+v", got)
		}
	}
	check(0)
	h.SetAuthManager(manager)
	check(1)

	start, replaced := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(replaced)
		<-start
		for range 32 {
			h.SetAuthManager(nil)
			h.SetAuthManager(manager)
		}
		h.SetAuthManager(nil)
	}()
	close(start)
	defer func() { <-replaced }()
	for range 32 {
		check(-1)
	}
	<-replaced
	check(0)
}
