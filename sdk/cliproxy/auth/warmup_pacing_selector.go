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
		if s.pacingDecision(ctx, c.auth, cfg, now).Allowed {
			filtered = append(filtered, c)
		}
	}
	return filtered
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
	return nil, "pacing-no-mature", newWarmupBusyError()
}
