package auth

import (
	"context"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	log "github.com/sirupsen/logrus"
)

// Track the entire local native Claude executor lifetime across strategy changes.
// This observes pre-bootstrap streams without installing an HTTP hook or changing
// transport/redirect behavior. Accounting finishes before the call is removed.
func (m *Manager) pacingExecutionContext(ctx context.Context, authID string) context.Context {
	ctx = pacingExecutionContext(ctx)
	holder, _ := ctx.Value(pacingOwnershipKey{}).(*pacingOwnership)
	m.pacingMu.Lock()
	m.mu.RLock()
	a := m.auths[authID]
	selector, adaptive := m.selector.(*AdaptiveSelector)
	target := pacingNativeClaude(a)
	if cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config); cfg != nil && cfg.Home.Enabled {
		target = false
	}
	m.mu.RUnlock()
	if target {
		holder.manager, holder.authID, holder.slot = m, authID, warmupExecutionSlotFromContext(ctx)
		cfg := m.accountSchedulingConfig()
		holder.gated = adaptive && cfg.WarmupTrafficPacing.Enabled && !selector.isMature(a, cfg, selector.now())
		if m.pacingCalls == nil {
			m.pacingCalls = make(map[*pacingOwnership]struct{})
		}
		m.pacingCalls[holder] = struct{}{}
	}
	m.pacingMu.Unlock()
	return ctx
}

func (m *Manager) finishPacingResult(ctx context.Context) {
	if ctx != nil {
		if holder, _ := ctx.Value(pacingOwnershipKey{}).(*pacingOwnership); holder != nil && holder.stream {
			return
		}
	}
	m.finishPacingCall(ctx)
}

func (m *Manager) finishPacingCall(ctx context.Context) {
	if ctx == nil {
		return
	}
	holder, _ := ctx.Value(pacingOwnershipKey{}).(*pacingOwnership)
	if holder == nil || holder.manager != m {
		return
	}
	m.pacingMu.Lock()
	if _, active := m.pacingCalls[holder]; active && holder.started && !holder.gated && !warmupCountPurpose(ctx) && m.pacing != nil && m.pacing.pacer != nil && m.pacing.managed[holder.authID] {
		if _, err := m.pacing.pacer.RecordLegacy(holder.authID, WarmupPacingLegacyWindow{}, WarmupPacingLegacyEvent{UnknownTokens: true}); err != nil {
			log.WithField("auth_id", holder.authID).Warn("warmup pacing legacy terminal persistence failed")
		}
	}
	delete(m.pacingCalls, holder)
	m.pacingMu.Unlock()
}

// The old gate alone misses a legacy stream before its first chunk. Conversely,
// another owned paced call is already constrained by the atomic attempt ledger.
func (m *Manager) pacingLegacyInFlightLocked(own *pacingOwnership, gate *AccountConcurrencyGate, authID string) bool {
	ownedSlots := 0
	for call := range m.pacingCalls {
		if call.authID != authID {
			continue
		}
		if call != own && !call.gated {
			return true
		}
		if call.slot != nil && call.slot.target && call.slot.gate == gate {
			ownedSlots++
		}
	}
	return gate.InFlight(authID) > ownedSlots
}
