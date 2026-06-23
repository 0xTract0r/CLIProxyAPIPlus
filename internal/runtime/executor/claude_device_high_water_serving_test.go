package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

// servingHighWaterStore is a minimal auth.Store that records Save calls and the
// last persisted auth snapshot, so the serving-path tests can assert both that a
// disk write happened and that the persisted metadata carries the high-water.
type servingHighWaterStore struct {
	mu        sync.Mutex
	saveCount atomic.Int32
	lastSaved *cliproxyauth.Auth
}

func (s *servingHighWaterStore) List(context.Context) ([]*cliproxyauth.Auth, error) {
	return nil, nil
}

func (s *servingHighWaterStore) Save(_ context.Context, auth *cliproxyauth.Auth) (string, error) {
	s.saveCount.Add(1)
	s.mu.Lock()
	s.lastSaved = auth
	s.mu.Unlock()
	return "", nil
}

func (s *servingHighWaterStore) Delete(context.Context, string) error { return nil }

// newServingHighWaterFixture wires a Manager (with a capturing store) and a
// registered claude auth into a ClaudeExecutor whose upstream points at the
// supplied httptest server. The returned auth shares the registered ID so the
// executor's persistClaudeDeviceHighWater can resolve the manager-side record.
func newServingHighWaterFixture(t *testing.T, serverURL string) (*ClaudeExecutor, *cliproxyauth.Auth, *servingHighWaterStore, *cliproxyauth.Manager) {
	t.Helper()
	resetClaudeDeviceProfileCache()

	store := &servingHighWaterStore{}
	mgr := cliproxyauth.NewManager(store, nil, nil)

	const authID = "claude-serving-hw-1"
	registered := &cliproxyauth.Auth{
		ID:       authID,
		Provider: "claude",
		Metadata: map[string]any{"type": "claude"},
		Attributes: map[string]string{
			"api_key":  "key-serving-hw",
			"base_url": serverURL,
		},
	}
	if _, err := mgr.Register(context.Background(), registered); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	executor := NewClaudeExecutorWithManager(&config.Config{AuthDir: t.TempDir()}, mgr)
	// The auth passed into the serving call shares the registered ID; the
	// device-profile observation and the high-water lookup are both scoped by
	// this same auth, so persist resolves the manager-side record by ID.
	servingAuth := &cliproxyauth.Auth{
		ID:       authID,
		ProxyURL: "direct",
		Provider: "claude",
		Attributes: map[string]string{
			"api_key":  "key-serving-hw",
			"base_url": serverURL,
		},
	}
	return executor, servingAuth, store, mgr
}

// versionedInboundHeaders carries a real, version-bearing claude-cli User-Agent
// (above the frozen floor 2.1.63, below the sanity ceiling 4.0.0) so the device
// profile resolution records it as a first-party observation.
func versionedInboundHeaders(version string) http.Header {
	h := http.Header{}
	h.Set("User-Agent", "claude-cli/"+version+" (external, cli)")
	h.Set("X-Stainless-Package-Version", "0.80.0")
	h.Set("X-Stainless-Runtime-Version", "v24.6.0")
	h.Set("X-Stainless-Os", "MacOS")
	h.Set("X-Stainless-Arch", "arm64")
	return h
}

func assertServingHighWaterPersisted(t *testing.T, mgr *cliproxyauth.Manager, store *servingHighWaterStore, authID, wantVersion string) {
	t.Helper()

	stored, ok := mgr.GetByID(authID)
	if !ok {
		t.Fatalf("auth %q not found after serving request", authID)
	}
	hw, ok := cliproxyauth.ClaudeDeviceHighWaterFromMetadata(stored.Metadata)
	if !ok {
		t.Fatalf("claude_device_high_water not written to auth.Metadata after serving request: metadata=%#v", stored.Metadata)
	}
	if hw.Version != wantVersion {
		t.Fatalf("persisted high-water version = %q, want %q (serving path must record the real observed version)", hw.Version, wantVersion)
	}
	if store.saveCount.Load() == 0 {
		t.Fatalf("expected at least one Save (disk persist) after serving request, got 0")
	}
	saved := func() *cliproxyauth.Auth {
		store.mu.Lock()
		defer store.mu.Unlock()
		return store.lastSaved
	}()
	if saved == nil {
		t.Fatalf("store captured no saved auth after serving request")
	}
	savedHW, ok := cliproxyauth.ClaudeDeviceHighWaterFromMetadata(saved.Metadata)
	if !ok || savedHW.Version != wantVersion {
		t.Fatalf("persisted snapshot high-water mismatch: ok=%v version=%q want=%q", ok, savedHW.Version, wantVersion)
	}
}

// TestClaudeExecutor_Execute_PersistsDeviceHighWaterFromServingPath is the
// regression guard for the PR #42 defect: the high-water write-back was only
// wired into PrepareRequest (the HttpRequest adapter bypass), so real /v1/messages
// serving via Execute never triggered persistence and the observed version was
// lost on restart. This test drives the real Execute serving flow with a
// version-bearing inbound UA and asserts claude_device_high_water lands in
// auth.Metadata (i.e. the write-back fires from the serving path, not only from
// PrepareRequest).
func TestClaudeExecutor_Execute_PersistsDeviceHighWaterFromServingPath(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","model":"claude-3-5-sonnet","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer server.Close()

	executor, auth, store, mgr := newServingHighWaterFixture(t, server.URL)

	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)
	if _, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-3-5-sonnet",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("claude"),
		Headers:      versionedInboundHeaders("2.5.0"),
	}); err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	assertServingHighWaterPersisted(t, mgr, store, auth.ID, "2.5.0")
}

// TestClaudeExecutor_ExecuteStream_PersistsDeviceHighWaterFromServingPath drives
// the streaming serving flow (the main conversation stream path) and asserts the
// same serving-path write-back.
func TestClaudeExecutor_ExecuteStream_PersistsDeviceHighWaterFromServingPath(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"model\":\"claude-3-5-sonnet\"}}\n\n"))
		_, _ = w.Write([]byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":1}}\n\n"))
		_, _ = w.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer server.Close()

	executor, auth, store, mgr := newServingHighWaterFixture(t, server.URL)

	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)
	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-3-5-sonnet",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("claude"),
		Headers:      versionedInboundHeaders("2.6.0"),
	})
	if err != nil {
		t.Fatalf("ExecuteStream returned error: %v", err)
	}
	// Drain the stream so the goroutine completes; the persist already fired
	// synchronously before the upstream call, but draining keeps the test clean.
	if result != nil {
		for range result.Chunks {
		}
	}

	assertServingHighWaterPersisted(t, mgr, store, auth.ID, "2.6.0")
}

// TestClaudeExecutor_CountTokens_PersistsDeviceHighWaterFromServingPath drives the
// count_tokens serving flow, which also passes through applyClaudeHeaders and so
// must trigger the same monotonic high-water write-back.
func TestClaudeExecutor_CountTokens_PersistsDeviceHighWaterFromServingPath(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"input_tokens":42}`))
	}))
	defer server.Close()

	executor, auth, store, mgr := newServingHighWaterFixture(t, server.URL)

	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)
	if _, err := executor.CountTokens(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-3-5-sonnet",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("claude"),
		Headers:      versionedInboundHeaders("2.7.0"),
	}); err != nil {
		t.Fatalf("CountTokens returned error: %v", err)
	}

	assertServingHighWaterPersisted(t, mgr, store, auth.ID, "2.7.0")
}
