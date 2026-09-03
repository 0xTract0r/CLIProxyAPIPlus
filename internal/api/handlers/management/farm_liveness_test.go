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

// TestQuotaReauth2StrikeEscalatesToAuthoritative is C1 + C5(a) + F1: a confirmed
// quota `credential unauthorized` writes the AUTHORITATIVE reauth-required lock —
// but only after TWO consecutive confirmations (a single 401 must NOT lock).
func TestQuotaReauth2StrikeEscalatesToAuthoritative(t *testing.T) {
	t.Setenv(FarmLivenessDetectionEnvVar, "true")
	t.Setenv(coreauth.FarmRequireProvisionedEnvVar, "0") // isolate from the gate

	handler, manager, exec := newLivenessQuotaTestHandler(t)
	exec.responsesByAuth = map[string]quotaSnapshotTestResponse{
		"claude-revoked": {statusCode: http.StatusUnauthorized},
	}
	registerClaudeAuth(t, manager, "claude-revoked", true) // farm-enrolled (F1b)

	// Strike 1: must NOT lock.
	auth, _ := manager.GetByID("claude-revoked")
	_, _ = handler.refreshQuotaSnapshot(context.Background(), auth, defaultQuotaSnapshotTestPolicy())
	afterOne, _ := manager.GetByID("claude-revoked")
	if coreauth.IsReauthRequiredMetadata(afterOne.Metadata) {
		t.Fatal("F1: a single quota 401 must NOT write the authoritative lock")
	}
	if metadataInt(afterOne.Metadata, farmLivenessAuthFailStreakKey) != 1 {
		t.Fatalf("streak after strike 1 = %d, want 1", metadataInt(afterOne.Metadata, farmLivenessAuthFailStreakKey))
	}

	// Strike 2: now locks.
	_, _ = handler.refreshQuotaSnapshot(context.Background(), afterOne, defaultQuotaSnapshotTestPolicy())
	afterTwo, _ := manager.GetByID("claude-revoked")
	if !coreauth.IsReauthRequiredMetadata(afterTwo.Metadata) {
		t.Fatal("C1/F1: two consecutive quota 401s must escalate to the authoritative lock")
	}
	if afterTwo.Status != coreauth.StatusError {
		t.Fatalf("authoritative Status = %v, want StatusError", afterTwo.Status)
	}
}

// TestQuota403GoesThrough2Strike (F1.4): a 403 (often WAF/rate-limit, higher
// false-positive risk) must also require 2 strikes, never lock on a single one.
func TestQuota403GoesThrough2Strike(t *testing.T) {
	t.Setenv(FarmLivenessDetectionEnvVar, "true")
	t.Setenv(coreauth.FarmRequireProvisionedEnvVar, "0")

	handler, manager, exec := newLivenessQuotaTestHandler(t)
	exec.responsesByAuth = map[string]quotaSnapshotTestResponse{
		"claude-403": {statusCode: http.StatusForbidden},
	}
	registerClaudeAuth(t, manager, "claude-403", true)

	auth, _ := manager.GetByID("claude-403")
	_, _ = handler.refreshQuotaSnapshot(context.Background(), auth, defaultQuotaSnapshotTestPolicy())
	afterOne, _ := manager.GetByID("claude-403")
	if coreauth.IsReauthRequiredMetadata(afterOne.Metadata) {
		t.Fatal("F1.4: a single 403 must NOT lock (transient WAF/rate-limit risk)")
	}

	_, _ = handler.refreshQuotaSnapshot(context.Background(), afterOne, defaultQuotaSnapshotTestPolicy())
	afterTwo, _ := manager.GetByID("claude-403")
	if !coreauth.IsReauthRequiredMetadata(afterTwo.Metadata) {
		t.Fatal("F1.4: two consecutive 403s should escalate")
	}
}

