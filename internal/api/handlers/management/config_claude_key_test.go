package management

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/MoeForge/moego/json"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// A typo must fail the write instead of reaching the request path, where it can
// only be reported as a warning behind every later request.
func TestPatchClaudeKeyRejectsUnknownFingerprintProfile(t *testing.T) {
	cfg := &config.Config{
		ClaudeKey: []config.ClaudeKey{
			{APIKey: "test-claude-key", FingerprintProfile: "claude-code-cli"},
		},
	}
	h := &Handler{cfg: cfg, configFilePath: writeTestConfigFile(t)}

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodPatch, "/v0/management/claude-api-key",
		strings.NewReader(`{"index":0,"value":{"fingerprint-profile":"claude-code"}}`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	h.PatchClaudeKey(ctx)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "fingerprint-profile") {
		t.Fatalf("error body = %s, want it to name the field", rec.Body.String())
	}
	if got := cfg.ClaudeKey[0].FingerprintProfile; got != "claude-code-cli" {
		t.Fatalf("FingerprintProfile = %q, want the rejected patch to leave it unchanged", got)
	}
}

func TestPutClaudeKeysRejectsUnknownFingerprintProfile(t *testing.T) {
	cfg := &config.Config{}
	h := &Handler{cfg: cfg, configFilePath: writeTestConfigFile(t)}

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodPut, "/v0/management/claude-api-key",
		strings.NewReader(`[{"api-key":"k1"},{"api-key":"k2","fingerprint-profile":"claude-cli"}]`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	h.PutClaudeKeys(ctx)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "claude-api-key[1].fingerprint-profile") {
		t.Fatalf("error body = %s, want the offending index", rec.Body.String())
	}
	if len(cfg.ClaudeKey) != 0 {
		t.Fatalf("ClaudeKey = %+v, want the rejected write to change nothing", cfg.ClaudeKey)
	}
}

func TestPatchClaudeKeyPriority(t *testing.T) {
	configFile := writeTestConfigFile(t)
	cfg := &config.Config{
		ClaudeKey: []config.ClaudeKey{
			{APIKey: "key-0", Priority: 0},
			{APIKey: "key-1", Priority: 5},
		},
	}
	h := &Handler{cfg: cfg, configFilePath: configFile}

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodPatch, "/v0/management/claude-api-key",
		strings.NewReader(`{"index":1,"value":{"priority":20}}`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	h.PatchClaudeKey(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if cfg.ClaudeKey[0].APIKey != "key-0" || cfg.ClaudeKey[0].Priority != 0 || cfg.ClaudeKey[1].APIKey != "key-1" {
		t.Fatal("priority patch modified another credential or replaced a key")
	}
	if got := cfg.ClaudeKey[1].Priority; got != 20 {
		t.Fatalf("ClaudeKey[1].Priority = %d, want 20", got)
	}

	savedBytes, errRead := os.ReadFile(configFile)
	if errRead != nil {
		t.Fatalf("os.ReadFile() error = %v", errRead)
	}
	savedText := string(savedBytes)
	if !strings.Contains(savedText, "priority: 20") {
		t.Fatalf("saved YAML missing 'priority: 20':\n%s", savedText)
	}

	// Verify GET returns the updated priority with structured unmarshaling
	getRec := httptest.NewRecorder()
	getCtx, _ := gin.CreateTestContext(getRec)
	getCtx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/claude-api-key", nil)
	h.GetClaudeKeys(getCtx)

	if getRec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200; body=%s", getRec.Code, getRec.Body.String())
	}
	var resp struct {
		ClaudeKey []config.ClaudeKey `json:"claude-api-key"`
	}
	if errJSON := json.Unmarshal(getRec.Body.Bytes(), &resp); errJSON != nil {
		t.Fatalf("unmarshal GET response: %v", errJSON)
	}
	if len(resp.ClaudeKey) != 2 {
		t.Fatalf("GET response ClaudeKey length = %d, want 2", len(resp.ClaudeKey))
	}
	if resp.ClaudeKey[1].Priority != 20 {
		t.Fatalf("GET response ClaudeKey[1].Priority = %d, want 20", resp.ClaudeKey[1].Priority)
	}

	// Omitting priority must preserve existing non-zero value (20)
	rec = httptest.NewRecorder()
	ctx, _ = gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodPatch, "/v0/management/claude-api-key",
		strings.NewReader(`{"index":1,"value":{"prefix":"team-test"}}`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	h.PatchClaudeKey(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := cfg.ClaudeKey[1].Priority; got != 20 {
		t.Fatalf("ClaudeKey[1].Priority = %d, want preserved 20", got)
	}
	if got := cfg.ClaudeKey[1].Prefix; got != "team-test" {
		t.Fatalf("ClaudeKey[1].Prefix = %q, want %q", got, "team-test")
	}

	// Reset priority to 0 explicitly
	rec = httptest.NewRecorder()
	ctx, _ = gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodPatch, "/v0/management/claude-api-key",
		strings.NewReader(`{"index":1,"value":{"priority":0}}`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	h.PatchClaudeKey(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := cfg.ClaudeKey[1].Priority; got != 0 {
		t.Fatalf("ClaudeKey[1].Priority = %d, want 0", got)
	}

	// Invalid priority type returns 400
	rec = httptest.NewRecorder()
	ctx, _ = gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodPatch, "/v0/management/claude-api-key",
		strings.NewReader(`{"index":1,"value":{"priority":"invalid"}}`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	h.PatchClaudeKey(ctx)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}
