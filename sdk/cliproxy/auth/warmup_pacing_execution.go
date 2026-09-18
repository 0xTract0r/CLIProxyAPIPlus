package auth

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
)

type pacingHTTPGate struct {
	manager       *Manager
	authID, model string
	fingerprint   [32]byte
	config        *internalconfig.Config
	opts          cliproxyexecutor.Options
	holder        *pacingOwnership
}

type pacingHTTPPermit struct {
	gate      *pacingHTTPGate
	pacer     *WarmupPacer
	lease     *WarmupPacingAttempt
	estimate  int64
	groupHash string
}

func pacingAuthFingerprint(a *Auth) [32]byte {
	credentials := map[string]any{}
	for _, key := range []string{"access_token", "accessToken", "refresh_token", "refreshToken", "id_token"} {
		credentials[key] = a.Metadata[key]
	}
	b, _ := json.Marshal([]any{a.Provider, a.ProxyURL, a.Attributes, credentials})
	return sha256.Sum256(b)
}

func pacingExecutionContext(ctx context.Context) context.Context {
	if _, ok := ctx.Value(pacingOwnershipKey{}).(*pacingOwnership); ok {
		return ctx
	}
	return context.WithValue(ctx, pacingOwnershipKey{}, &pacingOwnership{})
}

func (m *Manager) pacingSendContext(ctx context.Context, executor ProviderExecutor, a *Auth, route string, opts cliproxyexecutor.Options) (context.Context, error) {
	m.mu.RLock()
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	s, _ := m.selector.(*AdaptiveSelector)
	target := cfg != nil && !cfg.Home.Enabled && cfg.AccountScheduling.WarmupTrafficPacing.Enabled && s != nil && pacingNativeClaude(a) && !s.isMature(a, cfg.AccountScheduling, s.now()) && s.adaptiveEligible(a, cfg.AccountScheduling)
	m.mu.RUnlock()
	if !target {
		if holder, _ := ctx.Value(pacingOwnershipKey{}).(*pacingOwnership); holder != nil {
			m.pacingMu.Lock()
			holder.gated = false
			m.pacingMu.Unlock()
		}
		return ctx, nil
	}
	aware, ok := executor.(cliproxyexecutor.HTTPAttemptGateAware)
	if !ok || !aware.SupportsHTTPAttemptGate() {
		return ctx, &cliproxyexecutor.HTTPAttemptGateError{Cause: warmupReselect(ctx, true)}
	}
	ctx = pacingExecutionContext(ctx)
	holder, _ := ctx.Value(pacingOwnershipKey{}).(*pacingOwnership)
	return cliproxyexecutor.WithHTTPAttemptGate(ctx, &pacingHTTPGate{m, a.ID, route, pacingAuthFingerprint(a), cfg, opts, holder}), nil
}

func (g *pacingHTTPGate) current(ctx context.Context) (*AdaptiveSelector, *Auth, WarmupPacingLimits, error) {
	m := g.manager
	if err := ctx.Err(); err != nil {
		return nil, nil, WarmupPacingLimits{}, err
	}
	m.mu.RLock()
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	s, _ := m.selector.(*AdaptiveSelector)
	a := m.auths[g.authID]
	if a != nil {
		a = a.Clone()
	}
	m.mu.RUnlock()
	if s == nil || a == nil || a.Disabled || pacingAuthFingerprint(a) != g.fingerprint {
		return nil, nil, WarmupPacingLimits{}, warmupReselect(ctx, false)
	}
	if _, err := getAvailableAuths([]*Auth{a}, "claude", g.model, s.now()); err != nil {
		return nil, nil, WarmupPacingLimits{}, warmupReselect(ctx, true)
	}
	if !m.authSupportsRouteModel(registry.GetGlobalRegistry(), a, g.model) || !m.authAllowsClaudeContextRequest(a, g.model, g.opts) {
		return nil, nil, WarmupPacingLimits{}, warmupReselect(ctx, false)
	}
	if cfg != g.config && cfg.AccountScheduling.WarmupTrafficPacing.Enabled {
		return nil, nil, WarmupPacingLimits{}, warmupReselect(ctx, false)
	}
	l := pacingLimitsFor(s, a, cfg.AccountScheduling, s.now())
	l.Config.Enabled = l.Config.Enabled && !cfg.Home.Enabled
	return s, a, l, nil
}

