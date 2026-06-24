package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	_ "github.com/router-for-me/CLIProxyAPI/v7/internal/translator"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

// codexServingHighWaterStore is a minimal auth.Store that records Save calls and
// the last persisted auth snapshot, so the codex serving-path tests can assert
// both that a disk write happened and that the persisted metadata carries the
// high-water.
type codexServingHighWaterStore struct {
	mu        sync.Mutex
	saveCount atomic.Int32
	lastSaved *cliproxyauth.Auth
}

func (s *codexServingHighWaterStore) List(context.Context) ([]*cliproxyauth.Auth, error) {
	return nil, nil
}

func (s *codexServingHighWaterStore) Save(_ context.Context, auth *cliproxyauth.Auth) (string, error) {
	s.saveCount.Add(1)
	s.mu.Lock()
	s.lastSaved = auth
	s.mu.Unlock()
	return "", nil
}

func (s *codexServingHighWaterStore) Delete(context.Context, string) error { return nil }

// newCodexServingHighWaterFixture wires a Manager (with a capturing store) and a
// registered codex auth into a CodexExecutor whose upstream points at the supplied
// httptest server. The returned auth shares the registered ID so the executor's
// persistCodexDeviceHighWater can resolve the manager-side record.
func newCodexServingHighWaterFixture(t *testing.T, serverURL string) (*CodexExecutor, *cliproxyauth.Auth, *codexServingHighWaterStore, *cliproxyauth.Manager) {
	t.Helper()
	helps.ResetCodexClientProfileCacheForTests()

	store := &codexServingHighWaterStore{}
	mgr := cliproxyauth.NewManager(store, nil, nil)

	const authID = "codex-serving-hw-1"
	registered := &cliproxyauth.Auth{
		ID:       authID,
		Provider: "codex",
		Metadata: map[string]any{"type": "codex"},
		Attributes: map[string]string{
			"api_key":  "key-serving-hw",
			"base_url": serverURL,
		},
	}
	if _, err := mgr.Register(context.Background(), registered); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	executor := NewCodexExecutorWithManager(&config.Config{AuthDir: t.TempDir()}, mgr)
	servingAuth := &cliproxyauth.Auth{
		ID:       authID,
		ProxyURL: "direct",
		Provider: "codex",
		Attributes: map[string]string{
			"api_key":  "key-serving-hw",
			"base_url": serverURL,
		},
	}
	return executor, servingAuth, store, mgr
}

// codexVersionedInboundContext returns a context carrying a gin.Context whose
// inbound request advertises a real first-party codex CLI User-Agent at the given
// version (above the frozen floor 0.140.0, below the CLI sanity ceiling 1.0.0), so
// the device-profile resolution records it as a first-party observation and the
// resolved outbound version becomes that version.
func codexVersionedInboundContext(version string) context.Context {
	ua := "codex_cli_rs/" + version + " (Mac OS 15.7.4; arm64) iTerm.app/3.6.8 (codex_cli_rs; " + version + ")"
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", http.NoBody)
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Originator", "codex_cli_rs")
	ginCtx.Request = req
	return context.WithValue(context.Background(), "gin", ginCtx)
}

