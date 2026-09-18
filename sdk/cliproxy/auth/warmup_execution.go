package auth

import (
	"context"
	"errors"
	"strings"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type warmupExecutionContextKey struct{}
type warmupAdmissionReselect struct{ previous error }

func (*warmupAdmissionReselect) Error() string { return "warming admission requires fresh selection" }

func isWarmupAdmissionReselect(err error) bool {
	err = pacingExecutionError(err)
	var local *warmupAdmissionReselect
	return errors.As(err, &local)
}

func warmupReselect(ctx context.Context, matureOnly bool) error {
	state := warmupRequestFromContext(ctx)
	if state == nil {
		return newWarmupBusyError()
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	state.suppressReserve = true
	state.matureOnly = state.matureOnly || matureOnly
	state.localReselects++
	if state.localReselects > 4 {
		return newWarmupBusyError()
	}
	return &warmupAdmissionReselect{}
}

func warmupExecutionSlotFromContext(ctx context.Context) *accountExecutionSlot {
	if ctx == nil {
		return nil
	}
	slot, _ := ctx.Value(warmupExecutionContextKey{}).(*accountExecutionSlot)
	return slot
}

func withWarmupExecutionSlot(ctx context.Context, slot *accountExecutionSlot) context.Context {
	return context.WithValue(ctx, warmupExecutionContextKey{}, slot)
}

func warmupSendSerial(ctx context.Context) uint64 {
	state := warmupRequestFromContext(ctx)
	if state == nil {
		return 0
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.sendSerial
}

func warmupBeforeSend(ctx context.Context, slot *accountExecutionSlot) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if cliproxyexecutor.HTTPAttemptGateFromContext(ctx) != nil {
		return nil
	}
	if holder, _ := ctx.Value(pacingOwnershipKey{}).(*pacingOwnership); holder != nil && holder.manager != nil {
		holder.manager.pacingMu.Lock()
		holder.started = true
		holder.manager.pacingMu.Unlock()
	}
	warmupMarkSent(ctx, slot)
	return nil
}

func warmupMarkSent(ctx context.Context, slot *accountExecutionSlot) {
	if slot != nil && slot.target {
		slot.sent.Store(true)
	}
	if state := warmupRequestFromContext(ctx); state != nil {
		state.mu.Lock()
		state.sendSerial++
		state.mu.Unlock()
	}
}

// A successful reservation owns both counters until its final result is stored.
// Rejection changes neither counter, unlike the legacy advisory Acquire method.
func (g *AccountConcurrencyGate) reserveWarmup(authID string, concurrency, budget int, seed []DailyWindowBucket) bool {
	hour := g.currentHour()
	g.mu.Lock()
	defer g.mu.Unlock()
	seedRollingLocked(g.daily, authID, seed)
	if (concurrency > 0 && g.inflight[authID] >= concurrency) || (budget > 0 && rollingCountLocked(g.daily, authID, hour)+g.pending[authID] >= budget) {
		return false
	}
	if g.pending == nil {
		g.pending = make(map[string]int)
	}
	g.inflight[authID]++
	g.pending[authID]++
	return true
}

func (g *AccountConcurrencyGate) releaseWarmupReservation(authID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.inflight[authID] <= 1 {
		delete(g.inflight, authID)
	} else {
		g.inflight[authID]--
	}
	if g.pending[authID] <= 1 {
		delete(g.pending, authID)
	} else {
		g.pending[authID]--
	}
}

func (s *AdaptiveSelector) pendingWarmupBudgetBusy(auth *Auth, cfg internalconfig.AccountSchedulingConfig, now time.Time) bool {
	status := AccountWarmupStatusFor(auth, now, cfg)
	budget := scaleLimitInt(status.DailyBudget, AccountRateScale(auth, cfg))
	if budget <= 0 {
		return false
	}
	g := s.gate
	hour := g.currentHour()
	g.mu.Lock()
	defer g.mu.Unlock()
	seedRollingLocked(g.daily, auth.ID, readDailyWindowBuckets(auth.Metadata, accountSchedulingDailyWindowKey))
	return g.pending[auth.ID] > 0 && rollingCountLocked(g.daily, auth.ID, hour)+g.pending[auth.ID] >= budget
}

func (m *Manager) admitWarmupExecution(ctx context.Context, auth *Auth, model string, opts cliproxyexecutor.Options) (*accountExecutionSlot, bool, error) {
	var slot *accountExecutionSlot
	target := false
	first := true
	err := runWarmupSelection(ctx, func(current context.Context) error {
		if !first {
			return warmupReselect(current, false)
		}
		first = false
		var err error
		slot, target, err = m.tryWarmupExecution(current, auth, model, opts)
		return err
	})
	return slot, target, err
}

func (m *Manager) tryWarmupExecution(ctx context.Context, auth *Auth, model string, options ...cliproxyexecutor.Options) (*accountExecutionSlot, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if auth == nil {
		return nil, false, nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	cfgRaw, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	selector, adaptive := m.selector.(*AdaptiveSelector)
	if !adaptive || selector == nil || cfgRaw == nil || cfgRaw.Home.Enabled || (cfgRaw.AccountScheduling.WarmupServingReserve <= 0 && !cfgRaw.AccountScheduling.WarmupTrafficPacing.Enabled) || !strings.EqualFold(auth.Provider, "claude") {
		return nil, false, nil
	}
	cfg := cfgRaw.AccountScheduling
	latest := m.auths[auth.ID]
	if latest == nil {
		return nil, true, warmupReselect(ctx, true)
	}
	now := time.Now()
	status := AccountWarmupStatusFor(latest, now, cfg)
	if status.Mature || !selector.adaptiveEligible(latest, cfg) {
		return nil, false, nil
	}
	var opts cliproxyexecutor.Options
	if len(options) > 0 {
		opts = options[0]
	}
	if (model != "" && !m.authSupportsRouteModel(registry.GetGlobalRegistry(), latest, model)) || !m.authAllowsClaudeContextRequest(latest, model, opts) {
		return nil, true, warmupReselect(ctx, false)
	}
	selector.servingMu.Lock()
	defer selector.servingMu.Unlock()
	if _, err := getAvailableAuths([]*Auth{latest}, "claude", model, now); err != nil {
		return nil, true, warmupReselect(ctx, true)
	}
	lease := warmupBindingFromContext(ctx)
	if lease.selector != nil && lease.authID == auth.ID {
		entry, ok := selector.servingEntry(lease.key, selector.now())
		if lease.selector != selector || lease.epoch != selector.servingEpoch || !ok || entry.serving == nil || entry.authID != auth.ID || entry.serving.revision != lease.revision {
			return nil, true, warmupReselect(ctx, false)
		}
	}
	matureOnly, _, _ := warmupSelectionPolicy(ctx)
	if selector.overWarmupBudget(latest, cfg, now) {
		return nil, true, warmupReselect(ctx, true)
	}
	limit := scaleLimitInt(status.ConcurrencyLimit, AccountRateScale(latest, cfg))
	budget := scaleLimitInt(status.DailyBudget, AccountRateScale(latest, cfg))
	wait := &warmupSelectionWait{selector: selector, key: lease.key, authID: auth.ID, revision: lease.revision, epoch: selector.servingEpoch, delay: 100 * time.Millisecond}
	if !selector.gate.reserveWarmup(auth.ID, limit, budget, readDailyWindowBuckets(latest.Metadata, accountSchedulingDailyWindowKey)) {
		if matureOnly {
			return nil, true, warmupReselect(ctx, true)
		}
		return nil, true, wait
	}
	slot := &accountExecutionSlot{gate: selector.gate, authID: auth.ID, target: true, countOnly: warmupCountPurpose(ctx), dailyBudget: budget, manager: m}
	if err := ctx.Err(); err != nil {
		slot.release()
		return nil, true, err
	}
	if !warmupTakeRateCharge(ctx, auth.ID) {
		rpm, burst := selector.rateLimitParams(latest, cfg, now)
		if allowed, delay := selector.limiter.AllowOrDelay(auth.ID, rpm, burst); !allowed {
			slot.release()
			if matureOnly {
				return nil, true, warmupReselect(ctx, true)
			}
			wait.delay = delay
			return nil, true, wait
		}
	}
	return slot, true, nil
}

func (slot *accountExecutionSlot) record(ctx context.Context, record func()) {
	if slot == nil || !slot.target {
		record()
		return
	}
	slot.resultOnce.Do(func() {
		if ctx.Err() != nil && slot.sent.Load() {
			slot.manager.recordWarmupCancellation(context.WithoutCancel(ctx), slot)
			return
		}
		record()
	})
}

func (slot *accountExecutionSlot) close(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	if holder, _ := ctx.Value(pacingOwnershipKey{}).(*pacingOwnership); holder != nil && holder.manager != nil {
		defer holder.manager.finishPacingCall(ctx)
	}
	if slot == nil {
		return
	}
	if slot.target {
		slot.resultOnce.Do(func() {
			if slot.sent.Load() && ctx.Err() != nil {
				slot.manager.recordWarmupCancellation(context.WithoutCancel(ctx), slot)
			}
		})
	}
	slot.release()
}

func (m *Manager) recordWarmupCancellation(ctx context.Context, slot *accountExecutionSlot) {
	defer m.finishPacingCall(ctx)
	txn := m.beginPacingHistory(slot.authID, slot)
	m.mu.Lock()
	if auth := m.auths[slot.authID]; auth != nil {
		m.recordWarmupSlotBudgetLocked(auth, slot)
		if txn == nil {
			_ = m.persist(ctx, auth)
		}
	}
	m.mu.Unlock()
	m.finishPacingHistory(ctx, txn)
	if txn != nil {
		m.persistCurrentPacingAuth(ctx, slot.authID)
	}
}

func (m *Manager) recordWarmupSlotBudgetLocked(auth *Auth, slot *accountExecutionSlot) {
	if slot.dailyBudget <= 0 {
		return
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	updated := slot.gate.RecordRequestWindow(auth.ID, readDailyWindowBuckets(auth.Metadata, accountSchedulingDailyWindowKey))
	setAccountSchedulingValue(auth.Metadata, accountSchedulingDailyWindowKey, dailyWindowToMetadata(updated))
}

// A one-shot continuation contains only IDs. Every resume goes through the
// original Manager picker, credential preparation and current model mapping.
type warmupModelResume struct{ authID, nextModel string }

func warmupSetModelResume(ctx context.Context, authID, model string) {
	state := warmupRequestFromContext(ctx)
	if state == nil {
		return
	}
	state.mu.Lock()
	state.modelResume = &warmupModelResume{authID: authID, nextModel: model}
	state.mu.Unlock()
}

func warmupCanResumeCredential(ctx context.Context, attempted map[string]struct{}) bool {
	state := warmupRequestFromContext(ctx)
	if state == nil {
		return false
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.modelResume == nil {
		return false
	}
	_, seen := attempted[state.modelResume.authID]
	return seen
}

func warmupResumeModels(ctx context.Context, authID string, models []string) ([]string, error) {
	state := warmupRequestFromContext(ctx)
	if state == nil {
		return models, nil
	}
	state.mu.Lock()
	resume := state.modelResume
	state.modelResume = nil
	state.mu.Unlock()
	if resume == nil || resume.authID != authID {
		return models, nil
	}
	for i, model := range models {
		if model == resume.nextModel {
			return models[i:], nil
		}
	}
	return nil, newWarmupBusyError()
}
