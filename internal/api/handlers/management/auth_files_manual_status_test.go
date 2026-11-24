package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codex"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	sdkauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestOrdinaryManualStatusChecksAndRecordsOneScopedCredential(t *testing.T) {
	dir := t.TempDir()
	foreign := t.TempDir()
	filename := "selected.json"
	raw := []byte(`{"type":"codex","email":"fixture@example.invalid","note":"unchanged","disabled":true,"access_token":"synthetic-private-token"}`)
	for _, dest := range []string{filepath.Join(dir, filename), filepath.Join(foreign, "foreign.json")} {
		if err := os.WriteFile(dest, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	store := sdkauth.NewFileTokenStore()
	store.SetBaseDir(dir)
	manager := coreauth.NewManager(store, nil, nil)
	if err := manager.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Register(coreauth.WithSkipPersist(context.Background()), &coreauth.Auth{
		ID: "foreign.json", FileName: "foreign.json", Provider: "codex",
		Attributes: map[string]string{coreauth.AttributePath: filepath.Join(foreign, "foreign.json")},
		Metadata:   map[string]any{"type": "codex"},
	}); err != nil {
		t.Fatal(err)
	}
	handler := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: dir}, manager)
	handler.tokenStore = store
	router := gin.New()
	router.POST("/refresh-status", handler.RefreshOrdinaryAuthFileStatus)
	router.GET("/history", handler.ListAuthStatusHistory)

	call := func(method, endpoint, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, endpoint, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		out := httptest.NewRecorder()
		router.ServeHTTP(out, req)
		return out
	}

	for body, code := range map[string]int{
		`{"name":"../foreign.json"}`:                 400,
		`{"name":"selected.json","trigger":"auto"}`:  400,
		`{"name":"foreign.json","trigger":"manual"}`: 404,
	} {
		if result := call(http.MethodPost, "/refresh-status", body); result.Code != code {
			t.Fatalf("status check %s got %d, want %d", body, result.Code, code)
		}
	}

	result := call(http.MethodPost, "/refresh-status", `{"name":"selected.json","trigger":"manual"}`)
	if result.Code != 200 || !strings.Contains(result.Body.String(), `"disabled":true`) ||
		strings.Contains(result.Body.String(), "synthetic-private-token") {
		t.Fatalf("unexpected status response: %d %s", result.Code, result.Body.String())
	}
	readback := call(http.MethodGet, "/history?auth_name=selected.json", "")
	var payload authStatusHistoryListResponse
	if err := json.Unmarshal(readback.Body.Bytes(), &payload); readback.Code != 200 || err != nil ||
		len(payload.Events) != 1 || payload.Events[0].AuthName != filename || payload.Events[0].Trigger != "manual" {
		t.Fatalf("expected one scoped manual history entry: %d %s (%v)", readback.Code, readback.Body.String(), err)
	}
	actual, err := os.ReadFile(filepath.Join(dir, filename))
	if err != nil || string(actual) != string(raw) {
		t.Fatalf("read-only status query changed credential bytes: %v", err)
	}
}

type manualStatusTestExecutor struct {
	coreauth.ProviderExecutor
	refresh func(*coreauth.Auth) (*coreauth.Auth, error)
}

func (*manualStatusTestExecutor) Identifier() string { return "codex" }

func (e *manualStatusTestExecutor) Refresh(_ context.Context, a *coreauth.Auth) (*coreauth.Auth, error) {
	return e.refresh(a)
}

type manualStatusWaitError struct{}

func (manualStatusWaitError) Error() string { return "private-fixture-upstream-detail" }

func (manualStatusWaitError) StatusCode() int { return http.StatusTooManyRequests }

func (manualStatusWaitError) Headers() http.Header {
	return http.Header{"Retry-After": {"3600"}}
}