// TestQuotaEscalationFarmScoped (F1b): a NON-farm account must never be escalated,
// even after repeated 401s — the escalation only touches farm-enrolled accounts.
func TestQuotaEscalationFarmScoped(t *testing.T) {
	t.Setenv(FarmLivenessDetectionEnvVar, "true")
	t.Setenv(coreauth.FarmRequireProvisionedEnvVar, "0")

	handler, manager, exec := newLivenessQuotaTestHandler(t)
	exec.responsesByAuth = map[string]quotaSnapshotTestResponse{
		"claude-prod": {statusCode: http.StatusUnauthorized},
	}
	registerClaudeAuth(t, manager, "claude-prod", false) // NOT farm-enrolled

	for i := 0; i < 3; i++ {
		current, _ := manager.GetByID("claude-prod")
		_, _ = handler.refreshQuotaSnapshot(context.Background(), current, defaultQuotaSnapshotTestPolicy())
	}
	updated, _ := manager.GetByID("claude-prod")
	if coreauth.IsReauthRequiredMetadata(updated.Metadata) {
		t.Fatal("F1b: a non-farm account must NEVER be escalated to the authoritative lock")
	}
	if _, ok := updated.Metadata[farmLivenessAuthFailStreakKey]; ok {
		t.Fatal("F1b: a non-farm account must not even accumulate the farm streak")
	}
}

