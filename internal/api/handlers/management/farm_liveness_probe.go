// Serving-independent farm account liveness probe (openspec change
// farm-account-liveness-detection, Phase 2 / gap A).
//
// The incident's core gap: an idle farm account revoked upstream is caught by
// NONE of the existing mechanisms — token refresh is short-circuited by
// refresh_disabled, the quota poller is skipped by the anti-corr container-alive
// gate, and auto-quarantine is fed only by real serving 401s. So a revoked idle
// account (no serving traffic) shows green forever.
//
// This probe closes that gap by actively RE-USING the existing profile/quota
// probe (fetchProviderQuotaSnapshot) against exactly the accounts the normal
// quota poller SKIPS (container-dead / refresh-frozen), so a revocation becomes
// an observable, serving-independent signal. It never introduces a fabricated
// /v1/messages business request (that would count as usage and look abusive);
// it is the same low-risk GET /api/oauth/profile+usage call the quota poller
// already performs.
//
// Anti-corr / anti-burn guards (A2), all enforced here:
//   - leak boundary: only ever probes AuthEverBoundToContainer accounts (their
//     device_id is already on-wire exposed); never a never-bound synthetic
//     account (that is the leak the RequireProvisionedBlocked gate prevents, and
//     it is NOT relaxed — this probe simply targets the already-exposed set);
//   - low frequency: minutes-scale loop, per-account staleness throttle, serial
//     with jitter, never a synchronized burst;
//   - cold-start exemption: brand-new / just-provisioned accounts are skipped;
//   - no raw token: reuses the managed executor request path (same as the quota
//     poller), never dumps or side-channels the bearer token;
//   - transient errors never overwrite a confirmed state (C2).
package management

import (
	"context"
	"math/rand"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const (
	// farmLivenessProbedAtMetadataKey records when this account was last actively
	// probed by the liveness loop (RFC3339 UTC). It throttles the probe cadence
	// per account independently of the quota poller's own last-refresh timestamp,
	// so a confirmed-dead account is re-checked only sparingly (recovery
	// detection) rather than hammered.
	farmLivenessProbedAtMetadataKey = "farm_liveness_probed_at"
)

// StartFarmLivenessProbe launches (or restarts) the serving-independent liveness
// probe loop. It mirrors StartQuotaSnapshotAutoRefresh's lifecycle exactly
// (cancel-previous, no-op when disarmed) so a config reload re-evaluates the
// gate cleanly. Default off: the loop only starts when FARM_LIVENESS_PROBE_ENABLED
// is armed.
func (h *Handler) StartFarmLivenessProbe(parent context.Context, policy QuotaSnapshotRefreshPolicy) {
	if h == nil {
		return
	}
	if parent == nil {
		parent = context.Background()
	}
	policy = policy.normalized()

	h.mu.Lock()
	cancelPrev := h.farmLivenessProbeCancel
	h.farmLivenessProbeCancel = nil
	h.mu.Unlock()
	if cancelPrev != nil {
		cancelPrev()
	}
	if !farmLivenessProbeEnabled() {
		return
	}

	ctx, cancel := context.WithCancel(parent)
	h.mu.Lock()
	h.farmLivenessProbeCancel = cancel
	h.mu.Unlock()

	go h.runFarmLivenessProbe(ctx, policy)
}

func (h *Handler) runFarmLivenessProbe(ctx context.Context, policy QuotaSnapshotRefreshPolicy) {
	ticker := time.NewTicker(farmLivenessProbeInterval)
	defer ticker.Stop()
	h.runFarmLivenessProbePass(ctx, policy)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			h.runFarmLivenessProbePass(ctx, policy)
		}
	}
}

// runFarmLivenessProbePass walks all accounts once and probes each eligible,
// due account serially (with a small jitter between probes). It re-checks the
// arm flag on every pass so a live disarm stops all probing at the next tick.
func (h *Handler) runFarmLivenessProbePass(ctx context.Context, policy QuotaSnapshotRefreshPolicy) {
	if !farmLivenessProbeEnabled() {
		return
	}
	manager := h.currentAuthManager()
	if manager == nil {
		return
	}
	policy = policy.normalized()
	now := time.Now().UTC()
	for _, auth := range manager.List() {
		select {
		case <-ctx.Done():
			return
		default:
		}
		if !farmLivenessProbeEligible(auth, now) {
			continue
		}
		if !farmLivenessProbeDue(auth, now) {
			continue
		}
		if !sleepWithJitter(ctx, farmLivenessProbeJitterMax) {
			return
		}
		h.probeAccountLiveness(ctx, manager, auth, policy)
	}
}