func pacingWait(ctx context.Context, s *AdaptiveSelector, authID string, delay time.Duration) error {
	lease := warmupBindingFromContext(ctx)
	if matureOnly, _, _ := warmupSelectionPolicy(ctx); matureOnly {
		return warmupReselect(ctx, true)
	}
	return &warmupSelectionWait{selector: s, key: lease.key, authID: authID, revision: lease.revision, epoch: lease.epoch, delay: delay}
}

func (g *pacingHTTPGate) Before(ctx context.Context, info cliproxyexecutor.HTTPAttemptInfo) (cliproxyexecutor.HTTPAttemptPermit, error) {
	var permit *pacingHTTPPermit
	first := true
	err := runWarmupSelection(ctx, func(ctx context.Context) error {
		if !first {
			return warmupReselect(ctx, false)
		}
		first = false
		m := g.manager
		m.pacingMu.Lock()
		defer m.pacingMu.Unlock()
		m.refreshWarmupPacingLocked()
		s, a, limits, err := g.current(ctx)
		if err != nil {
			return err
		}
		if !limits.Config.Enabled {
			permit = &pacingHTTPPermit{gate: g}
			return nil
		}
		if m.pacing == nil || m.pacing.fault != nil || m.pacing.pacer == nil {
			return warmupReselect(ctx, true)
		}
		if info.Provider != "claude" || info.CountOnly != warmupCountPurpose(ctx) {
			return warmupReselect(ctx, true)
		}
		if m.pacingLegacyInFlightLocked(g.holder, s.gate, a.ID) {
			return pacingWait(ctx, s, a.ID, 100*time.Millisecond)
		}
		request := WarmupPacingRequest{CountOnly: info.CountOnly, EstimatedTokens: info.EstimatedTokens, EstimateKnown: info.EstimateKnown}
		binding := warmupBindingFromContext(ctx)
		if !info.CountOnly {
			request.GroupID, request.BindingID, err = s.pacingBindingIdentity(binding, a.ID)
			if err != nil {
				return warmupReselect(ctx, true)
			}
		}
		if slot := warmupExecutionSlotFromContext(ctx); slot == nil || slot.pacingRateUsed.Load() {
			rpm, burst := s.rateLimitParams(a, m.accountSchedulingConfig(), s.now())
			if allowed, delay := s.limiter.AllowOrDelay(a.ID, rpm, burst); !allowed {
				return pacingWait(ctx, s, a.ID, delay)
			}
		}
		lease, d, err := m.pacing.pacer.Reserve(a.ID, limits, request)
		if err != nil {
			return warmupReselect(ctx, true)
		}
		if !d.Allowed {
			pacingLogDecision(ctx, a.ID, "denied", d)
			if d.Reason == "rpm" || d.Reason == "concurrency" {
				return pacingWait(ctx, s, a.ID, d.RetryAfter)
			}
			return warmupReselect(ctx, true)
		}
		permit = &pacingHTTPPermit{gate: g, pacer: m.pacing.pacer, lease: lease, estimate: info.EstimatedTokens}
		if request.GroupID != "" {
			permit.groupHash = pacingDigest(request.GroupID)[:12]
		}
		return nil
	})
	return permit, err
}

func (p *pacingHTTPPermit) MarkSent(ctx context.Context) error {
	m := p.gate.manager
	m.pacingMu.Lock()
	defer m.pacingMu.Unlock()
	m.refreshWarmupPacingLocked()
	s, a, limits, err := p.gate.current(ctx)
	if err != nil {
		return err
	}
	if p.lease == nil && limits.Config.Enabled {
		return warmupReselect(ctx, false)
	}
	if p.lease != nil {
		if m.pacing == nil || m.pacing.pacer != p.pacer || m.pacing.fault != nil {
			return warmupReselect(ctx, true)
		}
		if !warmupCountPurpose(ctx) {
			if _, _, err = s.pacingBindingIdentity(warmupBindingFromContext(ctx), a.ID); err != nil {
				return warmupReselect(ctx, false)
			}
		}
		if !limits.Config.Enabled {
			if err = p.pacer.CancelUnsent(p.lease); err != nil {
				return err
			}
			p.lease = nil
		}
	}
	if p.lease != nil {
		if err = p.pacer.MarkSent(p.lease); err != nil {
			return warmupReselect(ctx, true)
		}
		if d, err := p.pacer.PeekConfigured(a.ID, WarmupPacingRequest{CountOnly: true, EstimateKnown: true}); err == nil {
			pacingLogDecision(ctx, a.ID, "sent", d)
		}
	}
	p.gate.holder.mu.Lock()
	p.gate.holder.latest = p.lease
	p.gate.holder.mu.Unlock()
	p.gate.holder.gated = p.lease != nil
	p.gate.holder.started = true
	if slot := warmupExecutionSlotFromContext(ctx); slot != nil {
		slot.pacingRateUsed.Store(true)
	}
	warmupMarkSent(ctx, warmupExecutionSlotFromContext(ctx))
	return nil
}

