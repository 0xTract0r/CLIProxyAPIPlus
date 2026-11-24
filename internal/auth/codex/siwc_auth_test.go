package codex

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/MoeForge/moego/json"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/misc"
)

func TestParseOAuthCallbackClientID(t *testing.T) {
	cbURL := "http://127.0.0.1:1455/auth/callback?code=auth-code-123&state=state-abc&client_id=oaiapp_issued_99"
	parsed, err := misc.ParseOAuthCallback(cbURL)
	if err != nil {
		t.Fatalf("ParseOAuthCallback error: %v", err)
	}
	if parsed == nil {
		t.Fatal("expected non-nil callback")
	}
	if parsed.Code != "auth-code-123" || parsed.State != "state-abc" || parsed.ClientID != "oaiapp_issued_99" {
		t.Fatalf("parsed = %+v, want code=auth-code-123 state=state-abc client_id=oaiapp_issued_99", parsed)
	}

	srv := NewOAuthServer(0)
	req := httptest.NewRequest(http.MethodGet, "/auth/callback?code=c1&state=s1&client_id=oaiapp_srv_1", nil)
	rec := httptest.NewRecorder()
	srv.handleCallback(rec, req)
	res, errWait := srv.WaitForCallback(time.Second)
	if errWait != nil {
		t.Fatalf("WaitForCallback error: %v", errWait)
	}
	if res.Code != "c1" || res.State != "s1" || res.ClientID != "oaiapp_srv_1" {
		t.Fatalf("OAuthServer result = %+v", res)
	}
}

func TestSaveTokenToFileDoesNotOverwriteNewerDiskSIWC(t *testing.T) {
	tmpDir := t.TempDir()
	authPath := filepath.Join(tmpDir, "codex-newer-disk.json")

	// Disk has a newly bound SIWC credential (last_refresh 12:00:00Z)
	diskJSON := `{"type":"codex","access_token":"outer-old","refresh_token":"outer-ref-old","last_refresh":"2026-10-12T10:00:00Z","use_siwc":true,"siwc":{"access_token":"siwc-newer-on-disk","refresh_token":"siwc-ref-newer","client_id":"oaiapp_new","device_id":"e61bbe28-07ef-466d-8e5d-a344f94ab305","last_refresh":"2026-10-12T12:00:00Z","expired":"2026-10-12T12:57:00Z"}}`
	if err := os.WriteFile(authPath, []byte(diskJSON), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Server process finishes refreshing outer token (last_refresh 12:01:00Z), but its in-memory SIWC snapshot is stale (last_refresh 10:00:00Z)
	useSIWCFalse := false
	staleSnapshot := &CodexTokenStorage{
		AccessToken:  "outer-refreshed",
		RefreshToken: "outer-ref-refreshed",
		LastRefresh:  "2026-10-12T12:01:00Z",
		Expire:       "2026-10-20T12:01:00Z",
		Type:         "codex",
		UseSIWC:      &useSIWCFalse,
		SIWC: map[string]any{
			"access_token":  "siwc-stale-in-memory",
			"refresh_token": "siwc-ref-stale",
			"client_id":     "oaiapp_old",
			"last_refresh":  "2026-10-12T10:00:00Z",
			"expired":       "2026-10-12T10:57:00Z",
		},
	}
	var before map[string]any
	if errUnmarshal := json.Unmarshal([]byte(diskJSON), &before); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	staleSnapshot.RefreshPatch = NewCredentialRefreshPatch(before, tokenStorageTestMap(t, staleSnapshot), true, false)
	if err := staleSnapshot.SaveTokenToFile(authPath); err != nil {
		t.Fatalf("SaveTokenToFile: %v", err)
	}

	raw, errRead := os.ReadFile(authPath)
	if errRead != nil {
		t.Fatalf("ReadFile: %v", errRead)
	}
	var saved map[string]any
	if errUnmarshal := json.Unmarshal(raw, &saved); errUnmarshal != nil {
		t.Fatalf("Unmarshal: %v", errUnmarshal)
	}
	if saved["access_token"] != "outer-refreshed" {
		t.Fatalf("outer access_token = %v, want outer-refreshed", saved["access_token"])
	}
	siwcMap, _ := saved["siwc"].(map[string]any)
	if siwcMap["access_token"] != "siwc-newer-on-disk" || siwcMap["client_id"] != "oaiapp_new" {
		t.Fatalf("newer on-disk SIWC was overwritten by stale in-memory snapshot: %#v", siwcMap)
	}
	if saved["use_siwc"] != true {
		t.Fatalf("use_siwc from newer on-disk SIWC bind should remain true, got %v", saved["use_siwc"])
	}
}

func tokenStorageTestMap(t *testing.T, storage *CodexTokenStorage) map[string]any {
	t.Helper()
	raw, errMarshal := json.Marshal(storage)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	var metadata map[string]any
	if errUnmarshal := json.Unmarshal(raw, &metadata); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	return metadata
}
