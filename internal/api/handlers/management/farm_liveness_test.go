package management

import (
	"context"
	"net/http"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

const testLivenessDeviceID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestParseFarmLivenessFlag(t *testing.T) {
	armed := []string{"1", "true", "TRUE", "yes", "on", " on "}
	for _, v := range armed {
		if !parseFarmLivenessFlag(v) {
			t.Fatalf("parseFarmLivenessFlag(%q) = false, want true", v)
		}
	}
	disarmed := []string{"", "0", "false", "no", "off", "garbage", "  "}
	for _, v := range disarmed {
		if parseFarmLivenessFlag(v) {
			t.Fatalf("parseFarmLivenessFlag(%q) = true, want false (default off / allowlist)", v)
		}
	}
}

func TestFarmLivenessFlagsDefaultOff(t *testing.T) {
	// No env set: both must be off so production never silently arms.
	if farmLivenessDetectionEnabled() {
		t.Fatal("farmLivenessDetectionEnabled default = true, want false")
	}
	if farmLivenessProbeEnabled() {
		t.Fatal("farmLivenessProbeEnabled default = true, want false")
	}
	t.Setenv(FarmLivenessDetectionEnvVar, "true")
	t.Setenv(FarmLivenessProbeEnvVar, "on")
	if !farmLivenessDetectionEnabled() || !farmLivenessProbeEnabled() {
		t.Fatal("flags did not arm when set to truthy tokens")
	}
}

func TestFarmLivenessProbeEligibility(t *testing.T) {
	now := time.Now().UTC()
	matureAnchor := now.Add(-2 * time.Hour).Format(time.RFC3339)
	coldAnchor := now.Add(-1 * time.Minute).Format(time.RFC3339)

	everBoundMature := &coreauth.Auth{
		Provider: "claude",
		Metadata: map[string]any{
			coreauth.FarmEnrolledMetadataKey:      true,
			coreauth.ClaudeDeviceIDMetadataKey:    testLivenessDeviceID,
			coreauth.FirstProductionAtMetadataKey: matureAnchor,
		},
		Attributes: map[string]string{coreauth.ClaudeDeviceIDAttributeKey: testLivenessDeviceID},
	}
	if !farmLivenessProbeEligible(everBoundMature, now) {
		t.Fatal("ever-bound, farm-enrolled, mature claude account must be eligible")
	}

	neverBound := &coreauth.Auth{
		Provider: "claude",
		Metadata: map[string]any{
			coreauth.FarmEnrolledMetadataKey:      true,
			coreauth.FirstProductionAtMetadataKey: matureAnchor,
		},
	}
	if farmLivenessProbeEligible(neverBound, now) {
		t.Fatal("never-bound (synthetic) account must NOT be eligible (leak boundary)")
	}

	coldStart := &coreauth.Auth{
		Provider: "claude",
		Metadata: map[string]any{
			coreauth.FarmEnrolledMetadataKey:      true,
			coreauth.ClaudeDeviceIDMetadataKey:    testLivenessDeviceID,
			coreauth.FirstProductionAtMetadataKey: coldAnchor,
		},
		Attributes: map[string]string{coreauth.ClaudeDeviceIDAttributeKey: testLivenessDeviceID},
	}
	if farmLivenessProbeEligible(coldStart, now) {
		t.Fatal("cold-start account must NOT be eligible (do not stress-test fresh accounts)")
	}

	noAnchor := &coreauth.Auth{
		Provider: "claude",
		Metadata: map[string]any{
			coreauth.FarmEnrolledMetadataKey:   true,
			coreauth.ClaudeDeviceIDMetadataKey: testLivenessDeviceID,
		},
		Attributes: map[string]string{coreauth.ClaudeDeviceIDAttributeKey: testLivenessDeviceID},
	}
	if farmLivenessProbeEligible(noAnchor, now) {
		t.Fatal("account with no first-production anchor (never served) must NOT be eligible")
	}

	notEnrolled := &coreauth.Auth{
		Provider: "claude",
		Metadata: map[string]any{
			coreauth.ClaudeDeviceIDMetadataKey:    testLivenessDeviceID,
			coreauth.FirstProductionAtMetadataKey: matureAnchor,
		},
		Attributes: map[string]string{coreauth.ClaudeDeviceIDAttributeKey: testLivenessDeviceID},
	}
	if farmLivenessProbeEligible(notEnrolled, now) {
		t.Fatal("non-enrolled account must NOT be eligible")
	}

	codex := &coreauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{
			coreauth.FarmEnrolledMetadataKey:      true,
			coreauth.FirstProductionAtMetadataKey: matureAnchor,
		},
	}
	if farmLivenessProbeEligible(codex, now) {
		t.Fatal("non-claude provider must NOT be eligible")
	}
}

