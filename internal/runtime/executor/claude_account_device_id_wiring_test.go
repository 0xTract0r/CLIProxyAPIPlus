package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

// fork(anticorr) F1 guard — per-account synthetic device_id on the real serving
// path.
//
// claude_executor.go applyCloaking (~L2509) always rewrites the device_id inside
// metadata.user_id with a per-account synthetic value derived by
// helps.InjectAccountDeviceID, so that distinct upstream accounts never share one
// real device_id (the anti-correlation core). This runs before the ShouldCloak
// gate so it also covers real claude-cli clients.
//
// The existing helps unit tests only exercise InjectAccountDeviceID in isolation;
// they cannot catch an upstream merge that keeps the helper but drops its call
// site in the executor. This test drives the real Execute serving flow and asserts
// the OUTBOUND wire body carries the synthetic device_id, not the inbound sentinel.
//
// Red condition: delete the `payload = helps.InjectAccountDeviceID(...)` call in
// applyCloaking (claude_executor.go ~L2509). The inbound sentinel device_id then
// survives to the wire and both assertions below fail.
//
// Level: executor-wiring.
func TestClaudeExecutor_Execute_RewritesDeviceIDOnServingPath(t *testing.T) {
	resetClaudeDeviceProfileCache()

	const sentinelDeviceID = "0000000000000000000000000000000000000000000000000000deadbeefcafe"

	var mu sync.Mutex
	var capturedBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		capturedBody = b
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","model":"claude-3-5-sonnet","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer server.Close()

	authDir := t.TempDir()
	store := &servingHighWaterStore{}
	mgr := cliproxyauth.NewManager(store, nil, nil)

	const authID = "claude-device-id-wiring-1"
	registered := &cliproxyauth.Auth{
		ID:       authID,
		Provider: "claude",
		Metadata: map[string]any{"type": "claude"},
		Attributes: map[string]string{
			"api_key":  "key-device-id-wiring",
			"base_url": server.URL,
		},
	}
	if _, err := mgr.Register(context.Background(), registered); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	executor := NewClaudeExecutorWithManager(&config.Config{AuthDir: authDir}, mgr)
	auth := &cliproxyauth.Auth{
		ID:       authID,
		ProxyURL: "direct",
		Provider: "claude",
		Attributes: map[string]string{
			"api_key":  "key-device-id-wiring",
			"base_url": server.URL,
		},
	}

	// Inbound payload carries a recognizable sentinel device_id inside the
	// metadata.user_id JSON string (the exact wire shape Claude Code sends).
	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],` +
		`"metadata":{"user_id":"{\"device_id\":\"` + sentinelDeviceID + `\",\"account_uuid\":\"\",\"session_id\":\"sess-1\"}"}}`)

	if _, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-3-5-sonnet",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("claude"),
		Headers:      versionedInboundHeaders("2.5.0"),
	}); err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	mu.Lock()
	body := capturedBody
	mu.Unlock()
	if len(body) == 0 {
		t.Fatal("upstream captured no request body")
	}

	// metadata.user_id is a JSON *string* whose content is JSON; parse the inner
	// device_id out of the outbound wire body.
	userIDStr := gjson.GetBytes(body, "metadata.user_id").String()
	if userIDStr == "" {
		t.Fatalf("outbound body has no metadata.user_id: %s", body)
	}
	outboundDeviceID := gjson.Get(userIDStr, "device_id").String()
	if outboundDeviceID == "" {
		t.Fatalf("outbound metadata.user_id has no device_id: %q", userIDStr)
	}

	if outboundDeviceID == sentinelDeviceID {
		t.Fatalf("device_id NOT rewritten on serving path: inbound sentinel %q reached the wire (InjectAccountDeviceID call site missing)", sentinelDeviceID)
	}

	// The rewrite must produce the per-account synthetic value, not just any
	// change (blanking / random would also differ from the sentinel).
	want := helps.SyntheticDeviceID(authDir, auth, "key-device-id-wiring")
	if outboundDeviceID != want {
		t.Fatalf("outbound device_id = %q, want per-account synthetic %q", outboundDeviceID, want)
	}
}
