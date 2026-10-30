package auth

import (
	"context"
	"strconv"
	"sync/atomic"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

type pacingSelectionKey struct{}

func warmupServingEnabled(cfg internalconfig.AccountSchedulingConfig) bool {
	return cfg.WarmupServingReserve > 0 || cfg.WarmupTrafficPacing.Enabled
}

var pacingSelectorSequence atomic.Uint64

func (s *AdaptiveSelector) pacingMemberID(key string, revision uint64) string {
	return strconv.FormatUint(s.pacingInstance, 10) + "::" + key + "::revision:" + strconv.FormatUint(revision, 10)
}

func (s *AdaptiveSelector) pacingDecision(ctx context.Context, a *Auth, cfg internalconfig.AccountSchedulingConfig, now time.Time) WarmupPacingDecision {
	if !cfg.WarmupTrafficPacing.Enabled || !pacingNativeClaude(a) || s.isMature(a, cfg, now) || !s.adaptiveEligible(a, cfg) {
		return WarmupPacingDecision{Allowed: true, Reason: "not-target"}
	}
	p := s.pacer.Load()
	if p == nil {
		return WarmupPacingDecision{Reason: "state-unavailable"}
	}
	req := WarmupPacingRequest{CountOnly: warmupCountPurpose(ctx), EstimateKnown: true}
	if ctx != nil {
		if identity, ok := ctx.Value(pacingSelectionKey{}).(WarmupPacingRequest); ok {
			req.GroupID, req.BindingID = identity.GroupID, identity.BindingID
		}
	}
	d, err := p.PeekConfigured(a.ID, req)
	if err != nil {
		return WarmupPacingDecision{Reason: "state-error"}
	}
	return d
}

func (s *AdaptiveSelector) pacingFilter(ctx context.Context, pool []adaptiveCandidate, cfg internalconfig.AccountSchedulingConfig, now time.Time) []adaptiveCandidate {
	if !cfg.WarmupTrafficPacing.Enabled {
		return pool
	}
	filtered := make([]adaptiveCandidate, 0, len(pool))
	for _, c := range pool {
		if s.pacingDecision(ctx, c.auth, cfg, now).Allowed && pacingQueueTurn(ctx, s, c.auth.ID, warmupBindingFromContext(ctx).key) {
			filtered = append(filtered, c)
		}
	}
	return filtered
}

func (s *AdaptiveSelector) admissionWaitFor(ctx context.Context, a *Auth, cfg internalconfig.AccountSchedulingConfig, now time.Time, key string, revision uint64) *warmupSelectionWait {
	if !cfg.WarmupTrafficPacing.Enabled || !pacingNativeClaude(a) || !s.adaptiveEligible(a, cfg) || s.isMature(a, cfg, now) {
		return nil
	}
	d := s.pacingDecision(ctx, a, cfg, now)
	if d.Allowed {
		switch {
		case s.gate.bindingInFlight(key):
			d.Reason = "binding_busy"
		case !s.hasConcurrencyHeadroom(a, cfg, now):
			d.Reason = "concurrency"
		case !pacingQueueTurn(ctx, s, a.ID, key):
			d.Reason = "queue_turn"
		default:
			return nil
		}
	}
	remaining := warmupWaitLimit
	if state := warmupRequestFromContext(ctx); state != nil {
		state.mu.Lock()
		if state.admission != nil && !state.admission.started.IsZero() {
			remaining = state.admission.deadline.Sub(state.now())
		} else if state.admission != nil {
			remaining = time.Duration(state.admission.config.MaxWaitSeconds) * time.Second
		}
		state.mu.Unlock()
	}
	if !pacingWaitEligible(d.Reason, d.RetryAfter, remaining) {
		return nil
	}
	w := &warmupSelectionWait{selector: s, key: key, authID: a.ID, revision: revision, epoch: s.servingEpoch, delay: d.RetryAfter, reason: d.Reason}
	w.ready = func() bool {
		current := s.scheduling()
		at := s.now()
		return s.pacingDecision(ctx, a, current, at).Allowed && s.hasConcurrencyHeadroom(a, current, at) && !s.gate.bindingInFlight(key)
	}
	return w
}

func attachPacingWaitReadiness(ctx context.Context, w *warmupSelectionWait, a *Auth) {
	if w == nil || w.selector == nil || a == nil {
		return
	}
	s := w.selector
	snapshot := a.Clone()
	identity, hasIdentity := ctx.Value(pacingSelectionKey{}).(WarmupPacingRequest)
	if !hasIdentity && w.key != "" {
		if entry, ok := s.servingEntry(w.key, s.now()); ok && entry.serving != nil {
			identity.GroupID = entry.serving.pacingGroup
			identity.BindingID = s.pacingMemberID(w.key, entry.serving.revision)
		}
	}
	probeCtx := context.WithValue(ctx, pacingSelectionKey{}, identity)
	w.ready = func() bool {
		cfg, now := s.scheduling(), s.now()
		return s.pacingDecision(probeCtx, snapshot, cfg, now).Allowed && s.hasConcurrencyHeadroom(snapshot, cfg, now) && !s.gate.bindingInFlight(w.key)
	}
}

func (s *AdaptiveSelector) availableAdmissionWait(ctx context.Context, available []*Auth, cfg internalconfig.AccountSchedulingConfig, now time.Time, key string) error {
	for _, a := range available {
		if wait := s.admissionWaitFor(ctx, a, cfg, now, key, 0); wait != nil {
			return wait
		}
	}
	return nil
}

func (s *AdaptiveSelector) pacingUnavailableError(ctx context.Context, available []*Auth, cfg internalconfig.AccountSchedulingConfig, now time.Time) error {
	reason := "unavailable"
	for _, a := range available {
		if !pacingNativeClaude(a) || s.isMature(a, cfg, now) {
			continue
		}
		d := s.pacingDecision(ctx, a, cfg, now)
		if !d.Allowed {
			reason = d.Reason
		} else if !s.hasConcurrencyHeadroom(a, cfg, now) {
			reason = "concurrency"
		} else {
			continue
		}
		if state := warmupRequestFromContext(ctx); state != nil {
			state.mu.Lock()
			state.proof = &warmupSelectionWait{selector: s, authID: a.ID, reason: reason, delay: d.RetryAfter}
			state.mu.Unlock()
		}
		break
	}
	return localAdmissionError(reason, ctx)
}

func (s *AdaptiveSelector) releasePacingBinding(key string, entry sessionEntry) {
	if entry.serving == nil || entry.serving.pacingGroup == "" {
		return
	}
	if p := s.pacer.Load(); p != nil {
		_ = p.ReleaseBinding(entry.authID, entry.serving.pacingGroup, s.pacingMemberID(key, entry.serving.revision))
	}
}

func (s *AdaptiveSelector) pickPacingMatureHandoff(ctx context.Context, available []*Auth, cfg internalconfig.AccountSchedulingConfig, now time.Time) (*Auth, string, error) {
	if picked, ok := s.pickFromCandidatesForRequest(ctx, s.scoreCandidates(available, cfg, now, true), cfg, now); ok {
		return picked, "pacing-handoff-mature", nil
	}
	return nil, "pacing-no-mature", s.pacingUnavailableError(ctx, available, cfg, now)
}