func (p *pacingHTTPPermit) CancelUnsent(context.Context) error {
	if p.lease == nil {
		return nil
	}
	return p.pacer.CancelUnsent(p.lease)
}

func (p *pacingHTTPPermit) Finish(ctx context.Context, result cliproxyexecutor.HTTPAttemptResult) error {
	if p.lease == nil {
		return nil
	}
	err := p.pacer.Finish(p.lease, result.Complete, result.SchedulerTokens)
	selectorLogEntry(ctx).WithFields(log.Fields{"auth_id": p.gate.authID, "group_hash": p.groupHash, "complete": result.Complete, "estimated_tokens": p.estimate, "observed_tokens": result.SchedulerTokens, "token_difference": result.SchedulerTokens - p.estimate, "persisted": err == nil}).Info("warmup-pacing-settled")
	if err != nil {
		log.WithField("auth_id", p.gate.authID).Warn("warmup pacing settlement failed; subsequent warming sends denied")
	}
	return err
}

func pacingLogDecision(ctx context.Context, authID, event string, d WarmupPacingDecision) {
	selectorLogEntry(ctx).WithFields(log.Fields{"auth_id": authID, "reason": d.Reason, "request_balance": d.Balance, "minute_attempts": d.MinuteRequests, "day_attempts": d.DayRequests, "in_flight": d.InFlight, "active_groups": d.ActiveGroups, "tokens": d.Tokens, "pending_tokens": d.PendingTokens, "retry_ms": d.RetryAfter.Milliseconds()}).Info("warmup-pacing-" + event)
}

// A real previous failure wins over a local reselect wrapped by the SDK. The
// conductor still records one logical result even after multiple HTTP attempts.
func pacingExecutionError(err error) error {
	var gate *cliproxyexecutor.HTTPAttemptGateError
	if errors.As(err, &gate) && gate.Previous != nil {
		return gate.Previous
	}
	return err
}

func pacingKeepPrevious(err, previous error) error {
	var gate *cliproxyexecutor.HTTPAttemptGateError
	if previous != nil && errors.As(err, &gate) {
		return previous
	}
	return pacingExecutionError(err)
}

func pacingLocalError(err error) (error, bool) {
	var gate *cliproxyexecutor.HTTPAttemptGateError
	if errors.As(err, &gate) && gate.Previous == nil && !gate.Sent {
		return gate.Cause, true
	}
	return nil, false
}

func (m *Manager) executePacingAttempt(ctx context.Context, executor ProviderExecutor, a *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, route string, count bool) (cliproxyexecutor.Response, error) {
	sendCtx, err := m.pacingSendContext(ctx, executor, a, route, opts)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	if err = warmupBeforeSend(sendCtx, warmupExecutionSlotFromContext(ctx)); err != nil {
		return cliproxyexecutor.Response{}, err
	}
	if count {
		resp, err := executor.CountTokens(sendCtx, a, req, opts)
		return resp, pacingExecutionError(err)
	}
	resp, err := executor.Execute(sendCtx, a, req, opts)
	return resp, pacingExecutionError(err)
}

func (s *AdaptiveSelector) pacingBindingIdentity(lease warmupBindingLease, authID string) (string, string, error) {
	s.servingMu.Lock()
	defer s.servingMu.Unlock()
	entry, ok := s.servingEntry(lease.key, s.now())
	if !ok || entry.serving == nil || !entry.serving.pacingReliable || lease.selector != s || lease.authID != authID || entry.authID != authID || lease.epoch != s.servingEpoch || lease.revision != entry.serving.revision {
		return "", "", errors.New("pacing binding changed")
	}
	group := entry.serving.pacingGroup
	if group == "" {
		group = lease.key
	}
	return group, s.pacingMemberID(lease.key, lease.revision), nil
}