func TestFarmLivenessProbeDue(t *testing.T) {
	now := time.Now().UTC()

	fresh := &coreauth.Auth{Metadata: map[string]any{
		quotaLastRefreshedMetadataKey: now.Add(-1 * time.Minute).Format(time.RFC3339),
	}}
	if farmLivenessProbeDue(fresh, now) {
		t.Fatal("account refreshed by the quota poller 1m ago is NOT due (covered)")
	}

	frozen := &coreauth.Auth{Metadata: map[string]any{
		quotaLastRefreshedMetadataKey: now.Add(-2 * time.Hour).Format(time.RFC3339),
	}}
	if !farmLivenessProbeDue(frozen, now) {
		t.Fatal("account not refreshed for 2h (frozen/blocked) IS due")
	}

	recentlyProbed := &coreauth.Auth{Metadata: map[string]any{
		quotaLastRefreshedMetadataKey:   now.Add(-2 * time.Hour).Format(time.RFC3339),
		farmLivenessProbedAtMetadataKey: now.Add(-1 * time.Minute).Format(time.RFC3339),
	}}
	if farmLivenessProbeDue(recentlyProbed, now) {
		t.Fatal("account probed 1m ago is NOT due (per-account throttle)")
	}

	neverSeen := &coreauth.Auth{Metadata: map[string]any{}}
	if !farmLivenessProbeDue(neverSeen, now) {
		t.Fatal("account with no health signal at all IS due")
	}
}

// TestQuotaReauthEscalatesToAuthoritativeWhenArmed is C1 + C5(a): a confirmed
// quota `credential unauthorized` writes the AUTHORITATIVE reauth-required lock
// (not just the quota sub-field), so the account reads red instead of green.
func TestQuotaReauthEscalatesToAuthoritativeWhenArmed(t *testing.T) {
	t.Setenv(FarmLivenessDetectionEnvVar, "true")
	// Disarm the provisioned gate so this test isolates the escalation logic from
	// the gate (the account is a plain claude account, not farm-scoped here).
	t.Setenv(coreauth.FarmRequireProvisionedEnvVar, "0")

	handler, manager, exec := newLivenessQuotaTestHandler(t)
	exec.responsesByAuth = map[string]quotaSnapshotTestResponse{
		"claude-revoked": {statusCode: http.StatusUnauthorized},
	}
	registerClaudeAuth(t, manager, "claude-revoked", false)

	auth, _ := manager.GetByID("claude-revoked")
	if _, err := handler.refreshQuotaSnapshot(context.Background(), auth, defaultQuotaSnapshotTestPolicy()); err == nil {
		t.Fatal("expected a reauth error from the 401 probe")
	}

	updated, _ := manager.GetByID("claude-revoked")
	if !coreauth.IsReauthRequiredMetadata(updated.Metadata) {
		t.Fatal("C1: quota 401 must escalate to the authoritative reauth-required lock")
	}
	if updated.Status != coreauth.StatusError {
		t.Fatalf("C1: authoritative Status = %v, want StatusError", updated.Status)
	}
	if got := metadataString(updated.Metadata, quotaRefreshStatusMetadataKey); got != quotaRefreshStatusReauthRequired {
		t.Fatalf("quota sub-field status = %q, want reauth_required", got)
	}
}

