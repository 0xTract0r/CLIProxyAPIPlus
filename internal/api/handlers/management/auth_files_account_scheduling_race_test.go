package management

import (
	"context"
	"net/http"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// raceQuotaError mirrors the auth package's planQuotaError: a plan-quota 429 body
// that (with Result.QuotaExceeded) escalates Quota.BackoffLevel to >=1, tripping
// the health-gate BackoffLevel distress signal so evaluateAccountHealthGate stamps
// warmup_last_distress_at inside the nested account_scheduling map on every call.
func raceQuotaError() *coreauth.Error {
	return &coreauth.Error{
		HTTPStatus: http.StatusTooManyRequests,
		Message:    `{"type":"error","error":{"type":"rate_limit_error","message":"usage limit reached; quota exceeded"}}`,
	}
}

// TestBuildAccountSchedulingView_ConcurrentMarkResultRaceFree is the cross-package
// regression guard for the HIGH concurrency bug: before the fix, Auth.Clone
// shallow-copied Metadata, so every clone SHARED the nested account_scheduling
// map[string]any with the live record. MarkResult (under m.mu) mutates that nested
// map in place on every request (EnsureAuthFirstProductionAt on success,
// evaluateAccountHealthGate's setLastDistressAt/setHealthStageCap on distress),
// while the management projection read path (List/GetByID -> Clone ->
// buildAccountSchedulingView, the high-frequency dashboard poll) reads it WITHOUT
// m.mu -> a fatal "concurrent map read and map write".
//
// This test drives the real writer (manager.MarkResult) and the real reader
// (h.buildAccountSchedulingView on manager.List() clones) concurrently. It must
// run clean under `go test -race`; the -race detector is what proves the clone's
// account_scheduling map is now fully isolated from the in-place writes. The
// pre-fix code fails this test (data race / fatal), the post-fix deep-copy Clone
// passes it.
func TestBuildAccountSchedulingView_ConcurrentMarkResultRaceFree(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("MANAGEMENT_PASSWORD", "")

	// Health-gate-enabled config (default) so MarkResult actively writes the nested
	// account_scheduling object on every distress hit -- the exact in-place writer
	// the pre-fix shallow clone shared with every projection read.
	scheduling := config.DefaultAccountSchedulingConfig()
	if !scheduling.HealthGate.Enabled {
		t.Fatalf("precondition: default health gate must be enabled to exercise the write path")
	}
	cfg := &config.Config{AuthDir: t.TempDir(), AccountScheduling: scheduling}

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	// SetConfig wires the same scheduling config into the manager's runtime snapshot
	// so evaluateAccountHealthGateLocked (read inside MarkResult) sees the enabled gate.
	manager.SetConfig(cfg)

	record := &coreauth.Auth{
		ID:       "race.json",
		FileName: "race.json",
		Provider: "claude",
		Metadata: map[string]any{"type": "claude"},
	}
	// Anchor ~50 days back so the account is in the warm-up band (not cold, not
	// mature): the health gate only records a cap/distress for warm-up-period accounts.
	record.SetAccountFirstProductionAt(time.Now().Add(-50 * 24 * time.Hour))
	if _, err := manager.Register(context.Background(), record); err != nil {
		t.Fatalf("failed to register auth record: %v", err)
	}
	h := NewHandlerWithoutConfigFilePath(cfg, manager)

	// Skip store persistence: the race we are guarding is purely the in-memory
	// nested-map read/write, and skipping Save keeps the writer tight and avoids
	// dragging unrelated store locking into the -race surface.
	ctx := coreauth.WithSkipPersist(context.Background())

	const iterations = 300
	var wg sync.WaitGroup
	wg.Add(2)

	// Writer: repeated plan-quota 429 failures -> distress -> in-place writes to the
	// live account_scheduling map under m.mu.
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			manager.MarkResult(ctx, coreauth.Result{
				AuthID:        "race.json",
				Provider:      "claude",
				Model:         "claude-sonnet-4",
				Success:       false,
				QuotaExceeded: true,
				Error:         raceQuotaError(),
			})
		}
	}()

	// Reader: the real management projection over List() clones -- reads the nested
	// account_scheduling map (tier_override / rate_scale / first_production_at /
	// health cap / last_distress) with no lock, exactly like the dashboard poll.
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			for _, a := range manager.List() {
				_ = h.buildAccountSchedulingView(a)
			}
		}
	}()

	wg.Wait()
}

// TestBuildAuthFileEntry_DualEmitsAccountAndAdaptiveScheduling asserts the MEDIUM
// transition-safety fix: the account list projection must emit BOTH the new
// "account_scheduling" name and the legacy "adaptive_scheduling" name, with an
// identical value, so any consumer still reading the pre-§8.5 name is not silently
// broken by the rename.
func TestBuildAuthFileEntry_DualEmitsAccountAndAdaptiveScheduling(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &Handler{cfg: &config.Config{AccountScheduling: config.DefaultAccountSchedulingConfig()}}

	auth := &coreauth.Auth{
		ID:        "dualemit.json",
		FileName:  "dualemit.json",
		Provider:  "claude",
		Status:    coreauth.StatusActive,
		UpdatedAt: time.Now(),
		// runtime_only so buildAuthFileEntry does not short-circuit on a missing
		// on-disk "path" attribute (mirrors TestBuildAuthFileEntry_AdaptiveScheduling).
		Attributes: map[string]string{"runtime_only": "true"},
		Metadata: map[string]any{
			"type":                "claude",
			"first_production_at": time.Now().Add(-10 * 24 * time.Hour).UTC().Format(time.RFC3339),
			"quota_snapshot": map[string]any{
				"profile": map[string]any{
					"organization": map[string]any{"rate_limit_tier": "default_claude_max_20x"},
				},
			},
		},
	}

	entry := h.buildAuthFileEntry(auth)
	if entry == nil {
		t.Fatal("buildAuthFileEntry() = nil, want an entry")
	}

	newView, ok := entry["account_scheduling"].(gin.H)
	if !ok {
		t.Fatalf(`entry["account_scheduling"] = %#v, want gin.H`, entry["account_scheduling"])
	}
	oldView, ok := entry["adaptive_scheduling"].(gin.H)
	if !ok {
		t.Fatalf(`entry["adaptive_scheduling"] (legacy dual-emit) = %#v, want gin.H`, entry["adaptive_scheduling"])
	}
	if !reflect.DeepEqual(newView, oldView) {
		t.Fatalf("dual-emit values differ:\n account_scheduling = %#v\n adaptive_scheduling = %#v", newView, oldView)
	}
	// Sanity: the shared value actually carries the projection (not an empty object).
	if _, present := newView["subscription_tier"]; !present {
		t.Fatalf("account_scheduling projection missing subscription_tier: %#v", newView)
	}
}