func assertCodexServingHighWaterPersisted(t *testing.T, mgr *cliproxyauth.Manager, store *codexServingHighWaterStore, authID, wantVersion string) {
	t.Helper()

	stored, ok := mgr.GetByID(authID)
	if !ok {
		t.Fatalf("auth %q not found after serving request", authID)
	}
	hw, ok := cliproxyauth.CodexDeviceHighWaterFromMetadata(stored.Metadata)
	if !ok {
		t.Fatalf("codex_device_high_water not written to auth.Metadata after serving request: metadata=%#v", stored.Metadata)
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
	savedHW, ok := cliproxyauth.CodexDeviceHighWaterFromMetadata(saved.Metadata)
	if !ok || savedHW.Version != wantVersion {
		t.Fatalf("persisted snapshot high-water mismatch: ok=%v version=%q want=%q", ok, savedHW.Version, wantVersion)
	}
}

// TestCodexExecutor_Execute_PersistsDeviceHighWaterFromServingPath drives the real
// /responses serving flow (CodexExecutor.Execute) with a version-bearing inbound
// codex CLI UA and asserts codex_device_high_water lands in auth.Metadata. This is
// the codex analogue of the claude serving-path guard: the write-back must fire
// from the real serving path, not from PrepareRequest (which on codex does not
// even resolve the client profile).
func TestCodexExecutor_Execute_PersistsDeviceHighWaterFromServingPath(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"status\":\"completed\",\"model\":\"gpt-5.4-mini\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n"))
	}))
	defer server.Close()

	executor, auth, store, mgr := newCodexServingHighWaterFixture(t, server.URL)

	ctx := codexVersionedInboundContext("0.150.0")
	if _, err := executor.Execute(ctx, auth, cliproxyexecutor.Request{
		Model:   "gpt-5.4-mini",
		Payload: []byte(`{"model":"gpt-5.4-mini","input":"hi"}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
	}); err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	assertCodexServingHighWaterPersisted(t, mgr, store, auth.ID, "0.150.0")
}

// TestCodexExecutor_ExecuteStream_PersistsDeviceHighWaterFromServingPath drives the
// streaming serving flow and asserts the same serving-path write-back.
func TestCodexExecutor_ExecuteStream_PersistsDeviceHighWaterFromServingPath(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"status\":\"completed\",\"model\":\"gpt-5.4-mini\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n"))
	}))
	defer server.Close()

	executor, auth, store, mgr := newCodexServingHighWaterFixture(t, server.URL)

	ctx := codexVersionedInboundContext("0.160.0")
	result, err := executor.ExecuteStream(ctx, auth, cliproxyexecutor.Request{
		Model:   "gpt-5.4-mini",
		Payload: []byte(`{"model":"gpt-5.4-mini","input":"hi"}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Stream:       true,
	})
	if err != nil {
		t.Fatalf("ExecuteStream returned error: %v", err)
	}
	if result != nil {
		for range result.Chunks {
		}
	}

	assertCodexServingHighWaterPersisted(t, mgr, store, auth.ID, "0.160.0")
}

// TestCodexExecutor_HighWaterMonotonicAndReadSeedLoop asserts the closed loop:
//   - a first serving request at 0.150.0 persists the high-water,
//   - a second serving request at a LOWER 0.145.0 does NOT lower it (only-up),
//   - a fresh resolution (cold cache, mimicking restart) seeds from the persisted
//     0.150.0 high-water instead of falling back to the static floor 0.140.0.
func TestCodexExecutor_HighWaterMonotonicAndReadSeedLoop(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"status\":\"completed\",\"model\":\"gpt-5.4-mini\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n"))
	}))
	defer server.Close()

	executor, auth, store, mgr := newCodexServingHighWaterFixture(t, server.URL)

	// First request observes 0.150.0 -> persisted.
	if _, err := executor.Execute(codexVersionedInboundContext("0.150.0"), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.4-mini",
		Payload: []byte(`{"model":"gpt-5.4-mini","input":"hi"}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response")}); err != nil {
		t.Fatalf("first Execute error: %v", err)
	}
	assertCodexServingHighWaterPersisted(t, mgr, store, auth.ID, "0.150.0")
	savesAfterFirst := store.saveCount.Load()

	// Second request observes a LOWER 0.145.0; high-water must stay at 0.150.0 and
	// no new persist write should fire (steady-state zero disk write).
	helps.ResetCodexClientProfileCacheForTests()
	if _, err := executor.Execute(codexVersionedInboundContext("0.145.0"), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.4-mini",
		Payload: []byte(`{"model":"gpt-5.4-mini","input":"hi"}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response")}); err != nil {
		t.Fatalf("second Execute error: %v", err)
	}
	stored, _ := mgr.GetByID(auth.ID)
	hw, ok := cliproxyauth.CodexDeviceHighWaterFromMetadata(stored.Metadata)
	if !ok || hw.Version != "0.150.0" {
		t.Fatalf("after lower observation high-water = (ok=%v %q), want 0.150.0 (only-up)", ok, hw.Version)
	}
	if store.saveCount.Load() != savesAfterFirst {
		t.Fatalf("lower observation triggered an extra persist write (saves %d -> %d); steady state must not write",
			savesAfterFirst, store.saveCount.Load())
	}

	// Read-seed loop: cold cache (mimicking a restart) with NO inbound observation
	// must seed the outbound version from the persisted 0.150.0 high-water rather
	// than the static floor 0.140.0.
	helps.ResetCodexClientProfileCacheForTests()
	seeded := helps.ResolveCodexClientProfile(stored, nil, &config.Config{})
	if seeded.Version != "0.150.0" {
		t.Fatalf("cold-cache read seed version = %q, want 0.150.0 (must seed from persisted high-water, not floor 0.140.0)", seeded.Version)
	}
}