// farmLivenessProbeDue reports whether an eligible account is due for a probe:
// its most recent health signal — the later of the quota poller's last refresh
// and this loop's own last probe — is staler than farmLivenessProbeStaleThreshold.
// This makes the probe naturally SKIP accounts the quota poller is still keeping
// fresh (they are covered) and COVER exactly the frozen/blocked accounts the
// poller has stopped refreshing, while throttling re-probes of a confirmed-dead
// account to at most once per threshold window.
func farmLivenessProbeDue(auth *coreauth.Auth, now time.Time) bool {
	if auth == nil {
		return false
	}
	last := time.Time{}
	if ts, ok := metadataTime(auth.Metadata, quotaLastRefreshedMetadataKey); ok && ts.After(last) {
		last = ts
	}
	if ts, ok := metadataTime(auth.Metadata, farmLivenessProbedAtMetadataKey); ok && ts.After(last) {
		last = ts
	}
	if last.IsZero() {
		return true
	}
	return now.Sub(last) >= farmLivenessProbeStaleThreshold
}

// probeAccountLiveness performs one liveness probe against an eligible account,
// DELIBERATELY bypassing RequireProvisionedBlocked: eligibility already required
// AuthEverBoundToContainer, so the account's device_id is already on-wire exposed
// and re-probing adds no new leak surface (the gate's leak-prevention semantics
// for never-bound accounts are untouched). It reuses the exact profile/quota
// probe the poller uses.
func (h *Handler) probeAccountLiveness(ctx context.Context, manager *coreauth.Manager, auth *coreauth.Auth, policy QuotaSnapshotRefreshPolicy) {
	if manager == nil || auth == nil {
		return
	}
	exec, ok := manager.Executor(auth.Provider)
	if !ok || exec == nil {
		return
	}
	providerCtx, cancel := context.WithTimeout(ctx, farmLivenessProviderTimeout)
	defer cancel()
	snapshot, planType, err := fetchProviderQuotaSnapshot(providerCtx, exec, auth)
	now := time.Now().UTC()

	if err == nil {
		h.applyLivenessProbeSuccess(ctx, manager, auth, snapshot, planType, now, policy)
		return
	}
	status, message := quotaSnapshotErrorStatusAndMessage(err)
	if status == quotaRefreshStatusReauthRequired {
		h.applyLivenessProbeUnauthorized(ctx, manager, auth, message, now, policy)
		return
	}
	// Transient / retryable probe failure (network timeout, context deadline,
	// proxy blip, ...): C2 anti-overwrite — never roll back a confirmed state and
	// never clear a lock. Only stamp the throttle timestamp so we do not re-probe
	// too soon.
	h.applyLivenessProbeTransient(ctx, manager, auth, now, err)
}

// applyLivenessProbeUnauthorized records a probe-confirmed credential
// unauthorized and, only once it reaches the 2-strike threshold, escalates it
// into the authoritative reauth-required lock (A3 closing back into Phase 1's
// C1). It shares the SAME persisted streak as the quota poller, so a single
// probe 401/403 never locks a healthy account (review F1); confirmations from
// either mechanism cooperate toward the threshold. It runs whenever the probe
// loop is armed — the probe IS the authoritative detector for idle accounts, so
// it does not depend on the Phase 1 detection flag. The quota sub-field is also
// written for quota-card consistency.
func (h *Handler) applyLivenessProbeUnauthorized(ctx context.Context, manager *coreauth.Manager, auth *coreauth.Auth, message string, now time.Time, policy QuotaSnapshotRefreshPolicy) {
	updated := auth.Clone()
	if updated.Metadata == nil {
		updated.Metadata = make(map[string]any)
	}
	updated.Metadata[quotaRefreshStatusMetadataKey] = quotaRefreshStatusReauthRequired
	updated.Metadata[quotaRefreshErrorMetadataKey] = message
	updated.Metadata[quotaNextRefreshMetadataKey] = quotaSnapshotNextRefreshTime(updated, now, policy).Format(time.RFC3339)
	updated.Metadata[farmLivenessProbedAtMetadataKey] = now.Format(time.RFC3339)

	streak := farmLivenessRecordAuthFailure(updated.Metadata, now)
	escalated := streak >= farmLivenessAuthFailThreshold
	if escalated {
		// A health-blind account that a probe just confirmed dead to threshold is
		// no longer merely blind — it has a concrete authoritative reason. Release
		// the softer marker and write the authoritative lock.
		delete(updated.Metadata, farmHealthBlindMetadataKey)
		delete(updated.Metadata, farmHealthBlindAtMetadataKey)
		updated.MarkCredentialUnauthorized(now)
	}
	updated.UpdatedAt = now
	if _, err := manager.Update(ctx, updated); err != nil && !isContextCanceled(err) {
		log.WithError(err).Debugf("farm liveness probe: unauthorized write failed for %s/%s", auth.Provider, auth.ID)
		return
	}
	event := "farm_liveness_probe_unauthorized_streak"
	msg := "farm liveness probe unauthorized below threshold; not escalated yet"
	if escalated {
		event = "farm_liveness_probe_unauthorized_escalated"
		msg = "farm liveness probe confirmed credential unauthorized to threshold; account marked reauth-required"
	}
	log.WithFields(log.Fields{
		"auth_id":  auth.ID,
		"provider": auth.Provider,
		"streak":   streak,
		"event":    event,
	}).Warn(msg)
}