// TestQuotaReauthNotEscalatedWhenDisarmed pins the default-off guarantee: with
// the flag off, behaviour is byte-identical to before (only the sub-field, no
// authoritative lock).
func TestQuotaReauthNotEscalatedWhenDisarmed(t *testing.T) {
	t.Setenv(coreauth.FarmRequireProvisionedEnvVar, "0")

	handler, manager, exec := newLivenessQuotaTestHandler(t)
	exec.responsesByAuth = map[string]quotaSnapshotTestResponse{
		"claude-revoked": {statusCode: http.StatusUnauthorized},
	}
	registerClaudeAuth(t, manager, "claude-revoked", false)

	auth, _ := manager.GetByID("claude-revoked")
	_, _ = handler.refreshQuotaSnapshot(context.Background(), auth, defaultQuotaSnapshotTestPolicy())

	updated, _ := manager.GetByID("claude-revoked")
	if coreauth.IsReauthRequiredMetadata(updated.Metadata) {
		t.Fatal("with the flag off the quota 401 must NOT write the authoritative lock")
	}
	if got := metadataString(updated.Metadata, quotaRefreshStatusMetadataKey); got != quotaRefreshStatusReauthRequired {
		t.Fatalf("existing sub-field behaviour changed: status = %q, want reauth_required", got)
	}
}

// TestTransientErrorDoesNotOverwriteConfirmedUnauthorized is C2 (anti-overwrite):
// once a credential is authoritatively confirmed unauthorized, a later network
// timeout MUST NOT roll it back to a benign error (the incident's exact bug).
func TestTransientErrorDoesNotOverwriteConfirmedUnauthorized(t *testing.T) {
	t.Setenv(FarmLivenessDetectionEnvVar, "true")
	t.Setenv(coreauth.FarmRequireProvisionedEnvVar, "0")

	handler, manager, exec := newLivenessQuotaTestHandler(t)
	registerClaudeAuth(t, manager, "claude-revoked", false)

	// 1) Confirm unauthorized.
	exec.responsesByAuth = map[string]quotaSnapshotTestResponse{
		"claude-revoked": {statusCode: http.StatusUnauthorized},
	}
	auth, _ := manager.GetByID("claude-revoked")
	_, _ = handler.refreshQuotaSnapshot(context.Background(), auth, defaultQuotaSnapshotTestPolicy())
	confirmed, _ := manager.GetByID("claude-revoked")
	if !coreauth.IsReauthRequiredMetadata(confirmed.Metadata) {
		t.Fatal("precondition: account must be confirmed unauthorized")
	}

	// 2) A subsequent transient network timeout must not clear the confirmed state.
	exec.responsesByAuth = map[string]quotaSnapshotTestResponse{
		"claude-revoked": {err: context.DeadlineExceeded},
	}
	_, _ = handler.refreshQuotaSnapshot(context.Background(), confirmed, defaultQuotaSnapshotTestPolicy())

	after, _ := manager.GetByID("claude-revoked")
	if !coreauth.IsReauthRequiredMetadata(after.Metadata) {
		t.Fatal("C2: transient timeout rolled back the authoritative reauth-required lock")
	}
	if got := metadataString(after.Metadata, quotaRefreshStatusMetadataKey); got != quotaRefreshStatusReauthRequired {
		t.Fatalf("C2: transient timeout overwrote quota sub-field to %q, want reauth_required", got)
	}
}

