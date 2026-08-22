package executor

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// enrolledUnprovisionedClaudeRefreshAuth builds a farm-enrolled Claude account
// that carries a refresh_token but no real claude_device_id binding — the
// population the R5-3d executor-layer supply-atomicity gate fail-closes when the
// account also has no proxy. proxyURL "" reproduces the no-proxy case.
func enrolledUnprovisionedClaudeRefreshAuth(proxyURL string) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{
		ID:       "claude-enrolled-unprov",
		Provider: "claude",
		ProxyURL: proxyURL,
		Metadata: map[string]any{
			"refresh_token":                      "fake_refresh_token",
			cliproxyauth.FarmEnrolledMetadataKey: true,
		},
	}
}

// TestClaudeExecutor_Refresh_FarmUnprovisionedNoProxy_FailsClosed is the core
// R5-3d guard: with FARM_REQUIRE_PROVISIONED armed, an enrolled-but-unprovisioned
// Claude account with no resolved proxy (neither account nor global config) must
// be refused BEFORE the OAuth service is constructed, so no direct egress ever
// dials api.anthropic.com. The absence of any test server plus the immediate
// sentinel error is the proof that no CONNECT / no network I/O happened.
func TestClaudeExecutor_Refresh_FarmUnprovisionedNoProxy_FailsClosed(t *testing.T) {
	t.Setenv(cliproxyauth.FarmRequireProvisionedEnvVar, "1")

	exec := NewClaudeExecutor(&config.Config{}) // no global proxy
	auth := enrolledUnprovisionedClaudeRefreshAuth("")

	got, err := exec.Refresh(context.Background(), auth)
	if !errors.Is(err, errFarmUnprovisionedRefreshProxyMissing) {
		t.Fatalf("Refresh err = %v, want errFarmUnprovisionedRefreshProxyMissing (fail-closed, no direct egress)", err)
	}
	if got != nil {
		t.Fatalf("Refresh returned auth = %v, want nil on fail-closed", got)
	}
}

// TestClaudeExecutor_Refresh_FarmUnprovisionedWithProxy_StillRefreshes proves the
// guard is scoped strictly to the no-proxy case: an enrolled-but-unprovisioned
// account that DOES carry a proxy is not fail-closed — its refresh egresses
// through that proxy (observed CONNECT) instead of returning the sentinel. This
// guarantees a properly-proxied enrolled account keeps refreshing normally.
func TestClaudeExecutor_Refresh_FarmUnprovisionedWithProxy_StillRefreshes(t *testing.T) {
	t.Setenv(cliproxyauth.FarmRequireProvisionedEnvVar, "1")

	var connectHits int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			atomic.AddInt32(&connectHits, 1)
		}
		// Fail fast so the executor returns quickly; observing CONNECT is enough.
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer proxy.Close()

	exec := NewClaudeExecutor(&config.Config{})
	auth := enrolledUnprovisionedClaudeRefreshAuth(proxy.URL)

	_, err := exec.Refresh(context.Background(), auth)
	if errors.Is(err, errFarmUnprovisionedRefreshProxyMissing) {
		t.Fatalf("Refresh fail-closed for a properly-proxied enrolled account; guard must be scoped to the no-proxy case only")
	}
	if got := atomic.LoadInt32(&connectHits); got == 0 {
		t.Fatalf("expected refresh to egress through account proxy (CONNECT), got 0 hits")
	}
}

// TestClaudeExecutor_Refresh_FlagOff_NoProxy_NotFailClosed proves the flag-off
// no-op: with FARM_REQUIRE_PROVISIONED unset, the exact enrolled-but-unprovisioned
// no-proxy account is NOT fail-closed — control falls through to the normal
// refresh path (which here fails with the pre-cancelled context, deterministically
// and without any real network I/O), never the fail-closed sentinel.
func TestClaudeExecutor_Refresh_FlagOff_NoProxy_NotFailClosed(t *testing.T) {
	// PG-1: FARM_REQUIRE_PROVISIONED now defaults to ARMED, so "" no longer
	// means off — force it off explicitly with a recognized falsey token.
	t.Setenv(cliproxyauth.FarmRequireProvisionedEnvVar, "0") // gate off

	exec := NewClaudeExecutor(&config.Config{})
	auth := enrolledUnprovisionedClaudeRefreshAuth("")

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // short-circuit the normal refresh attempt without hitting the network

	if _, err := exec.Refresh(ctx, auth); errors.Is(err, errFarmUnprovisionedRefreshProxyMissing) {
		t.Fatalf("flag off must be a byte-identical no-op; got the fail-closed sentinel")
	}
}