// TestQuotaSuccessClearsLockAndStreak (F1.2, quota recovery): after a lock, a
// single successful quota refresh must reliably clear the lock AND reset the streak.
func TestQuotaSuccessClearsLockAndStreak(t *testing.T) {
	t.Setenv(FarmLivenessDetectionEnvVar, "true")
	t.Setenv(coreauth.FarmRequireProvisionedEnvVar, "0")

	handler, manager, exec := newLivenessQuotaTestHandler(t)
	exec.responsesByAuth = map[string]quotaSnapshotTestResponse{
		"claude-revoked": {statusCode: http.StatusUnauthorized},
	}
	registerClaudeAuth(t, manager, "claude-revoked", true)

	// Lock it (2 strikes).
	for i := 0; i < 2; i++ {
		current, _ := manager.GetByID("claude-revoked")
		_, _ = handler.refreshQuotaSnapshot(context.Background(), current, defaultQuotaSnapshotTestPolicy())
	}
	locked, _ := manager.GetByID("claude-revoked")
	if !coreauth.IsReauthRequiredMetadata(locked.Metadata) {
		t.Fatal("precondition: account must be locked")
	}

	// Now a successful probe recovers it.
	exec.responsesByAuth = nil // default = 200 success
	_, err := handler.refreshQuotaSnapshot(context.Background(), locked, defaultQuotaSnapshotTestPolicy())
	if err != nil {
		t.Fatalf("recovery refresh error = %v", err)
	}
	recovered, _ := manager.GetByID("claude-revoked")
	if coreauth.IsReauthRequiredMetadata(recovered.Metadata) {
		t.Fatal("F1.2: a successful quota probe must clear the authoritative lock")
	}
	if recovered.Status != coreauth.StatusActive {
		t.Fatalf("F1.2: recovered Status = %v, want StatusActive", recovered.Status)
	}
	if _, ok := recovered.Metadata[farmLivenessAuthFailStreakKey]; ok {
		t.Fatal("F1.2: recovery must reset the auth-failure streak")
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
	registerClaudeAuth(t, manager, "claude-revoked", true)

	// 1) Confirm unauthorized to the 2-strike threshold so it is authoritatively locked.
	exec.responsesByAuth = map[string]quotaSnapshotTestResponse{
		"claude-revoked": {statusCode: http.StatusUnauthorized},
	}
	for i := 0; i < 2; i++ {
		current, _ := manager.GetByID("claude-revoked")
		_, _ = handler.refreshQuotaSnapshot(context.Background(), current, defaultQuotaSnapshotTestPolicy())
	}
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

	// Strike 1: reaches the provider but must NOT lock yet.
	handler.probeAccountLiveness(context.Background(), manager, auth, defaultQuotaSnapshotTestPolicy())
	if exec.CallsForAuth("claude-frozen") == 0 {
		t.Fatal("probe must actually reach the provider for a gate-blocked account")
	}
	afterOne, _ := manager.GetByID("claude-frozen")
	if coreauth.IsReauthRequiredMetadata(afterOne.Metadata) {
		t.Fatal("F1: a single probe 401 must NOT lock")
	}

	// Strike 2: now escalates.
	handler.probeAccountLiveness(context.Background(), manager, afterOne, defaultQuotaSnapshotTestPolicy())
	afterTwo, _ := manager.GetByID("claude-frozen")
	if !coreauth.IsReauthRequiredMetadata(afterTwo.Metadata) {
		t.Fatal("A3/F1: two probe-confirmed 401s must write the authoritative reauth-required lock")
	}
}

// TestLivenessProbeSuccessClearsLock is the single most safety-critical property
// the review flagged as untested: a successful probe must reliably drive
// applyLivenessProbeSuccess -> Auth.ClearCredentialUnauthorized and release the
// authoritative lock end-to-end, so a recovered account is never pinned red.
func TestLivenessProbeSuccessClearsLock(t *testing.T) {
	t.Setenv(FarmLivenessProbeEnvVar, "true")
	t.Setenv(coreauth.FarmRequireProvisionedEnvVar, "0")

	handler, manager, _ := newLivenessQuotaTestHandler(t)
	registerClaudeAuth(t, manager, "claude-recover", true)

	// Pre-lock the account with the probe-set authoritative lock.
	auth, _ := manager.GetByID("claude-recover")
	locked := auth.Clone()
	locked.MarkCredentialUnauthorized(time.Now().UTC())
	if _, err := manager.Update(context.Background(), locked); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	pre, _ := manager.GetByID("claude-recover")
	if !coreauth.IsReauthRequiredMetadata(pre.Metadata) {
		t.Fatal("precondition: account must be locked")
	}

	// A successful probe (default executor = 200) must clear the lock.
	handler.probeAccountLiveness(context.Background(), manager, pre, defaultQuotaSnapshotTestPolicy())

	recovered, _ := manager.GetByID("claude-recover")
	if coreauth.IsReauthRequiredMetadata(recovered.Metadata) {
		t.Fatal("F1.2 (critical): a successful probe must clear the authoritative credential-unauthorized lock")
	}
	if recovered.Status != coreauth.StatusActive {
		t.Fatalf("F1.2: recovered Status = %v, want StatusActive", recovered.Status)
	}
	if got := metadataString(recovered.Metadata, quotaRefreshStatusMetadataKey); got != quotaRefreshStatusOK {
		t.Fatalf("F1.2: quota status after recovery = %q, want ok", got)
	}
}

func TestFarmLivenessRecordAuthFailureStreak(t *testing.T) {
	now := time.Now().UTC()
	meta := map[string]any{}

	if got := farmLivenessRecordAuthFailure(meta, now); got != 1 {
		t.Fatalf("first failure streak = %d, want 1", got)
	}
	if got := farmLivenessRecordAuthFailure(meta, now.Add(time.Minute)); got != 2 {
		t.Fatalf("second failure streak = %d, want 2", got)
	}
	// At/above threshold: pinned, no unbounded growth.
	if got := farmLivenessRecordAuthFailure(meta, now.Add(2*time.Minute)); got != farmLivenessAuthFailThreshold {
		t.Fatalf("third failure streak = %d, want pinned at %d", got, farmLivenessAuthFailThreshold)
	}

	// A success resets.
	farmLivenessResetAuthFailure(meta)
	if _, ok := meta[farmLivenessAuthFailStreakKey]; ok {
		t.Fatal("reset must remove the streak")
	}

	// Window expiry restarts at 1, not 2.
	_ = farmLivenessRecordAuthFailure(meta, now)
	if got := farmLivenessRecordAuthFailure(meta, now.Add(farmLivenessAuthFailWindow+time.Minute)); got != 1 {
		t.Fatalf("post-window streak = %d, want restart at 1", got)
	}
}

func TestFarmLivenessRecoveryReprobeEligible(t *testing.T) {
	t.Setenv(FarmLivenessDetectionEnvVar, "true")
	now := time.Now().UTC()

	lockedFarm := &coreauth.Auth{Provider: "claude", Metadata: map[string]any{coreauth.FarmEnrolledMetadataKey: true}}
	lockedFarm.MarkCredentialUnauthorized(now)
	if !farmLivenessRecoveryReprobeEligible(lockedFarm) {
		t.Fatal("a locked farm account must be re-probe eligible for recovery (detection-only self-heal)")
	}

	lockedNonFarm := &coreauth.Auth{Provider: "claude", Metadata: map[string]any{}}
	lockedNonFarm.MarkCredentialUnauthorized(now)
	if farmLivenessRecoveryReprobeEligible(lockedNonFarm) {
		t.Fatal("a non-farm locked account must NOT be re-probed by this path")
	}

	reuseFarm := &coreauth.Auth{Provider: "claude", Metadata: map[string]any{coreauth.FarmEnrolledMetadataKey: true}}
	reuseFarm.MarkRefreshReauthRequired(now) // refresh-reuse lock, not our lock
	if farmLivenessRecoveryReprobeEligible(reuseFarm) {
		t.Fatal("a refresh-reuse lock must NOT trigger our recovery re-probe")
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