// applyLivenessProbeSuccess records a successful probe: the account is reachable
// and its credential is valid. It refreshes the quota snapshot (so the account's
// health view goes fresh/green), resets the auth-failure streak, clears any
// lingering health-blind marker, and — this is the critical F1 symmetric recovery
// path — reliably releases a previously-set probe-set reauth-required lock so a
// genuinely recovered account is never pinned red forever.
// Auth.ClearCredentialUnauthorized only clears the PROBE-SET lock; it never
// reopens a refresh-token-reuse lock or an operator's explicit refresh-disable.
func (h *Handler) applyLivenessProbeSuccess(ctx context.Context, manager *coreauth.Manager, auth *coreauth.Auth, snapshot map[string]any, planType string, now time.Time, policy QuotaSnapshotRefreshPolicy) {
	updated := auth.Clone()
	if updated.Metadata == nil {
		updated.Metadata = make(map[string]any)
	}
	updated.Metadata[quotaSnapshotMetadataKey] = snapshot
	updated.Metadata[quotaRefreshStatusMetadataKey] = quotaRefreshStatusOK
	delete(updated.Metadata, quotaRefreshErrorMetadataKey)
	delete(updated.Metadata, farmHealthBlindMetadataKey)
	delete(updated.Metadata, farmHealthBlindAtMetadataKey)
	updated.Metadata[quotaLastRefreshedMetadataKey] = now.Format(time.RFC3339)
	updated.Metadata[quotaNextRefreshMetadataKey] = quotaSnapshotNextRefreshTime(updated, now, policy).Format(time.RFC3339)
	updated.Metadata[farmLivenessProbedAtMetadataKey] = now.Format(time.RFC3339)
	if planType != "" {
		updated.Metadata[quotaSnapshotPlanTypeKey] = planType
	}
	farmLivenessResetAuthFailure(updated.Metadata)
	recovered := updated.ClearCredentialUnauthorized(now)
	updated.UpdatedAt = now
	if _, err := manager.Update(ctx, updated); err != nil && !isContextCanceled(err) {
		log.WithError(err).Debugf("farm liveness probe: success write failed for %s/%s", auth.Provider, auth.ID)
		return
	}
	if recovered {
		log.WithFields(log.Fields{
			"auth_id":  auth.ID,
			"provider": auth.Provider,
			"event":    "farm_liveness_probe_recovered",
		}).Warn("farm liveness probe succeeded; cleared authoritative credential-unauthorized lock")
	}
}

// applyLivenessProbeTransient records only the throttle timestamp after a
// retryable probe failure. It deliberately does NOT touch quota status, the
// health-blind marker, or any reauth lock: a transient error is not evidence of
// anything and must never roll back a confirmed state (C2).
func (h *Handler) applyLivenessProbeTransient(ctx context.Context, manager *coreauth.Manager, auth *coreauth.Auth, now time.Time, cause error) {
	updated := auth.Clone()
	if updated.Metadata == nil {
		updated.Metadata = make(map[string]any)
	}
	updated.Metadata[farmLivenessProbedAtMetadataKey] = now.Format(time.RFC3339)
	updated.UpdatedAt = now
	if _, err := manager.Update(ctx, updated); err != nil && !isContextCanceled(err) {
		log.WithError(err).Debugf("farm liveness probe: transient stamp failed for %s/%s", auth.Provider, auth.ID)
		return
	}
	log.WithFields(log.Fields{
		"auth_id":  auth.ID,
		"provider": auth.Provider,
		"event":    "farm_liveness_probe_transient",
	}).WithError(cause).Debug("farm liveness probe hit a transient error; confirmed state preserved")
}

// sleepWithJitter sleeps for a random duration in [0, max) or returns false if
// the context is cancelled first. A zero/negative max sleeps nothing.
func sleepWithJitter(ctx context.Context, max time.Duration) bool {
	if max <= 0 {
		return true
	}
	d := time.Duration(rand.Int63n(int64(max)))
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func isContextCanceled(err error) bool {
	return err != nil && (err == context.Canceled || err == context.DeadlineExceeded)
}
