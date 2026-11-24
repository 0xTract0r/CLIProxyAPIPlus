package management

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	sdkauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type manualActionRoundTripper func(*http.Request) (*http.Response, error)

func (f manualActionRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func manualActionFixture(t *testing.T) (*Handler, *coreauth.Manager, string) {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"explicit-public.json": `{"type":"openai-compatibility","base_url":"https://public.example.invalid/v1","api_key":"fake-explicit-public-key","enabled":true}`,
		"other-public.json":    `{"type":"openai-compatibility","base_url":"https://other.example.invalid/v1","api_key":"fake-different-key"}`,
		"selected-oauth.json":  `{"type":"claude","access_token":"fake-oauth-credential","quota_snapshot":{"usage":{"five_hour":{"utilization":27,"private":"fake-upstream-internal"}}}}`,
		"hidden-sibling.json":  `{"type":"claude","access_token":"fake-sibling-token","quota_snapshot":{"usage":{"five_hour":{"utilization":43}}}}`,
	}
	for name, value := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	store := sdkauth.NewFileTokenStore()
	store.SetBaseDir(dir)
	manager := coreauth.NewManager(store, nil, nil)
	if err := manager.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: dir}, manager)
	h.tokenStore = store
	return h, manager, dir
}

func callManualAction(router *gin.Engine, endpoint, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, endpoint, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	result := httptest.NewRecorder()
	router.ServeHTTP(result, req)
	return result
}

func TestOrdinaryManualTestMessageUsesSingleBoundCredential(t *testing.T) {
	h, _, _ := manualActionFixture(t)
	const selected = "explicit-public.json"
	models := registry.GetGlobalRegistry()
	models.RegisterClient(selected, "openai-compatibility", []*registry.ModelInfo{{ID: "local-fixture-model"}})
	t.Cleanup(func() { models.UnregisterClient(selected) })
	var status int
	calls := 0
	transport := manualActionRoundTripper(func(request *http.Request) (*http.Response, error) {
		calls++
		if request.URL.String() != "https://public.example.invalid/v1/chat/completions" ||
			request.Method != http.MethodPost || request.Header.Get("Authorization") != "Bearer fake-explicit-public-key" {
			t.Fatal("request escaped the selected credential's configured protocol and origin")
		}
		var payload struct {
			Model     string `json:"model"`
			MaxTokens int    `json:"max_tokens"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil ||
			payload.Model != "local-fixture-model" || payload.MaxTokens != 11 {
			t.Fatal("request did not use the explicitly selected model and token limit")
		}
		headers := make(http.Header)
		if status == http.StatusTemporaryRedirect {
			headers.Set("Location", "https://different.example.invalid/redirect")
		}
		if status == http.StatusTooManyRequests {
			headers.Set("Retry-After", "90")
		}
		return &http.Response{StatusCode: status, Header: headers, Body: io.NopCloser(strings.NewReader(
			`{"choices":[{"message":{"content":"hello fake-explicit-public-key"}}]}`))}, nil
	})
	router := gin.New()
	const endpoint = "/auth-files/test-message"
	router.POST(endpoint, func(c *gin.Context) { h.testOrdinaryAuthFileMessage(c, transport) })

	for body, wantCode := range map[string]int{
		`{"name":"explicit-public.json","model":"other-model"}`:                          400,
		`{"name":"selected-oauth.json","model":"local-fixture-model"}`:                   501,
		`{"name":"explicit-public.json","model":"local-fixture-model","all":true}`:       400,
		`{"name":"explicit-public.json","model":"local-fixture-model","max_tokens":257}`: 400,
	} {
		response := callManualAction(router, endpoint, body)
		if response.Code != wantCode || calls != 0 {
			t.Fatalf("unexpected rejected input: got=%d want=%d calls=%d", response.Code, wantCode, calls)
		}
	}
	body := `{"name":"explicit-public.json","model":"local-fixture-model","message":"Hi","max_tokens":11}`
	for i, testCase := range []struct {
		upstream, downstream int
	}{
		{200, 200}, {307, 502}, {401, 401}, {429, 429},
	} {
		status = testCase.upstream
		out := callManualAction(router, endpoint, body)
		if out.Code != testCase.downstream || calls != i+1 || out.Header().Get("Location") != "" ||
			strings.Contains(out.Body.String(), "fake-explicit-public-key") {
			t.Fatalf("upstream=%d downstream=%d calls=%d", status, out.Code, calls)
		}
		if status == 200 && (!strings.Contains(out.Body.String(), `"selected_auth_id":"explicit-public.json"`) ||
			!strings.Contains(out.Body.String(), `"output_preview":"hello`)) {
			t.Fatal("missing CPAMP success fields")
		}
		if status == 429 && out.Header().Get("Retry-After") != "90" {
			t.Fatal("provider wait was not returned")
		}
	}
	out := callManualAction(router, endpoint, body)
	if out.Code != http.StatusTooManyRequests || calls != 4 {
		t.Fatalf("repeated send before provider retry window: code=%d calls=%d", out.Code, calls)
	}
}

func TestOrdinaryManualQuotaRequiresOneSelectedCredential(t *testing.T) {
	h, manager, dir := manualActionFixture(t)
	router := gin.New()
	const endpoint = "/quota/refresh"
	router.POST(endpoint, h.RefreshOrdinaryQuota)
	for body, want := range map[string]int{
		`{}`:                    400,
		`{"provider":"claude"}`: 400,
		`{"name":"selected-oauth.json","provider":"codex"}`:              409,
		`{"name":"selected-oauth.json","auth_id":"hidden-sibling.json"}`: 404,
		`{"name":"../selected-oauth.json"}`:                              400,
	} {
		out := callManualAction(router, endpoint, body)
		if out.Code != want {
			t.Fatalf("got %d want %d for %s", out.Code, want, body)
		}
	}
	path := filepath.Join(dir, "selected-oauth.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out := callManualAction(router, endpoint, `{"auth_id":"selected-oauth.json","provider":"claude"}`)
	if out.Code != http.StatusNotImplemented || !strings.Contains(out.Body.String(), `"supported":false`) ||
		!strings.Contains(out.Body.String(), `"utilization":27`) ||
		strings.Contains(out.Body.String(), "fake-") || strings.Contains(out.Body.String(), "hidden-sibling") {
		t.Fatalf("unsupported response incorrectly exposed or replaced a snapshot: %d %s", out.Code, out.Body)
	}
	after, err := os.ReadFile(path)
	current, _ := manager.GetByID("selected-oauth.json")
	if err != nil || string(after) != string(before) || current.Metadata["quota_refresh_status"] != nil {
		t.Fatal("unsupported refresh changed the persisted or loaded credential")
	}
}