func TestOrdinaryManualStatusRefreshesOneGrantOrReportsWait(t *testing.T) {
	for _, scenario := range []string{"clock-rollback", "grant-replaced", "provider-wait"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			const name = "selected.json"
			old := time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339)
			data, err := json.Marshal(map[string]any{
				"type": "codex", "note": "unchanged", "refresh_token": "fake-previous-grant",
				"access_token": "fake-previous-access", "expired": old, "last_refresh": old,
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
				t.Fatal(err)
			}
			store := sdkauth.NewFileTokenStore()
			store.SetBaseDir(dir)
			manager := coreauth.NewManager(store, nil, nil)
			if err := manager.Load(ctx); err != nil {
				t.Fatal(err)
			}
			calls := 0
			manager.RegisterExecutor(&manualStatusTestExecutor{refresh: func(a *coreauth.Auth) (*coreauth.Auth, error) {
				calls++
				if a.ID != name || a.Metadata["refresh_token"] != "fake-previous-grant" {
					t.Fatalf("provider got a different credential")
				}
				switch scenario {
				case "grant-replaced":
					replacement, err := json.Marshal(map[string]any{
						"type": "codex", "note": "unchanged", "refresh_token": "fake-replacement",
						"access_token": "fake-replacement-access", "expired": old,
					})
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(dir, name), replacement, 0600); err != nil {
						t.Fatal(err)
					}
					if err := manager.Load(ctx); err != nil {
						t.Fatal(err)
					}
				case "provider-wait":
					return nil, manualStatusWaitError{}
				}
				baseline := a.Clone().Metadata
				a.Metadata["refresh_token"] = "fake-rotated"
				a.Metadata["access_token"] = "fake-rotated-access"
				a.Metadata["last_refresh"] = time.Now().UTC().Format(time.RFC3339)
				a.CodexRefreshPatch = codex.NewCredentialRefreshPatchWithDiskBaseline(baseline, baseline, a.Metadata, true, false)
				a.CodexRefreshPatch.SetUpstreamRefreshed(true)
				return a, nil
			}})
			h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: dir}, manager)
			h.tokenStore = store
			router := gin.New()
			router.POST("/auth-files/refresh-status", h.RefreshOrdinaryAuthFileStatus)
			router.GET("/auth-files/history", h.ListAuthStatusHistory)
			call := func(method, path, body string) *httptest.ResponseRecorder {
				t.Helper()
				req := httptest.NewRequest(method, path, strings.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				out := httptest.NewRecorder()
				router.ServeHTTP(out, req)
				return out
			}
			check := func() *httptest.ResponseRecorder {
				return call(http.MethodPost, "/auth-files/refresh-status", `{"name":"selected.json","trigger":"manual"}`)
			}
			response := check()
			expectedGrant := "fake-rotated"
			expectedStatus := http.StatusOK
			switch scenario {
			case "clock-rollback":
				if !strings.Contains(response.Body.String(), `"refreshed":true`) ||
					!strings.Contains(response.Body.String(), `"file":`) ||
					strings.Contains(response.Body.String(), old) {
					t.Fatalf("success did not return the refreshed file status: %s", response.Body.String())
				}
			case "grant-replaced":
				expectedGrant = "fake-replacement"
				expectedStatus = http.StatusConflict
			case "provider-wait":
				expectedGrant = "fake-previous-grant"
				expectedStatus = http.StatusTooManyRequests
				if response.Header().Get("Retry-After") != "3600" {
					t.Fatalf("actual Retry-After was lost: %v", response.Header())
				}
				blockedAgain := check()
				if next, err := http.ParseTime(blockedAgain.Header().Get("Retry-After")); blockedAgain.Code != 429 || err != nil || time.Until(next) < 50*time.Minute {
					t.Fatalf("earlier wait not honored: %d, %v", blockedAgain.Code, blockedAgain.Header())
				}
			}
			body := response.Body.String()
			if response.Code != expectedStatus || calls != 1 || strings.Contains(body, "fake-") || strings.Contains(body, "private-fixture") {
				t.Fatalf("unexpected outcome: status=%d calls=%d body=%s", response.Code, calls, body)
			}
			reopened := coreauth.NewManager(store, nil, nil)
			if err := reopened.Load(ctx); err != nil {
				t.Fatal(err)
			}
			refreshed, ok := reopened.GetByID(name)
			if !ok || refreshed.Metadata["refresh_token"] != expectedGrant || refreshed.Metadata["note"] != "unchanged" {
				t.Fatal("cold read did not preserve the correct grant and other fields")
			}
			history := call(http.MethodGet, "/auth-files/history?auth_name="+name, "")
			if history.Code != 200 || strings.Contains(history.Body.String(), "private-fixture") {
				t.Fatalf("could not read private-data-safe manual history: %d", history.Code)
			}
			if scenario == "clock-rollback" && !strings.Contains(history.Body.String(), `"trigger":"manual"`) {
				t.Fatal("successful manual refresh missing from history")
			}
		})
	}
}