// TestLivenessProbeCoversFrozenAccountAndMarksAuthoritative is the Phase-2 core
// closure: the serving-independent probe re-checks an account the quota poller
// SKIPS (container-dead / refresh-frozen) and, on a 401, writes the authoritative
// lock — the only path that turns an idle revoked account red.
func TestLivenessProbeCoversFrozenAccountAndMarksAuthoritative(t *testing.T) {
	t.Setenv(FarmLivenessProbeEnvVar, "true")
	// Arm the container-alive gate so the normal quota poller would SKIP this
	// account (its heartbeat is stale) — proving the probe covers the gap.
	t.Setenv(coreauth.FarmRequireContainerAliveEnvVar, "1")

	handler, manager, exec := newLivenessQuotaTestHandler(t)
	exec.responsesByAuth = map[string]quotaSnapshotTestResponse{
		"claude-frozen": {statusCode: http.StatusUnauthorized},
	}
	registerClaudeAuth(t, manager, "claude-frozen", true)

	auth, _ := manager.GetByID("claude-frozen")
	// Sanity: the anti-corr gate is indeed blocking the normal poller for it.
	if !coreauth.RequireProvisionedBlocked(auth) {
		t.Fatal("precondition: gate should block the normal quota poller for a stale-heartbeat account")
	}
	if !farmLivenessProbeEligible(auth, time.Now().UTC()) {
		t.Fatal("precondition: ever-bound mature farm account must be probe-eligible")
	}

	handler.probeAccountLiveness(context.Background(), manager, auth, defaultQuotaSnapshotTestPolicy())

	if exec.CallsForAuth("claude-frozen") == 0 {
		t.Fatal("probe must actually reach the provider for a gate-blocked account")
	}
	updated, _ := manager.GetByID("claude-frozen")
	if !coreauth.IsReauthRequiredMetadata(updated.Metadata) {
		t.Fatal("A3: a probe-confirmed 401 must write the authoritative reauth-required lock")
	}
}

// TestLivenessProbeTransientPreservesConfirmedState is the probe-side C2: a
// transient probe error must not clear a previously-confirmed lock.
func TestLivenessProbeTransientPreservesConfirmedState(t *testing.T) {
	t.Setenv(FarmLivenessProbeEnvVar, "true")
	t.Setenv(coreauth.FarmRequireProvisionedEnvVar, "0")

	handler, manager, exec := newLivenessQuotaTestHandler(t)
	registerClaudeAuth(t, manager, "claude-frozen", true)

	// Pre-confirm the lock directly.
	auth, _ := manager.GetByID("claude-frozen")
	locked := auth.Clone()
	locked.MarkCredentialUnauthorized(time.Now().UTC())
	if _, err := manager.Update(context.Background(), locked); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	exec.responsesByAuth = map[string]quotaSnapshotTestResponse{
		"claude-frozen": {err: context.DeadlineExceeded},
	}
	current, _ := manager.GetByID("claude-frozen")
	handler.probeAccountLiveness(context.Background(), manager, current, defaultQuotaSnapshotTestPolicy())

	after, _ := manager.GetByID("claude-frozen")
	if !coreauth.IsReauthRequiredMetadata(after.Metadata) {
		t.Fatal("probe C2: a transient probe error rolled back the confirmed lock")
	}
}

func newLivenessQuotaTestHandler(t *testing.T) (*Handler, *coreauth.Manager, *quotaSnapshotTestExecutor) {
	t.Helper()
	manager := coreauth.NewManager(nil, nil, nil)
	exec := &quotaSnapshotTestExecutor{provider: "claude"}
	manager.RegisterExecutor(exec)
	handler := NewHandlerWithoutConfigFilePath(nil, manager)
	return handler, manager, exec
}

func registerClaudeAuth(t *testing.T, manager *coreauth.Manager, id string, farm bool) {
	t.Helper()
	meta := map[string]any{
		coreauth.ClaudeDeviceIDMetadataKey:    testLivenessDeviceID,
		coreauth.FirstProductionAtMetadataKey: time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339),
	}
	if farm {
		meta[coreauth.FarmEnrolledMetadataKey] = true
	}
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       id,
		Provider: "claude",
		ProxyURL: "http://test-proxy:8080",
		Metadata: meta,
	}); err != nil {
		t.Fatalf("Register(%s) error = %v", id, err)
	}
}
